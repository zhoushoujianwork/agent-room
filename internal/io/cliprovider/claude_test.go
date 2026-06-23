package cliprovider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-room/internal/models"
)

func TestBuildClaudePromptInjectsContextDoc(t *testing.T) {
	doc := "INCIDENT HANDOFF: Flink 1.13 checkpoint stalls; see runbook step 4."
	prompt := buildClaudePrompt(models.ProviderRequest{
		RoomID:     "demo",
		AgentID:    "alice",
		AgentLabel: "alice",
		ContextDoc: doc,
		Input:      models.ChatMessage{SenderID: "bob", SenderKind: models.SenderKindUser, Content: "status?"},
	}, false)
	if !strings.Contains(prompt, doc) {
		t.Fatalf("prompt missing injected context doc:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Background context document") {
		t.Fatalf("prompt missing context section header:\n%s", prompt)
	}
}

func TestBuildClaudePromptOmitsContextSectionWhenEmpty(t *testing.T) {
	prompt := buildClaudePrompt(models.ProviderRequest{
		RoomID:  "demo",
		AgentID: "alice",
		Input:   models.ChatMessage{SenderID: "bob", Content: "hi"},
	}, false)
	if strings.Contains(prompt, "Background context document") {
		t.Fatalf("unexpected context section in prompt:\n%s", prompt)
	}
}

func TestBuildClaudePromptIncludesReplyMetadata(t *testing.T) {
	prompt := buildClaudePrompt(models.ProviderRequest{
		RoomID:     "demo",
		AgentID:    "agent-a",
		AgentLabel: "Agent A",
		Input: models.ChatMessage{
			SenderID:   "bob",
			SenderKind: models.SenderKindUser,
			Content:    "@agent-a 这里能看到我要回复的内容吗",
			Metadata: map[string]string{
				"reply_to":      "msg-quoted",
				"reply_sender":  "agent-a",
				"reply_preview": "原因很清楚: ImagePullBackOff, 镜像拉不下来。",
			},
		},
		History: []models.ChatMessage{
			{
				Type:       models.MessageTypeChat,
				ID:         "msg-quoted",
				SenderID:   "agent-a",
				SenderKind: models.SenderKindAgent,
				Content:    "原因很清楚: ImagePullBackOff, 镜像拉不下来。镜像名: patsnap-dataprocess.tencentcloudcr.com/foo/bar:bad-tag",
			},
		},
	}, false)
	for _, want := range []string{
		"Quoted message context for the incoming reply:",
		"reply_to: msg-quoted",
		`if the user says "this", "that", "这个", or "上面"`,
		"quoted_message: [agent-a/agent -> room] 原因很清楚: ImagePullBackOff, 镜像拉不下来。镜像名: patsnap-dataprocess.tencentcloudcr.com/foo/bar:bad-tag",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildClaudePromptFallsBackToReplyExcerpt(t *testing.T) {
	prompt := buildClaudePrompt(models.ProviderRequest{
		RoomID:     "demo",
		AgentID:    "agent-a",
		AgentLabel: "Agent A",
		Input: models.ChatMessage{
			SenderID:   "bob",
			SenderKind: models.SenderKindUser,
			Content:    "@agent-a 这个问题你看得到吗",
			Metadata: map[string]string{
				"reply_to":      "msg-too-old",
				"reply_sender":  "agent-a",
				"reply_preview": "短 preview",
				"reply_excerpt": "更完整的引用内容，包含镜像名 patsnap-dataprocess.tencentcloudcr.com/data/flink:1.12.7-scala_2.11_clent_logback 和拼写错误 clent。",
			},
		},
	}, false)
	for _, want := range []string{
		"Quoted message context for the incoming reply:",
		"reply_to: msg-too-old",
		"quoted_excerpt: 更完整的引用内容，包含镜像名 patsnap-dataprocess.tencentcloudcr.com/data/flink:1.12.7-scala_2.11_clent_logback 和拼写错误 clent。",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestBuildClaudePromptSkipsReplySectionWhenRelayInjectedContent(t *testing.T) {
	prompt := buildClaudePrompt(models.ProviderRequest{
		RoomID:     "demo",
		AgentID:    "agent-a",
		AgentLabel: "Agent A",
		Input: models.ChatMessage{
			SenderID:   "bob",
			SenderKind: models.SenderKindUser,
			Content:    "Quoted message context for this reply...\n\nIncoming reply message:\n@agent-a 这个说了啥",
			Metadata: map[string]string{
				"reply_to":               "msg-too-old",
				"reply_excerpt":          "更完整的引用内容",
				"reply_context_injected": "content",
			},
		},
	}, false)
	if strings.Contains(prompt, "Quoted message context for the incoming reply:") {
		t.Fatalf("prompt duplicated reply context section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Incoming reply message:") {
		t.Fatalf("prompt lost relay-injected content:\n%s", prompt)
	}
}

// TestCompleteInterruptReturnsSentinel verifies that cancelling the parent
// context mid-run surfaces ErrProviderInterrupted (deliberate stop) rather
// than a generic failure or timeout error.
func TestCompleteInterruptReturnsSentinel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub script is POSIX-only")
	}
	// A stub "claude" that ignores its args and just sleeps, so the run is
	// guaranteed to still be in flight when we cancel.
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	p := NewClaudeProvider(script, "", "", "", "", 0, 1, false, false, false)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	_, err := p.Complete(ctx, models.ProviderRequest{
		RoomID:  "demo",
		AgentID: "alice",
		Input:   models.ChatMessage{SenderID: "bob", SenderKind: models.SenderKindUser, Content: "hi"},
	})
	if !errors.Is(err, models.ErrProviderInterrupted) {
		t.Fatalf("expected ErrProviderInterrupted, got %v", err)
	}
}

func TestCompleteIdleTimeoutFires(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub script is POSIX-only")
	}
	// A stub that produces no output and just sleeps: with no stream lines to
	// reset the idle timer, the short idle timeout must fire.
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	p := NewClaudeProvider(script, "", "", "", "", 100*time.Millisecond, 1, false, false, false)

	_, err := p.Complete(context.Background(), models.ProviderRequest{
		RoomID:  "demo",
		AgentID: "alice",
		Input:   models.ChatMessage{SenderID: "bob", SenderKind: models.SenderKindUser, Content: "hi"},
	})
	if err == nil || !strings.Contains(err.Error(), "idle") {
		t.Fatalf("expected idle timeout error, got %v", err)
	}
}

func TestParseStreamResetsIdleTimerPerLine(t *testing.T) {
	// Replicates Complete's idle-timer mechanism without a subprocess (the go
	// test harness buffers subprocess pipes, making real-process stream timing
	// unreliable). A reader yields a line every 20ms for 400ms total — far past
	// the 200ms idle timeout — but because every line calls onLine -> reset, the
	// timer must never fire. The old wall-clock timeout would have killed this.
	const timeout = 200 * time.Millisecond
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() { timedOut.Store(true) })
	defer timer.Stop()
	resetIdle := func() { timer.Reset(timeout) }

	lines := make([]string, 20)
	for i := range lines {
		lines[i] = `{"type":"assistant","message":{"content":[{"type":"text","text":"step"}]}}`
	}
	lines = append(lines, `{"type":"result","result":"done"}`)
	r := &slowReader{lines: lines, gap: 20 * time.Millisecond}

	resp, _, _, err := parseClaudeStream(r, nil, resetIdle)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if timedOut.Load() {
		t.Fatal("idle timer fired despite steady per-line output")
	}
	if resp.Content != "done" {
		t.Fatalf("expected result content %q, got %q", "done", resp.Content)
	}
}

// slowReader feeds newline-terminated lines into a bufio.Scanner one at a time,
// sleeping gap between each so each Read models a stream line arriving over time.
type slowReader struct {
	lines []string
	gap   time.Duration
	idx   int
	buf   []byte
}

func (s *slowReader) Read(p []byte) (int, error) {
	for len(s.buf) == 0 {
		if s.idx >= len(s.lines) {
			return 0, io.EOF
		}
		if s.idx > 0 {
			time.Sleep(s.gap)
		}
		s.buf = []byte(s.lines[s.idx] + "\n")
		s.idx++
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

func TestBuildClaudePromptAttachmentGuide(t *testing.T) {
	req := models.ProviderRequest{
		AgentID:    "agent-a",
		AgentLabel: "Agent A",
		Input: models.ChatMessage{
			SenderID: "user-1", SenderKind: models.SenderKindUser, Content: "看下这张截图",
		},
		Attachments: []models.ProviderAttachment{
			{URL: "https://relay.example/v1/rooms/r1/attachments/att1", MIME: "image/png", Name: "shot.png"},
			{URL: "https://relay.example/v1/rooms/r1/attachments/att2", MIME: "image/jpeg"},
		},
	}
	prompt := buildClaudePrompt(req, false)
	for _, want := range []string{
		"https://relay.example/v1/rooms/r1/attachments/att1",
		"agentroom-att-1.png",
		"agentroom-att-2.jpg",
		"Read tool",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}

	// 无附件时不引入任何指引。
	req.Attachments = nil
	if strings.Contains(buildClaudePrompt(req, false), "image attachment") {
		t.Fatal("attachment guide leaked into prompt without attachments")
	}
}

func TestBuildClaudePromptAttachmentGuidePrefersLocalPath(t *testing.T) {
	req := models.ProviderRequest{
		AgentID:    "agent-a",
		AgentLabel: "Agent A",
		Input: models.ChatMessage{
			SenderID: "user-1", SenderKind: models.SenderKindUser, Content: "看下这张截图",
		},
		Attachments: []models.ProviderAttachment{
			{
				URL:       "https://relay.example/v1/rooms/r1/attachments/att1",
				MIME:      "image/png",
				Name:      "shot.png",
				LocalPath: "/tmp/agentroom-att-1.png",
			},
		},
	}
	prompt := buildClaudePrompt(req, false)
	for _, want := range []string{
		"already been downloaded",
		"/tmp/agentroom-att-1.png",
		"Read tool",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "curl -fsSL") {
		t.Fatalf("local attachment prompt should not ask Claude to curl:\n%s", prompt)
	}
}

func TestPrepareAttachmentFilesDownloadsAndCleansUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png-bytes"))
	}))
	defer srv.Close()

	atts, cleanup, err := prepareAttachmentFiles(context.Background(), []models.ProviderAttachment{
		{URL: srv.URL + "/shot.png", MIME: "image/png", Name: "shot.png"},
	})
	if err != nil {
		t.Fatalf("prepareAttachmentFiles: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil")
	}
	if len(atts) != 1 || atts[0].LocalPath == "" {
		t.Fatalf("missing local path: %#v", atts)
	}
	got, err := os.ReadFile(atts[0].LocalPath)
	if err != nil {
		t.Fatalf("read local attachment: %v", err)
	}
	if string(got) != "png-bytes" {
		t.Fatalf("local attachment bytes = %q", got)
	}
	dir := filepath.Dir(atts[0].LocalPath)
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("temp dir still exists or unexpected stat error: %v", err)
	}
}
