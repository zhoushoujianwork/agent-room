package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-room/internal/config"
	"agent-room/internal/io/memory"
	"agent-room/internal/models"
	"agent-room/internal/service/chat"
)

func TestRootServesSPA(t *testing.T) {
	store := memory.NewStore()
	s := NewServer(config.Config{}, chat.NewService(store), store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", rr.Header().Get("Content-Type"))
	}
}

func TestRootServesBannerAssets(t *testing.T) {
	store := memory.NewStore()
	s := NewServer(config.Config{}, chat.NewService(store), store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/banners/it-windows-agent-support.png", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET banner status = %d, want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("Content-Type = %q, want image/png", rr.Header().Get("Content-Type"))
	}
	if rr.Body.Len() == 0 {
		t.Fatal("banner body is empty")
	}
}

func TestListMessagesPagesBackwardsWithBefore(t *testing.T) {
	store := memory.NewStore()
	s := NewServer(config.Config{}, chat.NewService(store), store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// Seed five chat messages in order m1..m5.
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		if err := store.Append(ctx, models.ChatMessage{
			ID: id, RoomID: "r", Type: models.MessageTypeChat,
			SenderID: "u", SenderKind: models.SenderKindUser, Content: id,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	get := func(query string) []models.ChatMessage {
		req := httptest.NewRequest(http.MethodGet, "/v1/rooms/r/messages?"+query, nil)
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET messages?%s = %d; body=%s", query, rr.Code, rr.Body.String())
		}
		var out []models.ChatMessage
		if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// before=m4 with limit=2 returns the two messages just older than m4,
	// oldest-first: m2, m3.
	page := get("before=m4&limit=2")
	if len(page) != 2 || page[0].ID != "m2" || page[1].ID != "m3" {
		t.Fatalf("before=m4&limit=2 unexpected: %#v", idsOf(page))
	}

	// Paging again before the oldest of that page (m2) returns m1, and that's
	// the top of the room.
	page = get("before=m2&limit=2")
	if len(page) != 1 || page[0].ID != "m1" {
		t.Fatalf("before=m2 unexpected: %#v", idsOf(page))
	}
}

func idsOf(msgs []models.ChatMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

func TestCreateRoomGeneratesBackendRoomID(t *testing.T) {
	store := memory.NewStore()
	s := NewServer(config.Config{}, chat.NewService(store), store, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /v1/rooms status = %d, want %d; body=%s", rr.Code, http.StatusCreated, rr.Body.String())
	}
	var got models.Room
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	id := got.ID
	// No prefix, just unguessable hex — this whole app is rooms.
	if len(id) < 24 {
		t.Fatalf("room_id = %q, want >= 24 chars of random hex", id)
	}
	for _, r := range id {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			t.Fatalf("room_id = %q contains non-hex/dash char %q", id, r)
		}
	}
	if got.OwnerLogin != nil {
		t.Fatalf("owner = %q, want nil for anonymous create", *got.OwnerLogin)
	}
	if got.Gated || got.Ended {
		t.Fatalf("expected gated=false ended=false, got gated=%v ended=%v", got.Gated, got.Ended)
	}
	if got.CreatedAt.IsZero() {
		t.Fatalf("created_at zero")
	}
}

func TestParticipantsAggregatesUserConnections(t *testing.T) {
	h := newHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()

	userOne := &client{roomID: "demo", participant: models.Participant{
		ID:           "mikas",
		RoomID:       "demo",
		Kind:         models.SenderKindUser,
		Label:        "Mikas",
		ConnectionID: "tab-one",
		ConnectedAt:  now.Add(-2 * time.Minute),
		LastSeenAt:   now.Add(-20 * time.Second),
		Metadata: map[string]string{
			"principal_email": "mikas@example.com",
		},
	}}
	userTwo := &client{roomID: "demo", participant: models.Participant{
		ID:           "mikas",
		RoomID:       "demo",
		Kind:         models.SenderKindUser,
		Label:        "Mikas",
		ConnectionID: "tab-two",
		ConnectedAt:  now.Add(-1 * time.Minute),
		LastSeenAt:   now,
		Metadata: map[string]string{
			"principal_email": "mikas@example.com",
		},
	}}
	agent := &client{roomID: "demo", participant: models.Participant{
		ID:           "alice",
		RoomID:       "demo",
		Kind:         models.SenderKindAgent,
		Label:        "Alice",
		ConnectionID: "agent-one",
		ConnectedAt:  now.Add(-3 * time.Minute),
		LastSeenAt:   now.Add(-10 * time.Second),
		Metadata: map[string]string{
			"provider": "claude",
		},
	}}

	h.rooms["demo"] = map[*client]struct{}{userOne: {}, userTwo: {}, agent: {}}

	got := h.Participants("demo")
	if len(got) != 2 {
		t.Fatalf("len(Participants) = %d, want 2: %#v", len(got), got)
	}
	if got[0].ID != "alice" || got[0].Kind != models.SenderKindAgent {
		t.Fatalf("first participant = %#v, want agent alice first", got[0])
	}
	user := got[1]
	if user.ID != "mikas" || user.Kind != models.SenderKindUser {
		t.Fatalf("user participant = %#v, want mikas user", user)
	}
	if user.ConnectionCount != 2 {
		t.Fatalf("ConnectionCount = %d, want 2", user.ConnectionCount)
	}
	if len(user.Connections) != 2 {
		t.Fatalf("len(Connections) = %d, want 2", len(user.Connections))
	}
	if user.Connections[0].ID != "tab-one" || user.Connections[1].ID != "tab-two" {
		t.Fatalf("Connections = %#v, want sorted tab-one, tab-two", user.Connections)
	}
	if user.LastSeenAt != now {
		t.Fatalf("LastSeenAt = %s, want %s", user.LastSeenAt, now)
	}
}

func TestAuditConnectionsAreHiddenFromParticipantsAndPresence(t *testing.T) {
	h := newHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now().UTC()

	auditAdmin := &client{roomID: "demo", audit: true, participant: models.Participant{
		ID:           "root",
		RoomID:       "demo",
		Kind:         models.SenderKindUser,
		Label:        "Root",
		ConnectionID: "admin-audit",
		ConnectedAt:  now.Add(-1 * time.Minute),
		LastSeenAt:   now,
	}}
	visibleUser := &client{roomID: "demo", participant: models.Participant{
		ID:           "alice",
		RoomID:       "demo",
		Kind:         models.SenderKindUser,
		Label:        "Alice",
		ConnectionID: "alice-tab",
		ConnectedAt:  now.Add(-2 * time.Minute),
		LastSeenAt:   now,
	}}
	h.rooms["demo"] = map[*client]struct{}{auditAdmin: {}, visibleUser: {}}

	participants := h.Participants("demo")
	if len(participants) != 1 || participants[0].ID != "alice" {
		t.Fatalf("participants = %#v, want only alice", participants)
	}
	presence := h.UserPresence()
	if _, ok := presence["root"]; ok {
		t.Fatalf("audit admin leaked into user presence: %#v", presence["root"])
	}
	if presence["alice"].ConnectionCount != 1 {
		t.Fatalf("alice presence = %#v, want one connection", presence["alice"])
	}
}

// TestPublishStampsExecTokenFromPresence verifies the relay-mediated model:
// the executor reports its token via presence (recorded privately), a later
// targeted command carries no token from the sender, yet the relay stamps the
// recorded token onto the copy delivered to that executor and nowhere else.
func TestPublishStampsExecTokenFromPresence(t *testing.T) {
	store := memory.NewStore()
	service := chat.NewService(store)
	h := newHub(service, slog.New(slog.NewTextHandler(io.Discard, nil)))

	target := newFakeClient("mac-mini", models.SenderKindAgent)
	observer := newFakeClient("alice", models.SenderKindUser)
	h.rooms["demo"] = map[*client]struct{}{target: {}, observer: {}}

	// Executor announces itself and its token via presence.
	h.updateParticipant(target, models.ChatMessage{
		Type:       models.MessageTypePresence,
		SenderID:   "mac-mini",
		SenderKind: models.SenderKindAgent,
		Metadata:   map[string]string{"mode": "executor", "exec_token": "super-secret-token"},
	})

	// Agent dispatches a command WITHOUT any token; the relay supplies it.
	cmd := models.ChatMessage{
		Type:       models.MessageTypeCommand,
		SenderID:   "operator",
		SenderKind: models.SenderKindAgent,
		TargetID:   "mac-mini",
		Content:    "uname -a",
		Metadata:   map[string]string{"operation": "exec", "timeout_ms": "5000"},
	}
	saved, err := h.Publish(context.Background(), "demo", cmd)
	if err != nil {
		t.Fatalf("Publish error: %v", err)
	}

	if got := saved.Metadata["exec_token"]; got != "" {
		t.Errorf("Publish return leaked exec_token = %q", got)
	}
	if got := saved.Metadata["operation"]; got != "exec" {
		t.Errorf("Publish return dropped non-sensitive metadata: operation = %q", got)
	}

	stored, err := service.List(context.Background(), "demo", 10)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("len(List) = %d, want 1", len(stored))
	}
	if got := stored[0].Metadata["exec_token"]; got != "" {
		t.Errorf("history leaked exec_token = %q", got)
	}

	targetMsg := drain(t, target)
	if got := targetMsg.Metadata["exec_token"]; got != "super-secret-token" {
		t.Errorf("target client missing relay-stamped exec_token: got %q", got)
	}
	observerMsg := drain(t, observer)
	if got := observerMsg.Metadata["exec_token"]; got != "" {
		t.Errorf("observer client received exec_token = %q", got)
	}
	if got := observerMsg.Metadata["operation"]; got != "exec" {
		t.Errorf("observer client missing non-sensitive metadata: operation = %q", got)
	}
}

func TestPublishInjectsReplyContextIntoTargetAgentCopy(t *testing.T) {
	store := memory.NewStore()
	service := chat.NewService(store)
	h := newHub(service, slog.New(slog.NewTextHandler(io.Discard, nil)))

	target := newFakeClient("agent-a", models.SenderKindAgent)
	observer := newFakeClient("alice", models.SenderKindUser)
	h.rooms["demo"] = map[*client]struct{}{target: {}, observer: {}}

	msg := models.ChatMessage{
		Type:       models.MessageTypeChat,
		SenderID:   "alice",
		SenderKind: models.SenderKindUser,
		TargetID:   "agent-a",
		Content:    "@agent-a 这个说了啥",
		Metadata: map[string]string{
			"reply_to":      "msg-quoted",
			"reply_sender":  "agent-a",
			"reply_preview": "找到了。结论很明确：patent-translation JM 现状...",
			"reply_excerpt": "找到了。结论很明确：JM pod restartCount=0，job 失败来自 TM Spot 回收，不是 JM 自身问题。",
		},
	}
	saved, err := h.Publish(context.Background(), "demo", msg)
	if err != nil {
		t.Fatalf("Publish error: %v", err)
	}
	if saved.Content != msg.Content {
		t.Fatalf("saved.Content changed = %q, want %q", saved.Content, msg.Content)
	}
	if saved.Metadata["reply_context_injected"] != "" {
		t.Fatalf("saved metadata leaked injection marker: %#v", saved.Metadata)
	}

	stored, err := service.List(context.Background(), "demo", 10)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("len(List) = %d, want 1", len(stored))
	}
	if stored[0].Content != msg.Content {
		t.Fatalf("stored.Content changed = %q, want %q", stored[0].Content, msg.Content)
	}

	targetMsg := drain(t, target)
	for _, want := range []string{
		"Quoted message context for this reply",
		"reply_to: msg-quoted",
		"quoted_sender: agent-a",
		"JM pod restartCount=0",
		"Incoming reply message:",
		"@agent-a 这个说了啥",
	} {
		if !strings.Contains(targetMsg.Content, want) {
			t.Fatalf("target message missing %q:\n%s", want, targetMsg.Content)
		}
	}
	if got := targetMsg.Metadata["reply_context_injected"]; got != "content" {
		t.Fatalf("target metadata reply_context_injected = %q, want content", got)
	}

	observerMsg := drain(t, observer)
	if observerMsg.Content != msg.Content {
		t.Fatalf("observer.Content = %q, want original %q", observerMsg.Content, msg.Content)
	}
	if observerMsg.Metadata["reply_context_injected"] != "" {
		t.Fatalf("observer received injection marker: %#v", observerMsg.Metadata)
	}
}

// TestPublishStripsClientSuppliedExecToken verifies a client can never inject
// a token: even if the sender includes one, it is dropped and (absent a
// recorded executor token) never delivered to anyone.
func TestPublishStripsClientSuppliedExecToken(t *testing.T) {
	store := memory.NewStore()
	service := chat.NewService(store)
	h := newHub(service, slog.New(slog.NewTextHandler(io.Discard, nil)))

	target := newFakeClient("mac-mini", models.SenderKindAgent)
	h.rooms["demo"] = map[*client]struct{}{target: {}}

	cmd := models.ChatMessage{
		Type:       models.MessageTypeCommand,
		SenderID:   "attacker",
		SenderKind: models.SenderKindAgent,
		TargetID:   "mac-mini",
		Content:    "id",
		Metadata:   map[string]string{"operation": "exec", "exec_token": "forged-token"},
	}
	if _, err := h.Publish(context.Background(), "demo", cmd); err != nil {
		t.Fatalf("Publish error: %v", err)
	}

	targetMsg := drain(t, target)
	if got := targetMsg.Metadata["exec_token"]; got != "" {
		t.Errorf("forged exec_token reached target = %q, want empty", got)
	}
}

func TestPublishRetargetsAgentPeerMentionFromOldBridge(t *testing.T) {
	store := memory.NewStore()
	service := chat.NewService(store)
	h := newHub(service, slog.New(slog.NewTextHandler(io.Discard, nil)))

	alice := newFakeClient("agent-alice", models.SenderKindAgent)
	bob := newFakeClient("agent-bob", models.SenderKindAgent)
	user := newFakeClient("operator", models.SenderKindUser)
	h.rooms["demo"] = map[*client]struct{}{alice: {}, bob: {}, user: {}}

	msg := models.ChatMessage{
		Type:           models.MessageTypeChat,
		SenderID:       "agent-alice",
		SenderKind:     models.SenderKindAgent,
		TargetID:       "operator",
		Content:        "你好 agent-bob，请介绍一下你的运行环境",
		ReplyRequested: true,
		TurnBudget:     12,
	}
	saved, err := h.Publish(context.Background(), "demo", msg)
	if err != nil {
		t.Fatalf("Publish error: %v", err)
	}
	if saved.TargetID != "agent-bob" {
		t.Fatalf("saved.TargetID = %q, want agent-bob", saved.TargetID)
	}
	if !saved.ReplyRequested {
		t.Fatalf("saved.ReplyRequested = false, want true for initial retarget")
	}
	if got := drain(t, bob); got.TargetID != "agent-bob" {
		t.Fatalf("bob got TargetID = %q, want agent-bob", got.TargetID)
	}
}

func TestPublishSuppressesAgentPeerAckWithoutExplicitMention(t *testing.T) {
	store := memory.NewStore()
	service := chat.NewService(store)
	h := newHub(service, slog.New(slog.NewTextHandler(io.Discard, nil)))

	alice := newFakeClient("agent-alice", models.SenderKindAgent)
	bob := newFakeClient("agent-bob", models.SenderKindAgent)
	h.rooms["demo"] = map[*client]struct{}{alice: {}, bob: {}}

	msg := models.ChatMessage{
		Type:           models.MessageTypeChat,
		SenderID:       "agent-alice",
		SenderKind:     models.SenderKindAgent,
		TargetID:       "agent-bob",
		Content:        "收到，待命中。有活招呼。",
		ReplyRequested: true,
		TurnBudget:     12,
	}
	saved, err := h.Publish(context.Background(), "demo", msg)
	if err != nil {
		t.Fatalf("Publish error: %v", err)
	}
	if saved.TargetID != "agent-bob" {
		t.Fatalf("saved.TargetID = %q, want agent-bob", saved.TargetID)
	}
	if saved.ReplyRequested {
		t.Fatalf("saved.ReplyRequested = true, want false")
	}
}

func TestPublishKeepsAgentPeerReplyWhenTargetExplicitlyMentioned(t *testing.T) {
	store := memory.NewStore()
	service := chat.NewService(store)
	h := newHub(service, slog.New(slog.NewTextHandler(io.Discard, nil)))

	alice := newFakeClient("agent-alice", models.SenderKindAgent)
	bob := newFakeClient("agent-bob", models.SenderKindAgent)
	h.rooms["demo"] = map[*client]struct{}{alice: {}, bob: {}}

	msg := models.ChatMessage{
		Type:           models.MessageTypeChat,
		SenderID:       "agent-alice",
		SenderKind:     models.SenderKindAgent,
		TargetID:       "agent-bob",
		Content:        "@agent-bob 继续确认 Linux 侧结果",
		ReplyRequested: true,
		TurnBudget:     12,
	}
	saved, err := h.Publish(context.Background(), "demo", msg)
	if err != nil {
		t.Fatalf("Publish error: %v", err)
	}
	if !saved.ReplyRequested {
		t.Fatalf("saved.ReplyRequested = false, want true")
	}
}

func TestPublishDoesNotRetargetExecutorPeerMentionAsChat(t *testing.T) {
	store := memory.NewStore()
	service := chat.NewService(store)
	h := newHub(service, slog.New(slog.NewTextHandler(io.Discard, nil)))

	alice := newFakeClient("agent-alice", models.SenderKindAgent)
	exec := newFakeClient("executor-one", models.SenderKindAgent)
	exec.participant.Metadata = map[string]string{"mode": "executor"}
	user := newFakeClient("operator", models.SenderKindUser)
	h.rooms["demo"] = map[*client]struct{}{alice: {}, exec: {}, user: {}}

	msg := models.ChatMessage{
		Type:           models.MessageTypeChat,
		SenderID:       "agent-alice",
		SenderKind:     models.SenderKindAgent,
		TargetID:       "operator",
		Content:        "你好 executor-one，请介绍一下",
		ReplyRequested: true,
		TurnBudget:     12,
	}
	saved, err := h.Publish(context.Background(), "demo", msg)
	if err != nil {
		t.Fatalf("Publish error: %v", err)
	}
	if saved.TargetID != "operator" {
		t.Fatalf("saved.TargetID = %q, want operator", saved.TargetID)
	}
}

func TestPublishRejectsCommandWithoutTarget(t *testing.T) {
	store := memory.NewStore()
	service := chat.NewService(store)
	h := newHub(service, slog.New(slog.NewTextHandler(io.Discard, nil)))

	cmd := models.ChatMessage{
		Type:       models.MessageTypeCommand,
		SenderID:   "operator",
		SenderKind: models.SenderKindAgent,
		Content:    "rm -rf /",
	}
	_, err := h.Publish(context.Background(), "demo", cmd)
	if !errors.Is(err, ErrCommandMissingTarget) {
		t.Fatalf("Publish err = %v, want ErrCommandMissingTarget", err)
	}

	stored, err := service.List(context.Background(), "demo", 10)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("rejected command leaked into history: %#v", stored)
	}
}

