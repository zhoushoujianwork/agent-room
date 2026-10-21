package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testServer(t *testing.T, path string) (*server, http.Handler) {
	t.Helper()
	s, err := newServer(path, "https://board.example")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s, s.handler()
}

func request(t *testing.T, h http.Handler, method, path string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != status {
		t.Fatalf("%s %s: got %d expected %d: %s", method, path, w.Code, status, w.Body.String())
	}
	return w
}

func unpack[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func makeRoom(t *testing.T, h http.Handler, title, visibility string) room {
	t.Helper()
	return unpack[room](t, request(t, h, "POST", "/api/rooms", map[string]string{"title": title, "visibility": visibility}, 201))
}

type messagePage struct {
	Messages []message `json:"messages"`
	Next     int64     `json:"next_after"`
	More     bool      `json:"has_more"`
}
type roomPage struct {
	Rooms []room `json:"rooms"`
	Next  int64  `json:"next_after"`
	More  bool   `json:"has_more"`
}

func TestPublicDiscoveryPrivateDirectAndReadOnlyBoard(t *testing.T) {
	_, h := testServer(t, filepath.Join(t.TempDir(), "board.db"))
	private := makeRoom(t, h, "PRIVATE-DO-NOT-LIST", "")
	if private.Visibility != "private" || len(private.ID) != 32 {
		t.Fatalf("bad private default: %+v", private)
	}
	public := makeRoom(t, h, "Build investigation", "public")
	makeRoom(t, h, "Second public", "public")
	page := unpack[roomPage](t, request(t, h, "GET", "/api/rooms?limit=1", nil, 200))
	if len(page.Rooms) != 1 || page.Rooms[0].ID != public.ID || !page.More {
		t.Fatalf("bad directory: %+v", page)
	}
	newRoom := makeRoom(t, h, "Created during pagination", "public")
	page2 := unpack[roomPage](t, request(t, h, "GET", fmt.Sprintf("/api/rooms?after=%d", page.Next), nil, 200))
	if len(page2.Rooms) != 2 || page2.Rooms[1].ID != newRoom.ID {
		t.Fatalf("unstable directory: %+v", page2)
	}
	if strings.Contains(request(t, h, "GET", "/api/rooms", nil, 200).Body.String(), private.Title) {
		t.Fatal("private metadata in public directory")
	}
	request(t, h, "GET", "/api/rooms/"+private.ID, nil, 200)
	if private.URL != "https://board.example/rooms/"+private.ID {
		t.Fatal("configured base URL not used")
	}
	request(t, h, "GET", "/rooms/"+private.ID, nil, 200)
	request(t, h, "GET", "/api/rooms/missing/messages", nil, 404)
	request(t, h, "GET", "/api/rooms/missing/summary", nil, 404)
	request(t, h, "GET", "/rooms/missing", nil, 404)
	request(t, h, "POST", "/api/rooms/missing/messages", post{Sender: "a", Content: "x", ClientMessageID: "x"}, 404)
	pageHTML := request(t, h, "GET", "/", nil, 200).Body.String()
	for _, forbidden := range []string{"<textarea", "<form", "contenteditable", "Create room", "Send message"} {
		if strings.Contains(pageHTML, forbidden) {
			t.Fatalf("write control: %s", forbidden)
		}
	}
	js := request(t, h, "GET", "/app.js", nil, 200).Body.String()
	if strings.Contains(js, "innerHTML") || strings.Contains(js, "method:") {
		t.Fatal("board contains unsafe rendering or write request")
	}
	request(t, h, "POST", "/", nil, 405)
	request(t, h, "GET", "/healthz", nil, 200)
	guide := request(t, h, "GET", "/api/agent-guide", nil, 200).Body.String()
	if !strings.Contains(guide, "https://board.example") || !strings.Contains(guide, "covers_through_seq") {
		t.Fatal("guide incomplete")
	}
}

func TestConcurrentMessagesRetryConflictAndCatchup(t *testing.T) {
	_, h := testServer(t, filepath.Join(t.TempDir(), "board.db"))
	r := makeRoom(t, h, "Concurrent coordination", "public")
	const count = 40
	var wg sync.WaitGroup
	errs := make(chan string, count*2)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := post{Sender: fmt.Sprintf("agent-%d", i%2), Content: fmt.Sprintf("result-%d", i), ClientMessageID: fmt.Sprintf("message-%d", i)}
			b, _ := json.Marshal(p)
			for attempt := 0; attempt < 2; attempt++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("POST", "/api/rooms/"+r.ID+"/messages", bytes.NewReader(b)))
				if (attempt == 0 && w.Code != 201) || (attempt == 1 && w.Code != 200) {
					errs <- fmt.Sprintf("%d attempt %d: %d %s", i, attempt, w.Code, w.Body.String())
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	var after int64
	var seen []message
	for {
		page := unpack[messagePage](t, request(t, h, "GET", fmt.Sprintf("/api/rooms/%s/messages?after=%d&limit=7", r.ID, after), nil, 200))
		seen = append(seen, page.Messages...)
		after = page.Next
		if !page.More {
			break
		}
	}
	if len(seen) != count {
		t.Fatalf("got %d messages want %d", len(seen), count)
	}
	for i, m := range seen {
		if m.Seq != int64(i+1) {
			t.Fatalf("sequence gap at %d: %d", i, m.Seq)
		}
	}
	empty := unpack[messagePage](t, request(t, h, "GET", fmt.Sprintf("/api/rooms/%s/messages?after=%d", r.ID, after), nil, 200))
	if empty.Next != after || len(empty.Messages) != 0 {
		t.Fatal("empty read moved cursor")
	}
	request(t, h, "POST", "/api/rooms/"+r.ID+"/messages", post{Sender: "other", Content: "changed", ClientMessageID: "message-0"}, 409)
	request(t, h, "POST", "/api/rooms/"+r.ID+"/messages", post{Sender: "reviewer", Content: "verified", ReplyTo: seen[0].ID, To: "agent-0", ClientMessageID: "reply"}, 201)
	other := makeRoom(t, h, "Other room", "private")
	request(t, h, "POST", "/api/rooms/"+other.ID+"/messages", post{Sender: "other", Content: "cross-room reply", ReplyTo: seen[0].ID, ClientMessageID: "x"}, 404)
	page := unpack[messagePage](t, request(t, h, "GET", "/api/rooms/"+other.ID+"/messages", nil, 200))
	if len(page.Messages) != 0 {
		t.Fatal("room isolation failed")
	}
}

func TestSummaryCoverageAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	s, h := testServer(t, path)
	r := makeRoom(t, h, "Summary handoff", "public")
	endpoint := "/api/rooms/" + r.ID
	initial := request(t, h, "GET", endpoint+"/summary", nil, 200)
	if !strings.Contains(initial.Body.String(), `"summary":null`) {
		t.Fatal("expected absent summary")
	}
	zero := int64(0)
	request(t, h, "POST", endpoint+"/messages", post{Sender: "a", Kind: "summary", Content: "Goal: investigate", Covers: &zero, ClientMessageID: "initial"}, 201)
	work := unpack[message](t, request(t, h, "POST", endpoint+"/messages", post{Sender: "b", Content: "I found a cause", ClientMessageID: "work"}, 201))
	// Another message arrives while a summary of 'work' is being prepared.
	late := unpack[message](t, request(t, h, "POST", endpoint+"/messages", post{Sender: "b", Content: "Concurrent finding", ClientMessageID: "late"}, 201))
	latest := unpack[message](t, request(t, h, "POST", endpoint+"/messages", post{Sender: "a", Kind: "summary", Content: "Found cause; next: verify", Covers: &work.Seq, ClientMessageID: "summary"}, 201))
	request(t, h, "POST", endpoint+"/messages", post{Sender: "b", Kind: "summary", Content: "Stale coverage", Covers: &zero, ClientMessageID: "stale"}, 201)
	get := func(h http.Handler) message {
		var result struct {
			Summary message `json:"summary"`
		}
		result = unpack[struct {
			Summary message `json:"summary"`
		}](t, request(t, h, "GET", endpoint+"/summary", nil, 200))
		return result.Summary
	}
	if got := get(h); got.ID != latest.ID {
		t.Fatalf("summary regressed: %+v", got)
	}
	page := unpack[messagePage](t, request(t, h, "GET", fmt.Sprintf("%s/messages?after=%d", endpoint, work.Seq), nil, 200))
	if page.Messages[0].ID != late.ID {
		t.Fatal("summary catchup skipped concurrent message")
	}
	tie := unpack[message](t, request(t, h, "POST", endpoint+"/messages", post{Sender: "b", Kind: "summary", Content: "Refined at same coverage", Covers: &work.Seq, ClientMessageID: "tie"}, 201))
	if get(h).ID != tie.ID {
		t.Fatal("same coverage must prefer latest seq")
	}
	s.db.Close()
	_, h2 := testServer(t, path)
	if get(h2).ID != tie.ID {
		t.Fatal("summary did not survive restart")
	}
	info := unpack[room](t, request(t, h2, "GET", endpoint, nil, 200))
	if info.LatestSeq != tie.Seq {
		t.Fatal("sequence did not survive restart")
	}
	next := unpack[message](t, request(t, h2, "POST", endpoint+"/messages", post{Sender: "a", Content: "After restart", ClientMessageID: "restart"}, 201))
	if next.Seq != tie.Seq+1 {
		t.Fatal("sequence reset")
	}
	request(t, h2, "POST", endpoint+"/messages", post{Sender: "b", Content: "I found a cause", ClientMessageID: "work"}, 200)
	public := unpack[roomPage](t, request(t, h2, "GET", "/api/rooms", nil, 200))
	if public.Rooms[0].Summary.ID != tie.ID {
		t.Fatal("directory summary missing")
	}
}

