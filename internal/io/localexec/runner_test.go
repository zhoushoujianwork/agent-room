package localexec

import (
	"context"
	"strings"
	"testing"
	"time"

	"agent-room/internal/models"
)

func TestRunnerExecutesShellCommand(t *testing.T) {
	runner := NewRunner("", "", time.Second, 1024)
	result, err := runner.Run(context.Background(), models.CommandRunRequest{
		Command: "echo agent-room-exec-ok",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0; stderr=%q", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "agent-room-exec-ok") {
		t.Fatalf("Stdout = %q, want marker", result.Stdout)
	}
}

func TestRunnerCapturesNonZeroExit(t *testing.T) {
	runner := NewRunner("", "", time.Second, 1024)
	result, err := runner.Run(context.Background(), models.CommandRunRequest{
		Command: "exit 7",
	})
	if err != nil {
		t.Fatalf("Run returned setup error for non-zero exit: %v", err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}
}

func TestRunnerTreatsCanceledContextAsCommandResult(t *testing.T) {
	runner := NewRunner("", "", time.Second, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := runner.Run(ctx, models.CommandRunRequest{
		Command: "echo should-not-run",
	})
	if err != nil {
		t.Fatalf("Run returned setup error for canceled context: %v", err)
	}
	if result.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", result.ExitCode)
	}
}

func TestDecodeShellOutput(t *testing.T) {
	// "系统找不到指定的路径。" 的 GBK 编码（中文 Windows cmd 的典型 stderr）。
	gbk := []byte{0xcf, 0xb5, 0xcd, 0xb3, 0xd5, 0xd2, 0xb2, 0xbb, 0xb5, 0xbd,
		0xd6, 0xb8, 0xb6, 0xa8, 0xb5, 0xc4, 0xc2, 0xb7, 0xbe, 0xb6, 0xa1, 0xa3}

	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, ""},
		{"plain ascii", []byte("hello"), "hello"},
		{"valid utf8 chinese", []byte("系统正常"), "系统正常"},
		{"gbk decoded", gbk, "系统找不到指定的路径。"},
		{"utf8 with truncated trailing rune", append([]byte("好的"), 0xe7), "好的"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeShellOutput(tc.in); got != tc.want {
				t.Fatalf("decodeShellOutput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRawCmdLine(t *testing.T) {
	got := rawCmdLine(`C:\Windows\System32\cmd.exe`, `dir /b "C:\Users\x\.codex\skills"`)
	want := `"C:\Windows\System32\cmd.exe" /S /C "dir /b "C:\Users\x\.codex\skills""`
	if got != want {
		t.Fatalf("rawCmdLine = %q, want %q", got, want)
	}
}
