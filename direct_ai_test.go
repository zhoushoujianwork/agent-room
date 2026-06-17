package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-room/internal/models"
)

func TestDirectAIEndpointsFromRoomURL(t *testing.T) {
	got, err := directAIEndpointsFrom(
		"https://agent-room.example.test/?room=ABC-123",
		"",
		"ai-test",
		"AI Test",
	)
	if err != nil {
		t.Fatalf("directAIEndpointsFrom error: %v", err)
	}
	if got.RoomID != "abc-123" {
		t.Fatalf("RoomID = %q, want abc-123", got.RoomID)
	}
	if got.HTTPBase != "https://agent-room.example.test" {
		t.Fatalf("HTTPBase = %q", got.HTTPBase)
	}
	if got.MessagesURL != "https://agent-room.example.test/v1/rooms/abc-123/messages" {
		t.Fatalf("MessagesURL = %q", got.MessagesURL)
	}
	if !strings.HasPrefix(got.WebSocketURL, "wss://agent-room.example.test/v1/rooms/abc-123/ws?") {
		t.Fatalf("WebSocketURL = %q", got.WebSocketURL)
	}
}

func TestPostDirectAIMessagePayload(t *testing.T) {
	var got models.ChatMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		got.ID = "msg-test"
		_ = json.NewEncoder(w).Encode(got)
	}))
	defer srv.Close()

	msg, err := postDirectAIMessage(context.Background(), srv.URL, directAIOptions{
		agentID:      "ai-test",
		targetID:     "bridge-1",
		requestReply: true,
		turnBudget:   1,
	}, "hello bridge")
	if err != nil {
		t.Fatalf("postDirectAIMessage error: %v", err)
	}
	if msg.ID != "msg-test" {
		t.Fatalf("response ID = %q", msg.ID)
	}
	if got.SenderID != "ai-test" || got.SenderKind != models.SenderKindAgent {
		t.Fatalf("sender = %q/%q", got.SenderID, got.SenderKind)
	}
	if got.TargetID != "bridge-1" || !got.ReplyRequested || got.TurnBudget != 1 {
		t.Fatalf("target/reply/budget = %q/%v/%d", got.TargetID, got.ReplyRequested, got.TurnBudget)
	}
	if got.Metadata["source"] != "direct-ai-cli" {
		t.Fatalf("source metadata = %q", got.Metadata["source"])
	}
}

func TestDirectAIAddressed(t *testing.T) {
	cases := []struct {
		name string
		msg  models.ChatMessage
		want bool
	}{
		{
			name: "target",
			msg:  models.ChatMessage{SenderID: "user", TargetID: "ai-test", Content: "ping"},
			want: true,
		},
		{
			name: "mention",
			msg:  models.ChatMessage{SenderID: "user", Content: "hi @ai-test, check this"},
			want: true,
		},
		{
			name: "self ignored",
			msg:  models.ChatMessage{SenderID: "ai-test", TargetID: "ai-test", Content: "@ai-test"},
			want: false,
		},
		{
			name: "targeted trace ignored",
			msg:  models.ChatMessage{Type: models.MessageTypeTrace, SenderID: "bridge", TargetID: "ai-test", Content: "thinking"},
			want: false,
		},
		{
			name: "other mention ignored",
			msg:  models.ChatMessage{SenderID: "user", Content: "hi @ai-other"},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := directAIAddressed(tc.msg, "ai-test"); got != tc.want {
				t.Fatalf("directAIAddressed = %v, want %v", got, tc.want)
			}
		})
	}
}
