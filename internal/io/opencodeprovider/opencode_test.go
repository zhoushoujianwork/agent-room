package opencodeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-room/internal/models"
)

// mockServer 模拟一个 opencode HTTP server 的最小子集:
//   - POST /session                                 建会话, 返回 {id}
//   - POST /session/:id/message                     接收 prompt(只 200, 真正回复走 SSE)
//   - GET  /event                                   SSE: 把 events 通道里的事件逐条推给客户端
//   - POST /session/:id/permissions/:permID         审批回写, 记录到 replies
//
// events 通道用于在测试主体里编排事件时序(part -> permission.asked -> idle)。
type mockServer struct {
	srv      *httptest.Server
	events   chan map[string]any // 待推送的 SSE 事件; 关闭通道 = SSE 流自然结束
	sessHits int

	mu      sync.Mutex
	replies []permReply // 收到的审批回写
	sentMsg []string    // 收到的 prompt 文本
}

type permReply struct {
	sessionID string
	permID    string
	reply     string
}

func newMockServer(t *testing.T) *mockServer {
	t.Helper()
	m := &mockServer{events: make(chan map[string]any, 16)}
	mux := http.NewServeMux()

	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// /session/... 子路由也会落到这里(ServeMux 前缀匹配), 分流处理。
			m.handleSessionSub(w, r)
			return
		}
		m.mu.Lock()
		m.sessHits++
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "sess-1"})
	})

	// /session/ 前缀的子路由(message / permissions)。
	mux.HandleFunc("/session/", m.handleSessionSub)

	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer is not a Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case ev, ok := <-m.events:
				if !ok {
					return
				}
				buf, _ := json.Marshal(ev)
				fmt.Fprintf(w, "data: %s\n\n", buf)
				flusher.Flush()
			}
		}
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockServer) handleSessionSub(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/session/")
	// path 形如 "sess-1/message" 或 "sess-1/permissions/perm-9"
	parts := strings.Split(path, "/")
	switch {
	case len(parts) == 2 && parts[1] == "message":
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		text := ""
		if ps, ok := parsed["parts"].([]any); ok && len(ps) > 0 {
			if p0, ok := ps[0].(map[string]any); ok {
				text, _ = p0["text"].(string)
			}
		}
		m.mu.Lock()
		m.sentMsg = append(m.sentMsg, text)
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case len(parts) == 3 && parts[1] == "permissions":
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		// 实测 1.14.30: 回写 body 的字段名是 response(用 reply 会 400)。
		reply, _ := parsed["response"].(string)
		m.mu.Lock()
		m.replies = append(m.replies, permReply{sessionID: parts[0], permID: parts[2], reply: reply})
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func textPartEvent(text string) map[string]any {
	return map[string]any{
		"type": "message.part.updated",
		"properties": map[string]any{
			"part": map[string]any{"type": "text", "text": text},
		},
	}
}

func (m *mockServer) push(ev map[string]any) { m.events <- ev }
func (m *mockServer) idle()                  { m.events <- map[string]any{"type": "session.idle"} }

func (m *mockServer) getReplies() []permReply {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]permReply, len(m.replies))
	copy(out, m.replies)
	return out
}

func (m *mockServer) getSentMsgs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.sentMsg))
	copy(out, m.sentMsg)
	return out
}

func baseReq() models.ProviderRequest {
	return models.ProviderRequest{
		RoomID:     "demo",
		AgentID:    "alice",
		AgentLabel: "alice",
		Input: models.ChatMessage{
			SenderID:   "bob",
			SenderKind: models.SenderKindUser,
			Content:    "hi there",
		},
	}
}

