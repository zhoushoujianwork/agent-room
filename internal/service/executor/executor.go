package executor

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strconv"
	"strings"
	"time"

	"agent-room/internal/models"
	"agent-room/pkg/id"
)

const ProviderName = "local-exec"
const ProtocolVersion = "agent-room.executor.v1"

const PresenceAPI = `send {"type":"command","target_id":"<agent_id>","content":"<shell command>","metadata":{"operation":"exec","exec_token":"<token>","timeout_ms":"10000","cwd":"/optional/path","shell":"optional: powershell|cmd|bash"}}; receives {"type":"command_result","metadata":{"exit_code":"0","timed_out":"false","stdout_truncated":"false","stderr_truncated":"false"}}`

type Config struct {
	AgentID              string
	AgentLabel           string
	Token                string
	AllowUnauthenticated bool
	WorkingDir           string
	Timeout              time.Duration
	MaxOutputBytes       int
}

type Executor struct {
	cfg    Config
	runner models.CommandRunner
}

func New(cfg Config, runner models.CommandRunner) *Executor {
	return &Executor{cfg: cfg, runner: runner}
}

func (e *Executor) ShouldHandle(msg models.ChatMessage) bool {
	if msg.Type != models.MessageTypeCommand {
		return false
	}
	if strings.TrimSpace(msg.Content) == "" {
		return false
	}
	return msg.TargetID == e.cfg.AgentID
}

func (e *Executor) Execute(ctx context.Context, msg models.ChatMessage) models.ChatMessage {
	if !e.authorized(msg) {
		return e.errorResult(msg, "unauthorized", "command rejected: invalid exec token")
	}

	req := models.CommandRunRequest{
		Command:        msg.Content,
		WorkingDir:     firstNonEmpty(msg.Metadata["cwd"], e.cfg.WorkingDir),
		Timeout:        parseTimeout(msg.Metadata, e.cfg.Timeout),
		MaxOutputBytes: e.cfg.MaxOutputBytes,
		// 可选的 per-command shell 覆盖(如 Windows 上指定 powershell 绕开
		// cmd.exe 的 JSON 引号粉碎)。executor 本就执行任意命令,无新增风险。
		Shell: strings.TrimSpace(msg.Metadata["shell"]),
	}
	result, err := e.runner.Run(ctx, req)
	stopped := ctx.Err() != nil
	if err != nil && !stopped {
		return e.errorResult(msg, "runner_error", fmt.Sprintf("command failed before execution: %v", err))
	}
	if stopped && result.ExitCode == 0 {
		result.ExitCode = -1
	}

	metadata := e.baseMetadata(msg)
	metadata["exit_code"] = strconv.Itoa(result.ExitCode)
	metadata["duration_ms"] = strconv.FormatInt(result.Duration.Milliseconds(), 10)
	metadata["timed_out"] = strconv.FormatBool(result.TimedOut)
	metadata["stopped"] = strconv.FormatBool(stopped)
	metadata["stdout_truncated"] = strconv.FormatBool(result.StdoutTruncated)
	metadata["stderr_truncated"] = strconv.FormatBool(result.StderrTruncated)
	if req.WorkingDir != "" {
		metadata["cwd"] = req.WorkingDir
	}

	return models.ChatMessage{
		ID:         id.New("msg"),
		RoomID:     msg.RoomID,
		Type:       models.MessageTypeCommandResult,
		SenderID:   e.cfg.AgentID,
		SenderKind: models.SenderKindAgent,
		TargetID:   msg.SenderID,
		Content:    formatResult(result),
		CreatedAt:  time.Now().UTC(),
		Metadata:   metadata,
	}
}

func (e *Executor) authorized(msg models.ChatMessage) bool {
	if e.cfg.AllowUnauthenticated {
		return true
	}
	token := strings.TrimSpace(e.cfg.Token)
	if token == "" {
		return false
	}
	got := msg.Metadata["exec_token"]
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func (e *Executor) errorResult(msg models.ChatMessage, errorType string, content string) models.ChatMessage {
	metadata := e.baseMetadata(msg)
	metadata["phase"] = "error"
	metadata["error_type"] = errorType
	return models.ChatMessage{
		ID:         id.New("msg"),
		RoomID:     msg.RoomID,
		Type:       models.MessageTypeCommandResult,
		SenderID:   e.cfg.AgentID,
		SenderKind: models.SenderKindAgent,
		TargetID:   msg.SenderID,
		Content:    content,
		CreatedAt:  time.Now().UTC(),
		Metadata:   metadata,
	}
}

func (e *Executor) baseMetadata(msg models.ChatMessage) map[string]string {
	metadata := map[string]string{
		"provider":   ProviderName,
		"operation":  "exec",
		"command_id": msg.ID,
	}
	if e.cfg.AgentLabel != "" {
		metadata["label"] = e.cfg.AgentLabel
	}
	return metadata
}

func parseTimeout(metadata map[string]string, fallback time.Duration) time.Duration {
	if metadata == nil {
		return fallback
	}
	if raw := strings.TrimSpace(metadata["timeout"]); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			return d
		}
	}
	if raw := strings.TrimSpace(metadata["timeout_ms"]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return time.Duration(n) * time.Millisecond
		}
	}
	return fallback
}

func formatResult(result models.CommandRunResult) string {
	var b strings.Builder
	b.WriteString("exit_code=")
	b.WriteString(strconv.Itoa(result.ExitCode))
	if result.TimedOut {
		b.WriteString(" timed_out=true")
	}
	b.WriteString("\n")
	if result.Stdout != "" {
		b.WriteString("\nstdout:\n")
		b.WriteString(result.Stdout)
		if result.StdoutTruncated {
			b.WriteString("\n...[stdout truncated]")
		}
	}
	if result.Stderr != "" {
		b.WriteString("\nstderr:\n")
		b.WriteString(result.Stderr)
		if result.StderrTruncated {
			b.WriteString("\n...[stderr truncated]")
		}
	}
	if result.Stdout == "" && result.Stderr == "" {
		b.WriteString("\n(no output)")
	}
	return strings.TrimSpace(b.String())
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
