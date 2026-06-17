package localexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"agent-room/internal/models"
)

const (
	defaultTimeout        = 30 * time.Second
	defaultMaxOutputBytes = 64 * 1024
)

type Runner struct {
	shell          string
	workingDir     string
	timeout        time.Duration
	maxOutputBytes int
}

func NewRunner(shell, workingDir string, timeout time.Duration, maxOutputBytes int) *Runner {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if maxOutputBytes <= 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	return &Runner{
		shell:          shell,
		workingDir:     workingDir,
		timeout:        timeout,
		maxOutputBytes: maxOutputBytes,
	}
}

func (r *Runner) Run(ctx context.Context, req models.CommandRunRequest) (models.CommandRunResult, error) {
	command := strings.TrimSpace(req.Command)
	if command == "" {
		return models.CommandRunResult{}, fmt.Errorf("command is empty")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = r.timeout
	}
	maxOutputBytes := req.MaxOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = r.maxOutputBytes
	}
	workingDir := strings.TrimSpace(req.WorkingDir)
	if workingDir == "" {
		workingDir = r.workingDir
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shellChoice := strings.TrimSpace(req.Shell)
	if shellChoice == "" {
		shellChoice = r.shell
	}
	shell, shellArg := shellCommand(shellChoice)
	cmd := exec.CommandContext(runCtx, shell, shellArg, command)
	// Windows cmd.exe 需要绕过 Go 的参数转义原样直传命令行(见
	// configureShell 的 windows 实现);其他平台为 no-op。
	configureShell(cmd, shell, command)
	if workingDir != "" {
		cmd.Dir = filepath.Clean(workingDir)
	}

	stdout := &limitedBuffer{limit: maxOutputBytes}
	stderr := &limitedBuffer{limit: maxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	startedAt := time.Now()
	err := cmd.Run()
	duration := time.Since(startedAt)
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	canceled := errors.Is(runCtx.Err(), context.Canceled)

	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if timedOut || canceled {
		exitCode = -1
	}

	result := models.CommandRunResult{
		Stdout:          decodeShellOutput(stdout.Bytes()),
		Stderr:          decodeShellOutput(stderr.Bytes()),
		ExitCode:        exitCode,
		Duration:        duration,
		TimedOut:        timedOut,
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
	}

	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !timedOut && !canceled {
		return result, err
	}
	return result, nil
}

// decodeShellOutput normalizes raw shell output to UTF-8. 中文 Windows 的
// cmd.exe/PowerShell 默认按 ANSI 代码页(GBK/CP936)输出，直接透传会让前端
// 满屏乱码（线上见过 stderr 全是 "ϵͳ�Ҳ���..."）。策略：本身是合法 UTF-8
// 就原样返回；否则按 GBK 解码；解码失败再原样兜底。截断可能切断 UTF-8
// 多字节序列，先剥掉结尾的不完整 rune 再判定，避免把真 UTF-8 误判成 GBK。
func decodeShellOutput(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := trimPartialRune(raw)
	if utf8.Valid(trimmed) {
		return string(trimmed)
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(raw)
	if err != nil {
		return string(raw)
	}
	return string(decoded)
}

// trimPartialRune drops an incomplete trailing multi-byte sequence produced by
// the output cap cutting mid-rune.
func trimPartialRune(b []byte) []byte {
	for i := 0; i < utf8.UTFMax && len(b) > 0; i++ {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size != 1 {
			return b
		}
		b = b[:len(b)-1]
	}
	return b
}

// rawCmdLine builds the literal command line handed to cmd.exe verbatim:
// `"<shell>" /S /C "<command>"`. /S 固定 cmd 的引号规则为"只剥首尾两个
// 引号",中间内容(含内嵌引号)原样执行——和用户在命令提示符里手敲完全
// 一致。背景:Go 的 EscapeArg 按 CommandLineToArgvW 规则把内嵌引号转义
// 成 \",但 cmd.exe 不认这套(内置命令把 \" 当字面量),导致任何带引号的
// 命令(如 dir /b "C:\path")必然 exit 1。
func rawCmdLine(shell, command string) string {
	return `"` + shell + `" /S /C "` + command + `"`
}

func shellCommand(shell string) (string, string) {
	if strings.TrimSpace(shell) == "" {
		if runtime.GOOS == "windows" {
			return "cmd", "/C"
		}
		return "/bin/sh", "-c"
	}

	base := strings.ToLower(filepath.Base(shell))
	switch base {
	case "bash", "zsh":
		return shell, "-lc"
	case "sh":
		return shell, "-c"
	case "cmd", "cmd.exe":
		return shell, "/C"
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return shell, "-Command"
	default:
		return shell, "-c"
	}
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}

	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		chunk := p
		if len(chunk) > remaining {
			chunk = chunk[:remaining]
			b.truncated = true
		}
		if _, err := b.buf.Write(chunk); err != nil {
			return 0, err
		}
	}
	if len(p) > remaining {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func (b *limitedBuffer) Truncated() bool {
	return b.truncated
}