func TestBuildOpenCodePromptIncludesReplyMetadata(t *testing.T) {
	req := baseReq()
	req.Input.Metadata = map[string]string{
		"reply_to":      "msg-quoted",
		"reply_sender":  "agent-a",
		"reply_preview": "原因很清楚: ImagePullBackOff, 镜像拉不下来。",
	}
	req.History = []models.ChatMessage{
		{
			Type:       models.MessageTypeChat,
			ID:         "msg-quoted",
			SenderID:   "agent-a",
			SenderKind: models.SenderKindAgent,
			Content:    "原因很清楚: ImagePullBackOff, 镜像拉不下来。镜像名: patsnap-dataprocess.tencentcloudcr.com/foo/bar:bad-tag",
		},
	}
	prompt := buildOpenCodePrompt(req)
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

func TestBuildOpenCodePromptFallsBackToReplyExcerpt(t *testing.T) {
	req := baseReq()
	req.Input.Metadata = map[string]string{
		"reply_to":      "msg-too-old",
		"reply_sender":  "agent-a",
		"reply_preview": "短 preview",
		"reply_excerpt": "更完整的引用内容，包含镜像名 patsnap-dataprocess.tencentcloudcr.com/data/flink:1.12.7-scala_2.11_clent_logback 和拼写错误 clent。",
	}
	prompt := buildOpenCodePrompt(req)
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

func TestBuildOpenCodePromptSkipsReplySectionWhenRelayInjectedContent(t *testing.T) {
	req := baseReq()
	req.Input.Content = "Quoted message context for this reply...\n\nIncoming reply message:\n@alice 这个说了啥"
	req.Input.Metadata = map[string]string{
		"reply_to":               "msg-too-old",
		"reply_excerpt":          "更完整的引用内容",
		"reply_context_injected": "content",
	}
	prompt := buildOpenCodePrompt(req)
	if strings.Contains(prompt, "Quoted message context for the incoming reply:") {
		t.Fatalf("prompt duplicated reply context section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Incoming reply message:") {
		t.Fatalf("prompt lost relay-injected content:\n%s", prompt)
	}
}

// TestCompleteAggregatesText: 基础回路 —— 发 prompt, 收到两段 text part,
// session.idle 结束 -> 聚合文本 = 两段拼接, 并触发对应 ProviderEventText。
func TestCompleteAggregatesText(t *testing.T) {
	m := newMockServer(t)
	p := NewOpenCodeProvider(m.srv.URL, "", 5*time.Second, "anthropic", "claude-3", false)

	var events []models.ProviderEvent
	var evMu sync.Mutex
	req := baseReq()
	req.OnEvent = func(ev models.ProviderEvent) {
		evMu.Lock()
		events = append(events, ev)
		evMu.Unlock()
	}

	// 编排事件时序: 等 message 被投递后再推 part + idle。
	go func() {
		waitForSends(m, 1)
		m.push(map[string]any{
			"type": "message.part.updated",
			"properties": map[string]any{
				"part": map[string]any{"type": "text", "text": "Hello, "},
			},
		})
		m.push(map[string]any{
			"type": "message.part.updated",
			"properties": map[string]any{
				"part": map[string]any{"type": "text", "text": "world!"},
			},
		})
		m.idle()
	}()

	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "Hello, world!" {
		t.Fatalf("content = %q, want %q", resp.Content, "Hello, world!")
	}
	if resp.SessionID != "sess-1" {
		t.Fatalf("session id = %q, want sess-1", resp.SessionID)
	}
	if got := m.getSentMsgs(); len(got) != 1 || !strings.Contains(got[0], "hi there") {
		t.Fatalf("sent prompt = %v, want one containing user input", got)
	}

	evMu.Lock()
	defer evMu.Unlock()
	var textEvents int
	for _, ev := range events {
		if ev.Type == models.ProviderEventText {
			textEvents++
		}
	}
	if textEvents != 2 {
		t.Fatalf("expected 2 text events, got %d (%+v)", textEvents, events)
	}
}

// TestPermissionAllowOnce: permission.asked -> RequestPermission 被调用且字段
// 正确 -> 房间给 allow_once -> provider 向 /permissions POST {response:"once"}。
// payload 用 1.14.30 实测形态: 工具名在 permission, 命令在 patterns 数组,
// tool 是 {messageID, callID} 对象, always 是「总是批准」放行的模式列表。
func TestPermissionAllowOnce(t *testing.T) {
	m := newMockServer(t)
	p := NewOpenCodeProvider(m.srv.URL, "", 5*time.Second, "anthropic", "claude-3", false)

	var gotReq models.PermissionRequest
	var reqCount int
	var reqMu sync.Mutex
	req := baseReq()
	req.RequestPermission = func(ctx context.Context, pr models.PermissionRequest) (models.PermissionDecision, error) {
		reqMu.Lock()
		gotReq = pr
		reqCount++
		reqMu.Unlock()
		return models.PermissionDecision{Reply: models.PermissionAllowOnce}, nil
	}

	go func() {
		waitForSends(m, 1)
		m.push(map[string]any{
			"type": "permission.asked",
			"properties": map[string]any{
				"id":         "perm-9",
				"sessionID":  "sess-1",
				"permission": "bash",
				"patterns":   []any{"ls -la", "date"},
				"always":     []any{"ls *", "date *"},
				"metadata":   map[string]any{},
				"tool":       map[string]any{"messageID": "msg-1", "callID": "call-7"},
			},
		})
		// 等审批回写后, 推一段最终文本再 idle(模拟工具放行后产出回复)。
		waitForReplies(m, 1)
		m.push(textPartEvent("done"))
		m.idle()
	}()

	_, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	reqMu.Lock()
	if reqCount != 1 {
		reqMu.Unlock()
		t.Fatalf("RequestPermission called %d times, want 1", reqCount)
	}
	if gotReq.RequestID != "perm-9" {
		t.Errorf("RequestID = %q, want perm-9", gotReq.RequestID)
	}
	if gotReq.Tool != "bash" {
		t.Errorf("Tool = %q, want bash", gotReq.Tool)
	}
	if gotReq.Input != "ls -la\ndate" {
		t.Errorf("Input = %q, want 'ls -la\\ndate'", gotReq.Input)
	}
	if gotReq.Metadata["call_id"] != "call-7" {
		t.Errorf("Metadata[call_id] = %q, want call-7", gotReq.Metadata["call_id"])
	}
	if gotReq.Metadata["always"] != "ls *\ndate *" {
		t.Errorf("Metadata[always] = %q, want 'ls *\\ndate *'", gotReq.Metadata["always"])
	}
	reqMu.Unlock()

	replies := m.getReplies()
	if len(replies) != 1 {
		t.Fatalf("expected 1 permission reply, got %d (%+v)", len(replies), replies)
	}
	if replies[0].reply != "once" {
		t.Errorf("reply = %q, want once", replies[0].reply)
	}
	if replies[0].permID != "perm-9" {
		t.Errorf("permID = %q, want perm-9", replies[0].permID)
	}
	if replies[0].sessionID != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1", replies[0].sessionID)
	}
}

// TestPermissionAllowAlways: allow_always -> reply "always"。
func TestPermissionAllowAlways(t *testing.T) {
	m := newMockServer(t)
	p := NewOpenCodeProvider(m.srv.URL, "", 5*time.Second, "anthropic", "claude-3", false)

	req := baseReq()
	req.RequestPermission = func(ctx context.Context, pr models.PermissionRequest) (models.PermissionDecision, error) {
		return models.PermissionDecision{Reply: models.PermissionAllowAlways}, nil
	}

	go func() {
		waitForSends(m, 1)
		m.push(map[string]any{
			"type":       "permission.asked",
			"properties": map[string]any{"id": "perm-A", "sessionID": "sess-1", "title": "edit file"},
		})
		waitForReplies(m, 1)
		m.push(textPartEvent("ok"))
		m.idle()
	}()

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	replies := m.getReplies()
	if len(replies) != 1 || replies[0].reply != "always" {
		t.Fatalf("expected reply always, got %+v", replies)
	}
}

// TestAlwaysRuleSurvivesAcrossSessions: 第一轮 allow_always 记住 always 模式后,
// 第二轮(新 session)同模式的 permission.asked 不再回灌房间, 由 provider 按
// 记忆自动放行(回写 always)。这是「总是批准」跨消息生效的关键 —— OpenCode 的
// always 只在单 session 内有效, 而每次 Complete 都新建 session。
func TestAlwaysRuleSurvivesAcrossSessions(t *testing.T) {
	m := newMockServer(t)
	p := NewOpenCodeProvider(m.srv.URL, "", 5*time.Second, "anthropic", "claude-3", false)

	var reqCount int
	var reqMu sync.Mutex
	req := baseReq()
	req.RequestPermission = func(ctx context.Context, pr models.PermissionRequest) (models.PermissionDecision, error) {
		reqMu.Lock()
		reqCount++
		reqMu.Unlock()
		return models.PermissionDecision{Reply: models.PermissionAllowAlways}, nil
	}
	var events []models.ProviderEvent
	var evMu sync.Mutex
	req.OnEvent = func(ev models.ProviderEvent) {
		evMu.Lock()
		events = append(events, ev)
		evMu.Unlock()
	}

	askEvent := func(permID string) map[string]any {
		return map[string]any{
			"type": "permission.asked",
			"properties": map[string]any{
				"id":         permID,
				"sessionID":  "sess-1",
				"permission": "bash",
				"patterns":   []any{"ping -c 3 www.baidu.com"},
				"always":     []any{"ping *"},
				"tool":       map[string]any{"messageID": "msg-1", "callID": "call-1"},
			},
		}
	}

	// 第一轮: 房间给 allow_always, provider 记下 "ping *"。
	go func() {
		waitForSends(m, 1)
		m.push(askEvent("perm-1"))
		waitForReplies(m, 1)
		m.push(textPartEvent("first done"))
		m.idle()
	}()
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete #1: %v", err)
	}

	// 第二轮: 同 always 模式再次触发, 不应再打扰房间。
	go func() {
		waitForSends(m, 2)
		m.push(askEvent("perm-2"))
		waitForReplies(m, 2)
		m.push(textPartEvent("second done"))
		m.idle()
	}()
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete #2: %v", err)
	}

	reqMu.Lock()
	if reqCount != 1 {
		reqMu.Unlock()
		t.Fatalf("RequestPermission called %d times, want 1 (second ask should auto-allow)", reqCount)
	}
	reqMu.Unlock()

	replies := m.getReplies()
	if len(replies) != 2 {
		t.Fatalf("expected 2 permission replies, got %d (%+v)", len(replies), replies)
	}
	if replies[0].reply != "always" || replies[0].permID != "perm-1" {
		t.Errorf("reply #1 = %+v, want always/perm-1", replies[0])
	}
	if replies[1].reply != "always" || replies[1].permID != "perm-2" {
		t.Errorf("reply #2 = %+v, want auto always/perm-2", replies[1])
	}

	// 自动放行应有一条 system 事件告知房间。
	evMu.Lock()
	defer evMu.Unlock()
	var autoNote bool
	for _, ev := range events {
		if ev.Type == models.ProviderEventSystem && strings.Contains(ev.Summary, "自动放行") {
			autoNote = true
		}
	}
	if !autoNote {
		t.Errorf("expected a system event noting auto-approval, got %+v", events)
	}
}

