package agent

import (
	"context"
	"testing"

	"agent-room/internal/models"
)

type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }

func (fakeProvider) Complete(_ context.Context, _ models.ProviderRequest) (models.ProviderResponse, error) {
	return models.ProviderResponse{Content: "hello from agent"}, nil
}

func TestResponderShouldReplyOnlyWhenAddressedAndBudgeted(t *testing.T) {
	responder := NewResponder("alice", "", "", "", "", 10, 1, "", fakeProvider{})

	tests := []struct {
		name string
		msg  models.ChatMessage
		want bool
	}{
		{
			name: "reply requested",
			msg:  models.ChatMessage{Type: models.MessageTypeChat, SenderID: "bob", Content: "hi", ReplyRequested: true, TurnBudget: 1},
			want: true,
		},
		{
			name: "self message",
			msg:  models.ChatMessage{Type: models.MessageTypeChat, SenderID: "alice", Content: "hi", ReplyRequested: true, TurnBudget: 1},
			want: false,
		},
		{
			name: "wrong target",
			msg:  models.ChatMessage{Type: models.MessageTypeChat, SenderID: "bob", TargetID: "carol", Content: "hi", ReplyRequested: true, TurnBudget: 1},
			want: false,
		},
		{
			name: "budget exhausted",
			msg:  models.ChatMessage{Type: models.MessageTypeChat, SenderID: "bob", Content: "hi", ReplyRequested: true, TurnBudget: 0},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responder.ShouldReply(tt.msg); got != tt.want {
				t.Fatalf("ShouldReply = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResponderReplyDecrementsTurnBudget(t *testing.T) {
	responder := NewResponder("alice", "", "", "", "", 10, 1, "", fakeProvider{})
	reply, err := responder.Reply(context.Background(), models.ChatMessage{
		RoomID:     "demo",
		SenderID:   "bob",
		Content:    "hi",
		TurnBudget: 2,
	}, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.SenderID != "alice" {
		t.Fatalf("SenderID = %q", reply.SenderID)
	}
	if reply.TargetID != "bob" {
		t.Fatalf("TargetID = %q", reply.TargetID)
	}
	if reply.TurnBudget != 1 || !reply.ReplyRequested {
		t.Fatalf("turn budget = %d reply requested = %v", reply.TurnBudget, reply.ReplyRequested)
	}
}

type scriptedProvider struct{ content string }

func (scriptedProvider) Name() string { return "scripted" }

func (p scriptedProvider) Complete(_ context.Context, _ models.ProviderRequest) (models.ProviderResponse, error) {
	return models.ProviderResponse{Content: p.content}, nil
}

type captureProvider struct {
	req models.ProviderRequest
}

func (p *captureProvider) Name() string { return "capture" }

func (p *captureProvider) Complete(_ context.Context, req models.ProviderRequest) (models.ProviderResponse, error) {
	p.req = req
	return models.ProviderResponse{Content: "captured"}, nil
}

func TestResponderReplyKeepsQuotedMessageOutsideHistoryLimit(t *testing.T) {
	provider := &captureProvider{}
	responder := NewResponder("alice", "", "", "", "", 2, 1, "", provider)
	history := []models.ChatMessage{
		{ID: "msg-quoted", Type: models.MessageTypeChat, SenderID: "alice", SenderKind: models.SenderKindAgent, Content: "full quoted content that would otherwise be outside the tail"},
		{ID: "msg-old-1", Type: models.MessageTypeChat, SenderID: "bob", SenderKind: models.SenderKindUser, Content: "old 1"},
		{ID: "msg-old-2", Type: models.MessageTypeChat, SenderID: "bob", SenderKind: models.SenderKindUser, Content: "old 2"},
		{ID: "msg-tail-1", Type: models.MessageTypeChat, SenderID: "bob", SenderKind: models.SenderKindUser, Content: "tail 1"},
		{ID: "msg-tail-2", Type: models.MessageTypeChat, SenderID: "bob", SenderKind: models.SenderKindUser, Content: "tail 2"},
	}
	_, err := responder.Reply(context.Background(), models.ChatMessage{
		RoomID:         "demo",
		SenderID:       "bob",
		SenderKind:     models.SenderKindUser,
		Content:        "这个问题你看得到吗",
		ReplyRequested: true,
		TurnBudget:     1,
		Metadata: map[string]string{
			"reply_to":      "msg-quoted",
			"reply_preview": "truncated preview",
		},
	}, history, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	gotIDs := map[string]bool{}
	for _, msg := range provider.req.History {
		gotIDs[msg.ID] = true
	}
	for _, id := range []string{"msg-quoted", "msg-tail-1", "msg-tail-2"} {
		if !gotIDs[id] {
			t.Fatalf("provider history missing %s: %#v", id, provider.req.History)
		}
	}
	if gotIDs["msg-old-1"] || gotIDs["msg-old-2"] {
		t.Fatalf("provider history kept unrelated old messages: %#v", provider.req.History)
	}
}

func TestResponderReplyRoutesMentionedAgentPeerWhenBudgetRemains(t *testing.T) {
	responder := NewResponder("alice", "", "", "", "", 10, 1, "", scriptedProvider{
		content: "@carol please introduce your runtime",
	})
	history := []models.ChatMessage{
		{Type: models.MessageTypePresence, SenderID: "alice", SenderKind: models.SenderKindAgent},
		{Type: models.MessageTypePresence, SenderID: "carol", SenderKind: models.SenderKindAgent},
	}
	reply, err := responder.Reply(context.Background(), models.ChatMessage{
		RoomID:         "demo",
		SenderID:       "bob",
		SenderKind:     models.SenderKindUser,
		Content:        "talk to carol",
		ReplyRequested: true,
		TurnBudget:     2,
	}, history, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.TargetID != "carol" {
		t.Fatalf("TargetID = %q, want carol", reply.TargetID)
	}
	if !reply.ReplyRequested || reply.TurnBudget != 1 {
		t.Fatalf("turn budget = %d reply requested = %v, want 1/true", reply.TurnBudget, reply.ReplyRequested)
	}
}

func TestResponderReplyNoReplyMarkerStopsPeerLoop(t *testing.T) {
	responder := NewResponder("alice", "", "", "", "", 10, 1, "", scriptedProvider{
		content: "收到，待命中。\n\n[[no-reply]]",
	})
	reply, err := responder.Reply(context.Background(), models.ChatMessage{
		RoomID:         "demo",
		SenderID:       "carol",
		SenderKind:     models.SenderKindAgent,
		TargetID:       "alice",
		Content:        "收到，分工确认。",
		ReplyRequested: true,
		TurnBudget:     12,
	}, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.Content != "收到，待命中。" {
		t.Fatalf("content = %q, want marker stripped", reply.Content)
	}
	if reply.ReplyRequested {
		t.Fatalf("ReplyRequested = true, want false")
	}
	if reply.TurnBudget != 11 {
		t.Fatalf("TurnBudget = %d, want 11", reply.TurnBudget)
	}
}

func TestResponderReplyToAgentPeerDoesNotContinueWithoutExplicitMention(t *testing.T) {
	responder := NewResponder("alice", "", "", "", "", 10, 1, "", scriptedProvider{
		content: "收到，待命中。有活招呼。",
	})
	reply, err := responder.Reply(context.Background(), models.ChatMessage{
		RoomID:         "demo",
		SenderID:       "carol",
		SenderKind:     models.SenderKindAgent,
		TargetID:       "alice",
		Content:        "收到，分工确认。",
		ReplyRequested: true,
		TurnBudget:     12,
	}, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.ReplyRequested {
		t.Fatalf("ReplyRequested = true, want false")
	}
}

func TestResponderReplyToAgentPeerContinuesWithExplicitMention(t *testing.T) {
	responder := NewResponder("alice", "", "", "", "", 10, 1, "", scriptedProvider{
		content: "@carol 继续确认一下 Linux 侧结果",
	})
	reply, err := responder.Reply(context.Background(), models.ChatMessage{
		RoomID:         "demo",
		SenderID:       "carol",
		SenderKind:     models.SenderKindAgent,
		TargetID:       "alice",
		Content:        "收到，分工确认。",
		ReplyRequested: true,
		TurnBudget:     12,
	}, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if !reply.ReplyRequested {
		t.Fatalf("ReplyRequested = false, want true")
	}
}

func TestResponderReplyDoesNotRouteToExecutorMention(t *testing.T) {
	responder := NewResponder("alice", "", "", "", "", 10, 1, "", scriptedProvider{
		content: "@exec-1 run hostname",
	})
	history := []models.ChatMessage{
		{Type: models.MessageTypePresence, SenderID: "exec-1", SenderKind: models.SenderKindAgent, Metadata: map[string]string{"mode": "executor"}},
	}
	reply, err := responder.Reply(context.Background(), models.ChatMessage{
		RoomID:     "demo",
		SenderID:   "bob",
		Content:    "ask executor",
		TurnBudget: 2,
	}, history, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply.TargetID != "bob" {
		t.Fatalf("TargetID = %q, want bob", reply.TargetID)
	}
}

func TestResolveAttachments(t *testing.T) {
	msg := models.ChatMessage{
		RoomID: "room1",
		Metadata: map[string]string{
			models.MetadataKeyAttachments: `[{"id":"a1","mime":"image/png","name":"shot.png"},{"id":"a2","mime":"image/jpeg"}]`,
		},
	}

	atts := resolveAttachments(msg, "https://relay.example/")
	if len(atts) != 2 {
		t.Fatalf("got %d attachments, want 2", len(atts))
	}
	if atts[0].URL != "https://relay.example/v1/rooms/room1/attachments/a1" {
		t.Fatalf("url = %q", atts[0].URL)
	}
	if atts[0].MIME != "image/png" || atts[0].Name != "shot.png" {
		t.Fatalf("ref fields lost: %+v", atts[0])
	}

	// relay HTTP base 未知时 agent 拉不到字节, 返回 nil。
	if got := resolveAttachments(msg, ""); got != nil {
		t.Fatalf("want nil without relay base, got %+v", got)
	}
	// 无附件 metadata。
	if got := resolveAttachments(models.ChatMessage{RoomID: "room1"}, "https://relay.example"); got != nil {
		t.Fatalf("want nil without metadata, got %+v", got)
	}
	// 坏 JSON 容忍。
	bad := models.ChatMessage{RoomID: "room1", Metadata: map[string]string{models.MetadataKeyAttachments: "{not json"}}
	if got := resolveAttachments(bad, "https://relay.example"); got != nil {
		t.Fatalf("want nil on bad json, got %+v", got)
	}
}
