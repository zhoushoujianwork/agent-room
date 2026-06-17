package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	"agent-room/internal/config"
	"agent-room/internal/io/console"
	"agent-room/internal/io/localexec"
	"agent-room/internal/io/memory"
	"agent-room/internal/io/provider"
	"agent-room/internal/models"
	"agent-room/internal/service/agent"
	"agent-room/internal/service/chat"
	"agent-room/internal/service/executor"
	"agent-room/internal/version"
)

type BridgeApp struct {
	cfg    config.Config
	logger *slog.Logger
}

func NewBridgeApp(cfg config.Config, logger *slog.Logger) *BridgeApp {
	return &BridgeApp{cfg: cfg, logger: logger}
}

type executorRunState struct {
	mu            sync.Mutex
	currentID     string
	currentCancel context.CancelFunc
}

func shouldCancelCommand(msg models.ChatMessage, currentCommandID string) bool {
	if currentCommandID == "" {
		return false
	}
	if msg.Type != models.MessageTypeControl {
		return false
	}
	if strings.TrimSpace(msg.Metadata["operation"]) != models.ControlOperationStop {
		return false
	}
	wantCommandID := strings.TrimSpace(msg.Metadata["command_id"])
	if wantCommandID == "" {
		wantCommandID = strings.TrimSpace(msg.Metadata["reply_to"])
	}
	if wantCommandID == "" {
		return true
	}
	return wantCommandID == currentCommandID
}

func (s *executorRunState) register(commandID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentID = commandID
	s.currentCancel = cancel
}

func (s *executorRunState) clear(commandID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentID != commandID {
		return
	}
	s.currentID = ""
	s.currentCancel = nil
}

func (s *executorRunState) cancel(msg models.ChatMessage) bool {
	s.mu.Lock()
	if s.currentCancel == nil || !shouldCancelCommand(msg, s.currentID) {
		s.mu.Unlock()
		return false
	}
	cancel := s.currentCancel
	s.mu.Unlock()
	cancel()
	return true
}

func (a *BridgeApp) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Windows conhost 的 QuickEdit 会在用户点击窗口后阻塞所有 console 写，
	// 冻住整个处理循环（executor 收到命令数分钟不执行）。启动即关闭。
	console.DisableQuickEdit()

	switch a.cfg.BridgeMode {
	case "", "agent":
		return a.runAgent(ctx)
	case "executor":
		return a.runExecutor(ctx)
	default:
		return fmt.Errorf("unsupported bridge mode %q", a.cfg.BridgeMode)
	}
}

type bridgeConnectionFunc func(context.Context, *websocket.Conn) error

// 心跳参数：bridge 主动 ping，并要求在 pongWait 内看到 pong（或任何数据）。
// 一条假死连接（对端没了、NAT 表项过期、进程冻结）在 ~75s 内必然触发读超时
// → handle 返回 → runWithReconnect 重连自愈。此前没有任何读超时，executor
// 冻住后会以"在线"状态永久挂死。
const (
	bridgePingInterval = 30 * time.Second
	bridgePongWait     = 75 * time.Second
	bridgeWriteWait    = 10 * time.Second
)

// keepAlive arms the read deadline + pong handler on conn and starts a pinger
// goroutine. Returned stop func must be called when the connection ends.
// gorilla 允许 WriteControl 与数据帧写并发，所以 pinger 不需要拿写锁。
func keepAlive(ctx context.Context, conn *websocket.Conn) (stop func()) {
	_ = conn.SetReadDeadline(time.Now().Add(bridgePongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(bridgePongWait))
	})

	pingCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(bridgePingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(bridgeWriteWait)); err != nil {
					// 写不出去说明连接已坏；读侧很快会因超时退出并重连。
					return
				}
			}
		}
	}()
	return cancel
}

func (a *BridgeApp) runWithReconnect(ctx context.Context, mode string, handle bridgeConnectionFunc) error {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}

		headers := http.Header{}
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.cfg.RelayURL, headers)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			a.logger.Warn("bridge connect failed; retrying",
				slog.String("mode", mode),
				slog.String("relay_url", a.cfg.RelayURL),
				slog.Duration("retry_after", backoff),
				slog.Any("error", err),
			)
			if !sleepContext(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff)
			continue
		}

		backoff = time.Second
		a.logger.Info("bridge connected",
			slog.String("mode", mode),
			slog.String("relay_url", a.cfg.RelayURL),
			slog.String("agent_id", a.cfg.AgentID),
		)
		stopClose := closeConnOnDone(ctx, conn)
		stopPing := keepAlive(ctx, conn)
		err = handle(ctx, conn)
		stopPing()
		stopClose()
		_ = conn.Close()

		if ctx.Err() != nil {
			return nil
		}
		a.logger.Warn("bridge disconnected; reconnecting",
			slog.String("mode", mode),
			slog.String("relay_url", a.cfg.RelayURL),
			slog.Duration("retry_after", backoff),
			slog.Any("error", err),
		)
		if !sleepContext(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff)
	}
}

