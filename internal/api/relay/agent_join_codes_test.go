package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"agent-room/internal/models"
)

func createJoinCode(t *testing.T, s *Server, login, roomID string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/rooms/"+roomID+"/agent-join-codes", strings.NewReader(`{"note":"codex"}`))
	req.AddCookie(signedCookie(t, agentTestSecret, login))
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create join code = %d, body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode join code: %v", err)
	}
	if out.Code == "" {
		t.Fatalf("empty join code: %+v", out)
	}
	return out.Code
}

func redeemJoinCode(t *testing.T, s *Server, roomID, code, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"code":        code,
		"agent_id":    agentID,
		"agent_label": "Codex Skill",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/rooms/"+roomID+"/agent-join-codes/redeem", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	return rr
}

func TestAgentJoinCodeRedeemCreatesRoomScopedToken(t *testing.T) {
	s := newConfigServer(t, agentTestSecret, "the-secret", "root")
	ctx := context.Background()
	roomID := createTestRoom(t, s, "alice", true)
	code := createJoinCode(t, s, "alice", roomID)

	rr := redeemJoinCode(t, s, roomID, code, "agent-skill-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("redeem = %d, body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		AgentToken     string    `json:"agent_token"`
		TokenExpiresAt time.Time `json:"token_expires_at"`
		Relay          string    `json:"relay"`
		RelayWS        string    `json:"relay_ws"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode redeem: %v", err)
	}
	if out.AgentToken == "" || out.Relay == "" || out.RelayWS == "" || out.TokenExpiresAt.IsZero() {
		t.Fatalf("incomplete redeem response: %+v", out)
	}

	resolved, ok := s.resolveAgentToken(ctx, out.AgentToken)
	if !ok {
		t.Fatalf("temporary token did not validate")
	}
	if resolved.OwnerLogin != "alice" || resolved.RoomID != roomID || !resolved.Temporary {
		t.Fatalf("resolved token = %+v, want alice/%s/temporary", resolved, roomID)
	}
	rooms, err := s.agents.ListAgentRooms(ctx, "agent-skill-1")
	if err != nil {
		t.Fatalf("list rooms: %v", err)
	}
	if len(rooms) != 1 || rooms[0] != roomID {
		t.Fatalf("desired rooms = %v, want [%s]", rooms, roomID)
	}

	rr = redeemJoinCode(t, s, roomID, code, "agent-skill-2")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("second redeem = %d, want 403", rr.Code)
	}
}

func TestRoomScopedAgentTokenCannotJoinOtherRoom(t *testing.T) {
	s := newConfigServer(t, agentTestSecret, "the-secret", "root")
	ctx := context.Background()
	roomID := createTestRoom(t, s, "alice", false)
	otherRoomID := createTestRoom(t, s, "bob", false)
	code := createJoinCode(t, s, "alice", roomID)
	rr := redeemJoinCode(t, s, roomID, code, "agent-skill-scope")
	if rr.Code != http.StatusOK {
		t.Fatalf("redeem = %d, body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		AgentToken string `json:"agent_token"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&out)

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
	_, resp, err := websocket.DefaultDialer.Dial(wsBase+"/v1/rooms/"+otherRoomID+"/ws?agent_token="+out.AgentToken, nil)
	if err == nil {
		t.Fatalf("expected scoped-token handshake to other room to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("scoped-token other-room status = %v, want 403", resp)
	}

	conn := dialCtrlWS(t, wsBase, out.AgentToken, "agent-skill-scope")
	defer conn.Close()
	if err := conn.WriteJSON(models.ChatMessage{
		Type:       models.MessageTypeControl,
		SenderID:   "agent-skill-scope",
		SenderKind: models.SenderKindAgent,
		Metadata: map[string]string{
			"operation": models.ControlOperationRoomStateReport,
			"rooms":     otherRoomID,
		},
	}); err != nil {
		t.Fatalf("write room_state_report: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	rooms, err := s.agents.ListAgentRooms(ctx, "agent-skill-scope")
	if err != nil {
		t.Fatalf("list rooms: %v", err)
	}
	if len(rooms) != 1 || rooms[0] != roomID {
		t.Fatalf("scoped token reconciled other room: %v, want [%s]", rooms, roomID)
	}
}

func TestTemporaryAgentTokenDoesNotReceiveStoredConfig(t *testing.T) {
	s := newConfigServer(t, agentTestSecret, "the-secret", "root")
	ctx := context.Background()
	roomID := createTestRoom(t, s, "alice", false)
	agentID := "agent-skill-config"
	_ = s.agents.UpsertAgent(ctx, models.Agent{AgentID: agentID, OwnerLogin: "alice"})
	_ = putConfig(t, s, "alice", agentID, `{"model":"secret-model","api_key":"sk-secret-1234567890"}`)

	code := createJoinCode(t, s, "alice", roomID)
	rr := redeemJoinCode(t, s, roomID, code, agentID)
	if rr.Code != http.StatusOK {
		t.Fatalf("redeem = %d, body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		AgentToken string `json:"agent_token"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&out)

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn := dialCtrlWS(t, wsBase, out.AgentToken, agentID)
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	for {
		var msg models.ChatMessage
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		if msg.Type == models.MessageTypeControl && msg.Metadata["operation"] == models.ControlOperationConfigUpdate {
			t.Fatalf("temporary token received config_update: %+v", msg.Metadata)
		}
	}
}
