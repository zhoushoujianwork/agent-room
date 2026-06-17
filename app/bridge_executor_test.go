package app

import (
	"context"
	"testing"
	"time"

	"agent-room/internal/models"
)

func TestShouldCancelCommand(t *testing.T) {
	stop := func(commandID string) models.ChatMessage {
		meta := map[string]string{"operation": models.ControlOperationStop}
		if commandID != "" {
			meta["command_id"] = commandID
		}
		return models.ChatMessage{Type: models.MessageTypeControl, Metadata: meta}
	}

	tests := []struct {
		name             string
		msg              models.ChatMessage
		currentCommandID string
		want             bool
	}{
		{"nothing in flight", stop(""), "", false},
		{"stop without command_id cancels current", stop(""), "cmd_1", true},
		{"stop matching command_id", stop("cmd_1"), "cmd_1", true},
		{"stop mismatched command_id", stop("cmd_2"), "cmd_1", false},
		{
			name:             "legacy reply_to matches command_id",
			msg:              models.ChatMessage{Type: models.MessageTypeControl, Metadata: map[string]string{"operation": models.ControlOperationStop, "reply_to": "cmd_1"}},
			currentCommandID: "cmd_1",
			want:             true,
		},
		{
			name:             "non-control ignored",
			msg:              models.ChatMessage{Type: models.MessageTypeChat},
			currentCommandID: "cmd_1",
			want:             false,
		},
		{
			name:             "control without stop op ignored",
			msg:              models.ChatMessage{Type: models.MessageTypeControl, Metadata: map[string]string{"operation": "other"}},
			currentCommandID: "cmd_1",
			want:             false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldCancelCommand(tt.msg, tt.currentCommandID); got != tt.want {
				t.Fatalf("shouldCancelCommand = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExecutorRunStateCancel(t *testing.T) {
	state := &executorRunState{}
	ctx, cancel := context.WithCancel(context.Background())
	state.register("cmd_1", cancel)

	mismatch := models.ChatMessage{
		Type:     models.MessageTypeControl,
		Metadata: map[string]string{"operation": models.ControlOperationStop, "command_id": "cmd_2"},
	}
	if state.cancel(mismatch) {
		t.Fatal("mismatched stop should not cancel")
	}

	stop := models.ChatMessage{
		Type:     models.MessageTypeControl,
		Metadata: map[string]string{"operation": models.ControlOperationStop, "command_id": "cmd_1"},
	}
	if !state.cancel(stop) {
		t.Fatal("matching stop should cancel")
	}

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancel")
	}

	state.clear("cmd_1")
	if state.cancel(stop) {
		t.Fatal("cleared state should not cancel")
	}
}