func (a *BridgeApp) runAgent(ctx context.Context) error {
	localProvider, err := provider.New(a.cfg)
	if err != nil {
		return err
	}

	contextDoc := a.loadContextFile()

	store := memory.NewStore()
	chatService := chat.NewService(store)
	responder := agent.NewResponder(
		a.cfg.AgentID,
		a.cfg.AgentLabel,
		a.cfg.Capabilities,
		a.cfg.SystemPrompt,
		contextDoc,
		a.cfg.HistoryLimit,
		a.cfg.Claude.MaxTurns,
		relayHTTPBase(a.cfg.RelayURL),
		localProvider,
	)

	label := a.agentLabel()
	httpBase := relayHTTPBase(a.cfg.RelayURL)
	// One engine for the whole bridge lifetime: its worker and in-flight
	// generation state outlive individual connections, so a `claude -p` running
	// when the websocket drops keeps going and its reply/terminal trace flush on
	// reconnect instead of being orphaned (which used to strand the frontend
	// "thinking" clock and make stop a no-op).
	engine := &agentEngine{
		app:       a,
		responder: responder,
		chat:      chatService,
		provider:  localProvider,
		label:     label,
		httpBase:  httpBase,
		engineCtx: ctx,
		rooms:     make(map[string]*roomConn),
	}
	engine.start()

	hasToken := strings.TrimSpace(a.cfg.AgentToken) != ""

	if hasToken {
		// Token mode: start the control connection, which will receive
		// join_room / leave_room directives from the relay.
		go engine.runControlConn(ctx)

		// If -room was also provided, self-join it immediately (relay will
		// reconcile via room_state_report).
		if strings.TrimSpace(a.cfg.RoomID) != "" {
			engine.joinRoom(ctx, a.cfg.RoomID)
		}

		// Block until the process shuts down.
		<-ctx.Done()
		return nil
	}

	// Legacy (no-token) mode: single room, no control connection. Behavior is
	// identical to before this change.
	// registerRoom (not joinRoom) only creates the map entry without starting a
	// connection goroutine — the dial/reconnect cycle is owned by runWithReconnect
	// below, and serveConn drives presence + readLoop on each connection. Using
	// joinRoom here would start runRoomConn (connection #1) while runWithReconnect
	// simultaneously dials a second connection — causing duplicate presence and
	// double-handling of every inbound message.
	engine.registerRoom(ctx, a.cfg.RoomID)
	return a.runWithReconnect(ctx, "agent", func(ctx context.Context, conn *websocket.Conn) error {
		return engine.serveConn(ctx, conn)
	})
}

