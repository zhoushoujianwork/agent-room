package relay

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-room/internal/config"
	"agent-room/internal/io/memory"
	"agent-room/internal/models"
	"agent-room/internal/service/chat"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestGetSummaryEmptyWhenUnconfigured(t *testing.T) {
	store := memory.NewStore()
	s := NewServer(config.Config{}, chat.NewService(store), store, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/r1/summary", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got models.RoomSummary
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Summary != "" {
		t.Fatalf("summary = %q, want empty", got.Summary)
	}
}

func TestGetSummaryReturnsStored(t *testing.T) {
	store := memory.NewStore()
	_ = store.UpsertSummary(context.Background(), models.RoomSummary{RoomID: "r1", Summary: "digest here", CoveredSeq: 7})
	s := NewServer(config.Config{}, chat.NewService(store), store, discardLogger())
	s.WithSummary(store, nil) // store wired, summarizer off

	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/r1/summary", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)

	var got models.RoomSummary
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got.Summary != "digest here" || got.CoveredSeq != 7 {
		t.Fatalf("got %#v", got)
	}
}

func TestDeleteMessagesClearsHistoryAndSummary(t *testing.T) {
	store := memory.NewStore()
	svc := chat.NewService(store)
	ctx := context.Background()
	owner := "alice"
	if err := store.CreateRoom(ctx, models.Room{
		ID:         "r1",
		OwnerLogin: &owner,
	}); err != nil {
		t.Fatalf("create room: %v", err)
	}
	if _, err := svc.Add(ctx, "r1", models.ChatMessage{ID: "a", Type: models.MessageTypeChat, Content: "hello"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.UpsertSummary(ctx, models.RoomSummary{RoomID: "r1", Summary: "digest here", CoveredSeq: 7}); err != nil {
		t.Fatalf("summary: %v", err)
	}

	secret := "0123456789abcdef0123456789abcdef"
	cfg := config.Config{GitHub: config.GitHubConfig{
		ClientID: "id", ClientSecret: "sec", SessionSecret: secret, CookieName: "agent_room_session",
	}}
	s := NewServer(cfg, svc, store, discardLogger())
	s.WithSummary(store, nil)

	req := httptest.NewRequest(http.MethodDelete, "/v1/rooms/r1/messages", nil)
	req.AddCookie(signedCookie(t, secret, owner))
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE messages = %d; body=%s", rr.Code, rr.Body.String())
	}
	msgs, err := store.List(ctx, "r1", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("messages len = %d, want 0", len(msgs))
	}
	sum, err := store.GetSummary(ctx, "r1")
	if err != nil {
		t.Fatalf("get summary: %v", err)
	}
	if sum.Summary != "" || sum.CoveredSeq != 0 {
		t.Fatalf("summary after delete = %#v, want empty", sum)
	}
}

func TestListMessagesSearchFilters(t *testing.T) {
	store := memory.NewStore()
	svc := chat.NewService(store)
	ctx := context.Background()
	for _, m := range []models.ChatMessage{
		{ID: "a", RoomID: "r1", Type: models.MessageTypeChat, Content: "看下出口IP"},
		{ID: "b", RoomID: "r1", Type: models.MessageTypeTrace, Content: "thinking IP"},
		{ID: "c", RoomID: "r1", Type: models.MessageTypeChat, Content: "放通8080"},
	} {
		if _, err := svc.Add(ctx, "r1", m); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	s := NewServer(config.Config{}, svc, store, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/v1/rooms/r1/messages?q=IP&type=chat", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var got []models.ChatMessage
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("q=IP&type=chat got %d messages: %#v", len(got), got)
	}
}