// TestParticipantsHideExecutorToken verifies the unauthenticated participants
// view never exposes an executor's token, even though presence carries it.
func TestParticipantsHideExecutorToken(t *testing.T) {
	h := newHub(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	executorClient := newFakeClient("executor-workstation-alice-1234abcd", models.SenderKindAgent)
	h.rooms["demo"] = map[*client]struct{}{executorClient: {}}

	h.updateParticipant(executorClient, models.ChatMessage{
		Type:       models.MessageTypePresence,
		SenderID:   "executor-workstation-alice-1234abcd",
		SenderKind: models.SenderKindAgent,
		Metadata: map[string]string{
			"mode":              "executor",
			"protocol":          "agent-room.executor.v1",
			"exec_token":        "room-visible-token",
			"credential_source": "relay",
		},
	})

	got := h.Participants("demo")
	if len(got) != 1 {
		t.Fatalf("len(Participants) = %d, want 1: %#v", len(got), got)
	}
	if leaked := got[0].Metadata["exec_token"]; leaked != "" {
		t.Fatalf("participants leaked exec_token = %q, want empty", leaked)
	}
	for _, conn := range got[0].Connections {
		if leaked := conn.Metadata["exec_token"]; leaked != "" {
			t.Fatalf("participant connection leaked exec_token = %q, want empty", leaked)
		}
	}
	if got[0].Metadata["mode"] != "executor" {
		t.Fatalf("non-sensitive metadata dropped: mode = %q", got[0].Metadata["mode"])
	}
	// The relay must still have recorded the token privately for stamping.
	if h.execToken("demo", "executor-workstation-alice-1234abcd") != "room-visible-token" {
		t.Fatalf("relay did not record exec_token for stamping")
	}
}

func newFakeClient(id string, kind models.SenderKind) *client {
	return &client{
		roomID: "demo",
		send:   make(chan outgoing, 4),
		participant: models.Participant{
			ID:     id,
			RoomID: "demo",
			Kind:   kind,
			Label:  id,
		},
	}
}

func drain(t *testing.T, c *client) models.ChatMessage {
	t.Helper()
	select {
	case msg := <-c.send:
		cm, ok := msg.(models.ChatMessage)
		if !ok {
			t.Fatalf("client %q received unexpected envelope type %T", c.participant.ID, msg)
		}
		return cm
	case <-time.After(time.Second):
		t.Fatalf("client %q timed out waiting for message", c.participant.ID)
		return models.ChatMessage{}
	}
}