func (a *BridgeApp) runExecutor(ctx context.Context) error {
	if a.cfg.Exec.Token == "" && !a.cfg.Exec.AllowUnauthenticated {
		return fmt.Errorf("executor bridge mode requires -exec-token or explicit -exec-allow-unauthenticated")
	}

	store := memory.NewStore()
	chatService := chat.NewService(store)
	runner := localexec.NewRunner(
		a.cfg.Exec.Shell,
		a.cfg.Exec.WorkingDir,
		a.cfg.Exec.Timeout,
		a.cfg.Exec.MaxOutputBytes,
	)
	commandExecutor := executor.New(executor.Config{
		AgentID:              a.cfg.AgentID,
		AgentLabel:           a.agentLabel(),
		Token:                a.cfg.Exec.Token,
		AllowUnauthenticated: a.cfg.Exec.AllowUnauthenticated,
		WorkingDir:           a.cfg.Exec.WorkingDir,
		Timeout:              a.cfg.Exec.Timeout,
		MaxOutputBytes:       a.cfg.Exec.MaxOutputBytes,
	}, runner)

	label := a.agentLabel()
	return a.runWithReconnect(ctx, "executor", func(ctx context.Context, conn *websocket.Conn) error {
		presenceMetadata := map[string]string{
			"provider":     executor.ProviderName,
			"mode":         "executor",
			"protocol":     executor.ProtocolVersion,
			"api":          executor.PresenceAPI,
			"label":        label,
			"capabilities": firstNonEmpty(a.cfg.Capabilities, "Passive command executor"),
			"os":           runtime.GOOS,
			"version":      version.String(),
		}
		if token := strings.TrimSpace(a.cfg.Exec.Token); token != "" {
			// Reported to the relay only; it records the token privately and
			// stamps it onto commands itself. The relay strips this key before
			// any storage or broadcast, so it never reaches other participants.
			presenceMetadata["exec_token"] = token
			presenceMetadata["credential_source"] = "relay"
		}
		// gorilla/websocket 禁止并发写：presence/trace/result 全部经由 writeMu
		// 串行化（心跳 ping 走 WriteControl，gorilla 允许其与数据帧并发）。
		var writeMu sync.Mutex
		writeJSON := func(msg models.ChatMessage) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			return conn.WriteJSON(msg)
		}

		if err := writeJSON(models.ChatMessage{
			RoomID:     a.cfg.RoomID,
			Type:       models.MessageTypePresence,
			SenderID:   a.cfg.AgentID,
			SenderKind: models.SenderKindAgent,
			Content:    "executor joined",
			CreatedAt:  time.Now().UTC(),
			Metadata:   presenceMetadata,
		}); err != nil {
			return err
		}

		// 命令执行与读循环解耦：readLoop 只负责持续 pump（保证 relay 的
		// ping 能被即时 pong、stop/后续消息不被在执行中的命令阻塞），命令
		// 丢给独立 worker 串行执行。曾出现过 executor 在同步循环里被
		// Windows 控制台写阻塞 3 分钟、期间整条连接假死的事故。
		cmds := make(chan models.ChatMessage, 16)
		workerDone := make(chan struct{})
		runState := &executorRunState{}
		go func() {
			defer close(workerDone)
			for msg := range cmds {
				cmdCtx, cancel := context.WithCancel(ctx)
				runState.register(msg.ID, cancel)
				a.executeAndReport(cmdCtx, commandExecutor, label, msg, writeJSON)
				runState.clear(msg.ID)
				cancel()
			}
		}()
		defer func() {
			close(cmds)
			<-workerDone
		}()

		for {
			select {
			case <-ctx.Done():
				return nil
			default:
			}

			var msg models.ChatMessage
			if err := conn.ReadJSON(&msg); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if _, err := chatService.Add(ctx, msg.RoomID, msg); err != nil {
				a.logger.Warn("store incoming message failed", slog.Any("error", err))
			}
			if msg.Type == models.MessageTypeControl &&
				msg.IsAddressedTo(a.cfg.AgentID) &&
				strings.TrimSpace(msg.Metadata["operation"]) == models.ControlOperationStop {
				if runState.cancel(msg) {
					a.logger.Info("executor stop signal honored",
						slog.String("room_id", msg.RoomID),
						slog.String("from", msg.SenderID),
						slog.String("command_id", strings.TrimSpace(msg.Metadata["command_id"])),
					)
				}
				continue
			}
			if !commandExecutor.ShouldHandle(msg) {
				continue
			}

			select {
			case cmds <- msg:
			default:
				// 队列满说明 worker 积压；明确回报而不是悄悄丢弃。
				a.logger.Warn("command queue full; rejecting", slog.String("command_id", msg.ID), slog.String("from", msg.SenderID))
				_ = writeJSON(models.ChatMessage{
					RoomID:     msg.RoomID,
					Type:       models.MessageTypeCommandResult,
					SenderID:   a.cfg.AgentID,
					SenderKind: models.SenderKindAgent,
					TargetID:   msg.SenderID,
					Content:    "executor busy: command queue full, try again later",
					CreatedAt:  time.Now().UTC(),
					Metadata: map[string]string{
						"command_id": msg.ID,
						"error":      "queue_full",
						"provider":   executor.ProviderName,
						"label":      label,
					},
				})
			}
		}
	})
}

// executeAndReport runs one delegated command and reports the executing/result/
// done sequence through writeJSON. Runs on the executor worker goroutine so a
// slow command (or a blocked console log write) never stalls the read loop.
func (a *BridgeApp) executeAndReport(ctx context.Context, commandExecutor *executor.Executor, label string, msg models.ChatMessage, writeJSON func(models.ChatMessage) error) {
	a.logger.Info("executing remote command", slog.String("room_id", msg.RoomID), slog.String("from", msg.SenderID), slog.String("target", msg.TargetID))
	startedAt := time.Now().UTC()
	if err := writeJSON(models.ChatMessage{
		RoomID:     msg.RoomID,
		Type:       models.MessageTypeTrace,
		SenderID:   a.cfg.AgentID,
		SenderKind: models.SenderKindAgent,
		TargetID:   msg.SenderID,
		Content:    "executing",
		CreatedAt:  startedAt,
		Metadata: map[string]string{
			"phase":      "executing",
			"command_id": msg.ID,
			"provider":   executor.ProviderName,
			"label":      label,
		},
	}); err != nil {
		a.logger.Warn("write executing trace failed", slog.Any("error", err))
		return
	}

	result := commandExecutor.Execute(ctx, msg)
	if err := writeJSON(result); err != nil {
		a.logger.Warn("write command result failed", slog.Any("error", err))
		return
	}
	phase := "done"
	content := "done"
	if result.Metadata["stopped"] == "true" {
		phase = "stopped"
		content = "stopped"
	}
	if err := writeJSON(models.ChatMessage{
		RoomID:     msg.RoomID,
		Type:       models.MessageTypeTrace,
		SenderID:   a.cfg.AgentID,
		SenderKind: models.SenderKindAgent,
		TargetID:   msg.SenderID,
		Content:    content,
		CreatedAt:  time.Now().UTC(),
		Metadata: map[string]string{
			"phase":       phase,
			"command_id":  msg.ID,
			"provider":    executor.ProviderName,
			"label":       label,
			"duration_ms": strconv.FormatInt(time.Since(startedAt).Milliseconds(), 10),
		},
	}); err != nil {
		a.logger.Warn("write done trace failed", slog.Any("error", err))
	}
}