// TestPermissionDeny: deny -> reply "reject"。
func TestPermissionDeny(t *testing.T) {
	m := newMockServer(t)
	p := NewOpenCodeProvider(m.srv.URL, "", 5*time.Second, "anthropic", "claude-3", false)

	req := baseReq()
	req.RequestPermission = func(ctx context.Context, pr models.PermissionRequest) (models.PermissionDecision, error) {
		return models.PermissionDecision{Reply: models.PermissionDeny}, nil
	}

	go func() {
		waitForSends(m, 1)
		m.push(map[string]any{
			"type":       "permission.asked",
			"properties": map[string]any{"id": "perm-D", "sessionID": "sess-1", "title": "rm -rf"},
		})
		waitForReplies(m, 1)
		m.push(textPartEvent("denied, stopping"))
		m.idle()
	}()

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	replies := m.getReplies()
	if len(replies) != 1 || replies[0].reply != "reject" {
		t.Fatalf("expected reply reject, got %+v", replies)
	}
}

// TestSkipPermissionsAutoAllows: skipPermissions=true 时自动放行(POST once),
// 且不调用 RequestPermission。
func TestSkipPermissionsAutoAllows(t *testing.T) {
	m := newMockServer(t)
	p := NewOpenCodeProvider(m.srv.URL, "", 5*time.Second, "anthropic", "claude-3", true)

	var reqCalled bool
	req := baseReq()
	req.RequestPermission = func(ctx context.Context, pr models.PermissionRequest) (models.PermissionDecision, error) {
		reqCalled = true
		return models.PermissionDecision{Reply: models.PermissionDeny}, nil
	}

	go func() {
		waitForSends(m, 1)
		m.push(map[string]any{
			"type":       "permission.asked",
			"properties": map[string]any{"id": "perm-S", "sessionID": "sess-1", "title": "anything"},
		})
		waitForReplies(m, 1)
		m.push(textPartEvent("done"))
		m.idle()
	}()

	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if reqCalled {
		t.Fatal("RequestPermission should not be called when skipPermissions=true")
	}
	replies := m.getReplies()
	if len(replies) != 1 || replies[0].reply != "once" {
		t.Fatalf("expected auto-allow reply once, got %+v", replies)
	}
}

// TestContextCancelReturnsInterrupted: 父 ctx 取消 -> ErrProviderInterrupted。
func TestContextCancelReturnsInterrupted(t *testing.T) {
	m := newMockServer(t)
	// 不推 idle, 让 Complete 一直挂着, 直到 ctx 被取消。
	p := NewOpenCodeProvider(m.srv.URL, "", 0, "anthropic", "claude-3", false)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitForSends(m, 1)
		cancel()
	}()

	_, err := p.Complete(ctx, baseReq())
	if !errors.Is(err, models.ErrProviderInterrupted) {
		t.Fatalf("expected ErrProviderInterrupted, got %v", err)
	}
}

// TestEmptyServerURL: 空 serverURL 直接报错, 不发任何请求。
func TestEmptyServerURL(t *testing.T) {
	p := NewOpenCodeProvider("", "", time.Second, "anthropic", "claude-3", false)
	if _, err := p.Complete(context.Background(), baseReq()); err == nil {
		t.Fatal("expected error for empty server URL")
	}
}

// --- 轮询小工具: 等 mock server 累计到 n 条 message / reply ---

func waitForSends(m *mockServer, n int) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.getSentMsgs()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForReplies(m *mockServer, n int) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.getReplies()) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}