func TestInvalidInput(t *testing.T) {
	_, h := testServer(t, filepath.Join(t.TempDir(), "board.db"))
	r := makeRoom(t, h, "Limits", "private")
	request(t, h, "POST", "/api/rooms", map[string]string{"title": " ", "visibility": "public"}, 400)
	request(t, h, "POST", "/api/rooms", map[string]string{"title": "x", "visibility": "secret"}, 400)
	request(t, h, "POST", "/api/rooms", map[string]string{"title": strings.Repeat("x", 201)}, 400)
	for _, query := range []string{"after=-1", "after=x", "after=", "limit=0", "limit=201", "limit=x", "limit="} {
		request(t, h, "GET", "/api/rooms?"+query, nil, 400)
		request(t, h, "GET", "/api/rooms/"+r.ID+"/messages?"+query, nil, 400)
	}
	negative := int64(-1)
	future := int64(1)
	zero := int64(0)
	for _, p := range []post{
		{Sender: "a", Content: "x"},
		{Sender: "", Content: "x", ClientMessageID: "id"},
		{Sender: "a", Content: strings.Repeat("x", 32769), ClientMessageID: "id"},
		{Sender: "a", Content: "x", ClientMessageID: "id", Kind: "task"},
		{Sender: "a", Content: "x", ClientMessageID: "id", Kind: "summary"},
		{Sender: "a", Content: "x", ClientMessageID: "id", Kind: "summary", Covers: &negative},
		{Sender: "a", Content: "x", ClientMessageID: "id", Kind: "summary", Covers: &future},
		{Sender: "a", Content: "x", ClientMessageID: "id", Covers: &zero},
	} {
		request(t, h, "POST", "/api/rooms/"+r.ID+"/messages", p, 400)
	}
	for _, raw := range []string{`{"title":"x","unknown":true}`, `{"title":"x"} {}`, `null`, `{"title":"` + strings.Repeat("x", 42000) + `"}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/api/rooms", strings.NewReader(raw)))
		if w.Code != 400 {
			t.Fatalf("invalid request returned %d", w.Code)
		}
	}
	request(t, h, "PUT", "/api/rooms/"+r.ID, nil, 405)
	request(t, h, "DELETE", "/api/rooms/"+r.ID, nil, 405)
}