func (a *BridgeApp) agentLabel() string {
	if a.cfg.AgentLabel != "" {
		return a.cfg.AgentLabel
	}
	return a.cfg.AgentID
}

// loadContextFile reads the optional -context-file so its contents can be
// injected into the agent prompt as trusted operator-provided background
// (e.g. an incident handoff note). Best-effort: a missing or unreadable file
// is logged and treated as "no context" rather than aborting startup, in
// keeping with the bridge's weak-dependency style. Returns "" when no context
// file is configured.
func (a *BridgeApp) loadContextFile() string {
	path := strings.TrimSpace(a.cfg.ContextFile)
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		a.logger.Warn("read context file failed; continuing without injected context",
			slog.String("path", path), slog.Any("error", err))
		return ""
	}
	doc := strings.TrimSpace(string(data))
	a.logger.Info("loaded agent context file",
		slog.String("path", path), slog.Int("bytes", len(doc)))
	return doc
}

// seedRoomHistory pulls recent room history from the relay REST API and
// loads it into the bridge's local store so the provider sees prior context
// across restarts/reconnects. Best-effort: errors are logged, not fatal, to
// preserve the bridge's anonymous-first, weak-dependency behavior. The
// memory store dedupes by message ID, so re-seeding on every reconnect is
// idempotent.
func (a *BridgeApp) seedRoomHistory(ctx context.Context, chatService *chat.Service, httpBase, roomID string) {
	if httpBase == "" {
		return
	}
	limit := a.cfg.BackfillLimit
	if limit <= 0 {
		limit = 100
	}
	history, err := fetchRoomHistory(ctx, httpBase, roomID, limit)
	if err != nil {
		a.logger.Warn("seed room history failed", slog.String("room_id", roomID), slog.Any("error", err))
		return
	}
	for _, msg := range history {
		if _, err := chatService.Add(ctx, msg.RoomID, msg); err != nil {
			a.logger.Warn("seed store message failed", slog.Any("error", err))
		}
	}
	stats := summarizeHistory(history)
	a.logger.Info("seeded room history",
		slog.String("room_id", roomID),
		slog.Int("messages", stats.Total),
		slog.String("by_type", stats.ByType),
		slog.String("oldest_age", stats.OldestAge),
	)
}

// replyHistory pulls a generous window from the local store (seeded by the
// join backfill) for building a reply. The responder applies type-aware
// truncation so presence/executor awareness survives a long history; we just
// need to fetch at least BackfillLimit (falling back to HistoryLimit) here.
// Best-effort: returns nil on error.
func (a *BridgeApp) replyHistory(ctx context.Context, chatService *chat.Service, roomID string) []models.ChatMessage {
	listLimit := a.cfg.BackfillLimit
	if listLimit < a.cfg.HistoryLimit {
		listLimit = a.cfg.HistoryLimit
	}
	history, err := chatService.List(ctx, roomID, listLimit)
	if err != nil {
		a.logger.Warn("list history failed", slog.Any("error", err))
		return nil
	}
	return history
}

func closeConnOnDone(ctx context.Context, conn *websocket.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current time.Duration) time.Duration {
	next := current * 2
	if next > 30*time.Second {
		return 30 * time.Second
	}
	return next
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// relayHTTPBase converts a relay websocket URL to its HTTP origin so providers
// can issue REST calls back at the relay (e.g. POST /v1/rooms/<room>/messages).
// Returns "" when the URL cannot be parsed.
func relayHTTPBase(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		return "https://" + u.Host
	case "ws", "http":
		return "http://" + u.Host
	default:
		return ""
	}
}
