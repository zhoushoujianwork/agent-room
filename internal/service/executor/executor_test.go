package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-room/internal/models"
)

type fakeRunner struct {
	called bool
	req    models.CommandRunRequest
	resp   models.CommandRunResult
	err    error
}

func (r *fakeRunner) Run(_ context.Context, req models.CommandRunRequest) (models.CommandRunResult, error) {
	r.called = true
	r.req = req
	if r.resp.Duration == 0 {
		r.resp.Duration = 12 * time.Millisecond
	}
	return r.resp, r.err
}

func TestExecutorShouldHandleOnlyTargetedCommands(t *testing.T) {
	exec := New(Config{AgentID: "target", Token: "secret"}, &fakeRunner{})

	tests := []struct {
		name string
		msg  models.ChatMessage
		want bool
	}{
		{
			name: "targeted command",
			msg:  models.ChatMessage{Type: models.MessageTypeCommand, TargetID: "target", Content: "pwd"},
			want: true,
		},
		{
			name: "broadcast command is ignored",
			msg:  models.ChatMessage{Type: models.MessageTypeCommand, Content: "pwd"},
			want: false,
		},
		{
			name: "wrong target",
			msg:  models.ChatMessage{Type: models.MessageTypeCommand, TargetID: "other", Content: "pwd"},
			want: false,
		},
		{
			name: "chat is ignored",
			msg:  models.ChatMessage{Type: models.MessageTypeChat, TargetID: "target", Content: "pwd"},
			want: false,
		},
		{
			name: "empty command is ignored",
			msg:  models.ChatMessage{Type: models.MessageTypeCommand, TargetID: "target", Content: " "},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exec.ShouldHandle(tt.msg); got != tt.want {
				t.Fatalf("ShouldHandle = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExecutorRejectsInvalidTokenWithoutRunningCommand(t *testing.T) {
	runner := &fakeRunner{}
	exec := New(Config{AgentID: "target", Token: "secret"}, runner)

	reply := exec.Execute(context.Background(), models.ChatMessage{
		ID:       "msg_1",
		RoomID:   "demo",
		Type:     models.MessageTypeCommand,
		SenderID: "operator",
		TargetID: "target",
		Content:  "pwd",
		Metadata: map[string]string{
			"exec_token": "wrong",
		},
	})

	if runner.called {
		t.Fatal("runner was called for an unauthorized command")
	}
	if reply.Type != models.MessageTypeCommandResult {
		t.Fatalf("Type = %q, want command_result", reply.Type)
	}
	if reply.Metadata["error_type"] != "unauthorized" {
		t.Fatalf("error_type = %q, want unauthorized", reply.Metadata["error_type"])
	}
}

func TestExecutorRunsAuthorizedCommand(t *testing.T) {
	runner := &fakeRunner{
		resp: models.CommandRunResult{Stdout: "ok\n", ExitCode: 0},
	}
	exec := New(Config{
		AgentID:        "target",
		AgentLabel:     "Target",
		Token:          "secret",
		WorkingDir:     "/tmp",
		Timeout:        time.Second,
		MaxOutputBytes: 128,
	}, runner)

	reply := exec.Execute(context.Background(), models.ChatMessage{
		ID:       "msg_1",
		RoomID:   "demo",
		Type:     models.MessageTypeCommand,
		SenderID: "operator",
		TargetID: "target",
		Content:  "pwd",
		Metadata: map[string]string{
			"exec_token": "secret",
			"timeout_ms": "250",
		},
	})

	if !runner.called {
		t.Fatal("runner was not called")
	}
	if runner.req.Command != "pwd" {
		t.Fatalf("Command = %q, want pwd", runner.req.Command)
	}
	if runner.req.WorkingDir != "/tmp" {
		t.Fatalf("WorkingDir = %q, want /tmp", runner.req.WorkingDir)
	}
	if runner.req.Timeout != 250*time.Millisecond {
		t.Fatalf("Timeout = %s, want 250ms", runner.req.Timeout)
	}
	if reply.Metadata["exit_code"] != "0" {
		t.Fatalf("exit_code = %q, want 0", reply.Metadata["exit_code"])
	}
	if !strings.Contains(reply.Content, "stdout:\nok") {
		t.Fatalf("reply content missing stdout: %q", reply.Content)
	}
}

func TestExecutorMarksCanceledCommandStopped(t *testing.T) {
	runner := &fakeRunner{resp: models.CommandRunResult{ExitCode: 0}}
	exec := New(Config{AgentID: "target", AllowUnauthenticated: true}, runner)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reply := exec.Execute(ctx, models.ChatMessage{
		ID:       "msg_1",
		RoomID:   "demo",
		Type:     models.MessageTypeCommand,
		SenderID: "operator",
		TargetID: "target",
		Content:  "sleep 30",
	})

	if reply.Metadata["stopped"] != "true" {
		t.Fatalf("stopped = %q, want true", reply.Metadata["stopped"])
	}
	if reply.Metadata["exit_code"] != "-1" {
		t.Fatalf("exit_code = %q, want -1", reply.Metadata["exit_code"])
	}
}
