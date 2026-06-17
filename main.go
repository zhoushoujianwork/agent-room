package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"agent-room/app"
	"agent-room/internal/config"
	"agent-room/internal/version"
	"agent-room/pkg/logger"
)

// defaultRelayBase is the local development relay. Hosted deployments should
// set AGENT_ROOM_RELAY_URL or pass -relay explicitly.
const defaultRelayBase = "ws://127.0.0.1:8080"

func main() {
	cfg := config.Load()
	log := logger.New(slog.LevelInfo)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "relay":
		log.Info("agent-room starting", slog.String("role", "relay"), slog.String("version", version.String()))
		err = runRelay(cfg, log, os.Args[2:])
	case "bridge":
		log.Info("agent-room starting", slog.String("role", "bridge"), slog.String("version", version.String()))
		err = runBridge(cfg, log, os.Args[2:])
	case "direct-ai":
		err = runDirectAI(os.Args[2:])
	case "generate-id":
		err = runGenerateID(os.Args[2:])
	case "generate-token":
		err = runGenerateToken(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println(version.String())
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("agent-room exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func runRelay(cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP/WebSocket relay listen address")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "SQLite database file path; empty uses in-memory store")
	admins := fs.String("admins", strings.Join(cfg.Admins, ","), "comma-separated session logins granted cross-room admin powers (overrides AGENT_ROOM_ADMINS)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if list := config.SplitList(*admins); len(list) > 0 {
		cfg.Admins = list
	}
	return app.NewRelayApp(cfg, log).Run(context.Background())
}

func runBridge(cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("bridge", flag.ExitOnError)
	modeAlias := ""
	fs.StringVar(&cfg.RelayURL, "relay", cfg.RelayURL, "relay websocket URL")
	fs.StringVar(&cfg.RoomID, "room", cfg.RoomID, "room id")
	fs.StringVar(&cfg.BridgeMode, "bridge-mode", cfg.BridgeMode, "bridge mode: agent or executor")
	fs.StringVar(&modeAlias, "mode", "", "bridge mode alias: agent or executor")
	fs.StringVar(&cfg.AgentID, "agent-id", cfg.AgentID, "local agent id")
	fs.StringVar(&cfg.AgentLabel, "agent-label", cfg.AgentLabel, "display label for this local agent")
	fs.StringVar(&cfg.AgentToken, "agent-token", cfg.AgentToken, "agent token binding this agent to a relay user account (overrides AGENT_ROOM_AGENT_TOKEN); empty = anonymous agent")
	fs.StringVar(&cfg.Capabilities, "agent-capabilities", cfg.Capabilities, "short description of what this local agent can do")
	fs.StringVar(&cfg.Provider, "provider", cfg.Provider, "local CLI provider")
	fs.StringVar(&cfg.SystemPrompt, "system-prompt", cfg.SystemPrompt, "additional local owner instructions")
	fs.StringVar(&cfg.ContextFile, "context-file", cfg.ContextFile, "path to a file whose contents seed the agent's background context at startup (agent mode)")
	fs.IntVar(&cfg.HistoryLimit, "history-limit", cfg.HistoryLimit, "max recent messages fed into the prompt")
	fs.IntVar(&cfg.BackfillLimit, "backfill-limit", cfg.BackfillLimit, "messages to pull from the relay on join to seed local history (agent mode)")
	fs.IntVar(&cfg.TurnBudget, "turn-budget", cfg.TurnBudget, "default turn budget for manually sent messages")
	fs.StringVar(&cfg.Claude.Command, "claude-command", cfg.Claude.Command, "Claude CLI command path")
	fs.StringVar(&cfg.Claude.WorkingDir, "claude-workdir", cfg.Claude.WorkingDir, "Claude CLI working directory")
	fs.DurationVar(&cfg.Claude.Timeout, "claude-timeout", cfg.Claude.Timeout, "Claude CLI stream idle timeout (cancel if no output for this long)")
	fs.IntVar(&cfg.Claude.MaxTurns, "claude-max-turns", cfg.Claude.MaxTurns, "Claude CLI --max-turns")
	fs.BoolVar(&cfg.Claude.DisableTools, "claude-disable-tools", cfg.Claude.DisableTools, "run Claude with --tools \"\"")
	fs.BoolVar(&cfg.Claude.NoSessionPersistence, "claude-no-session-persistence", cfg.Claude.NoSessionPersistence, "run Claude with --no-session-persistence")
	fs.BoolVar(&cfg.Claude.SkipPermissions, "claude-skip-permissions", cfg.Claude.SkipPermissions, "run Claude with --dangerously-skip-permissions (default ON; use -claude-skip-permissions=false or AGENT_ROOM_CLAUDE_SKIP_PERMISSIONS=false to require local approval prompts)")
	fs.StringVar(&cfg.OpenCode.ServerURL, "opencode-server", cfg.OpenCode.ServerURL, "OpenCode server base URL (http://host:port from `opencode serve`), required for -provider opencode")
	fs.StringVar(&cfg.OpenCode.ProviderID, "opencode-provider-id", cfg.OpenCode.ProviderID, "OpenCode model providerID (body.model.providerID)")
	fs.StringVar(&cfg.OpenCode.ModelID, "opencode-model", cfg.OpenCode.ModelID, "OpenCode model modelID (body.model.modelID)")
	fs.DurationVar(&cfg.OpenCode.Timeout, "opencode-timeout", cfg.OpenCode.Timeout, "OpenCode completion timeout")
	fs.BoolVar(&cfg.OpenCode.SkipPermissions, "opencode-skip-permissions", cfg.OpenCode.SkipPermissions, "auto-approve every OpenCode tool permission (default OFF; OFF enables the room online-approval loop)")
	fs.StringVar(&cfg.Exec.Token, "exec-token", cfg.Exec.Token, "shared token required by executor bridge command messages")
	fs.BoolVar(&cfg.Exec.AllowUnauthenticated, "exec-allow-unauthenticated", cfg.Exec.AllowUnauthenticated, "allow executor bridge commands without exec-token")
	fs.StringVar(&cfg.Exec.WorkingDir, "exec-workdir", cfg.Exec.WorkingDir, "default working directory for executor bridge commands")
	fs.DurationVar(&cfg.Exec.Timeout, "exec-timeout", cfg.Exec.Timeout, "default timeout for executor bridge commands")
	fs.IntVar(&cfg.Exec.MaxOutputBytes, "exec-max-output-bytes", cfg.Exec.MaxOutputBytes, "max stdout and stderr bytes captured per executor command")
	fs.StringVar(&cfg.Exec.Shell, "exec-shell", cfg.Exec.Shell, "shell used by executor bridge commands")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(modeAlias) != "" {
		cfg.BridgeMode = modeAlias
	}

	if err := resolveBridgeDefaults(&cfg, bridgeModeExplicit(fs, modeAlias)); err != nil {
		return err
	}

	// Summary lines so the user can see what got auto-resolved.
	mode := strings.ToLower(strings.TrimSpace(cfg.BridgeMode))
	fmt.Fprintf(os.Stderr, "→ mode   %s\n→ relay  %s\n→ room   %s\n→ agent  %s\n",
		mode, cfg.RelayURL, cfg.RoomID, cfg.AgentID)
	if mode == "agent" {
		// banner 按 provider 读取对应的 skip-permissions 开关——claude 用
		// cfg.Claude.SkipPermissions(默认 ON),opencode 用 cfg.OpenCode.SkipPermissions
		// (默认 OFF=审批开)。否则 -provider opencode 时会误显示 claude 的配置。
		provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
		skipPermissions := cfg.Claude.SkipPermissions
		if provider == "opencode" {
			skipPermissions = cfg.OpenCode.SkipPermissions
		}
		if skipPermissions {
			fmt.Fprintf(os.Stderr, "→ tools  [%s] skip-permissions=ON   (no per-tool approval prompts)\n", provider)
		} else {
			fmt.Fprintf(os.Stderr, "→ tools  [%s] skip-permissions=OFF  (each tool call needs room approval)\n", provider)
		}
	}
	if mode == "executor" && strings.TrimSpace(cfg.Exec.Token) != "" {
		fmt.Fprintf(os.Stderr, "→ token  %s  (share with operators who can @ this executor)\n",
			cfg.Exec.Token)
	}

	// Single-instance guard: refuse to start if another live bridge with the
	// same identity is already running. In agent+token mode one bridge process
	// serves all rooms, so we lock only by agentID. In all other modes (legacy
	// agent, executor) we lock by (agentID, roomID) as before.
	isAgentWithToken := strings.EqualFold(strings.TrimSpace(cfg.BridgeMode), "agent") &&
		strings.TrimSpace(cfg.AgentToken) != ""
	lockRoomID := cfg.RoomID
	if isAgentWithToken {
		lockRoomID = "" // lock only on agentID
	}
	unlock, err := acquireBridgeLock(cfg.AgentID, lockRoomID)
	if err != nil {
		if errors.Is(err, errBridgeAlreadyRunning) {
			return fmt.Errorf("%w (agent %q, room %q); stop the other process or pass a different -agent-id", err, cfg.AgentID, lockRoomID)
		}
		return err
	}
	defer unlock()

	return app.NewBridgeApp(cfg, log).Run(context.Background())
}

func bridgeModeExplicit(fs *flag.FlagSet, modeAlias string) bool {
	if strings.TrimSpace(modeAlias) != "" || strings.TrimSpace(os.Getenv("AGENT_ROOM_BRIDGE_MODE")) != "" {
		return true
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "bridge-mode" || f.Name == "mode" {
			explicit = true
		}
	})
	return explicit
}

// resolveBridgeDefaults fills in -room, -agent-id, -relay, and (for executor
// mode) -exec-token when the user hasn't provided them explicitly. Room is
// prompted from a TTY, ids and tokens are generated once and cached under
// ~/.agent-room/ so a bridge re-appears as the same participant across
// restarts.
func resolveBridgeDefaults(cfg *config.Config, modeExplicit bool) error {
	if !modeExplicit {
		if mode, ok := promptBridgeModeFromTTY(os.Stdin, os.Stderr); ok {
			cfg.BridgeMode = mode
		}
	}

	// In agent+token mode the bridge can start without a room (it will receive
	// join_room directives over the control connection). Otherwise a room is
	// required and we prompt interactively when running from a TTY.
	agentTokenSet := strings.TrimSpace(cfg.AgentToken) != ""
	agentMode := strings.EqualFold(strings.TrimSpace(cfg.BridgeMode), "agent") || strings.TrimSpace(cfg.BridgeMode) == ""
	requireRoom := !(agentMode && agentTokenSet)

	if strings.TrimSpace(cfg.RoomID) == "" && requireRoom {
		room, err := promptRoomFromTTY(os.Stdin, os.Stderr)
		if err != nil {
			return err
		}
		cfg.RoomID = room
	}
	// Whether the value came from the flag, an env var, or the TTY prompt,
	// accept full URLs and ?room=… query strings transparently.
	cfg.RoomID = parseRoomIDFromInput(cfg.RoomID)
	if cfg.RoomID == "" && requireRoom {
		return errors.New("room id is required (use -room, set AGENT_ROOM_ROOM_ID, or run interactively)")
	}

	if strings.TrimSpace(cfg.AgentID) == "" {
		id, err := resolveCachedAgentID(cfg.BridgeMode)
		if err != nil {
			return err
		}
		cfg.AgentID = id
	}

	if strings.TrimSpace(cfg.RelayURL) == "" {
		base := strings.TrimRight(defaultRelayBase, "/")
		if cfg.RoomID != "" {
			cfg.RelayURL = base + "/v1/rooms/" + url.PathEscape(cfg.RoomID) + "/ws"
		} else {
			// Token mode with no initial room: store just the base so that
			// buildRoomURL / agentControlURL can derive the correct paths later.
			cfg.RelayURL = base
		}
	} else if cfg.RoomID != "" {
		cfg.RelayURL = normalizeRelayWSURL(cfg.RelayURL, cfg.RoomID)
	} else {
		// Bare origin provided; normalise scheme only (no room path to append).
		cfg.RelayURL = normalizeWSScheme(cfg.RelayURL)
	}

	// Carry the agent token (when set) as a handshake query param so the relay
	// can bind this agent to its owner. Appended after the room path is fixed so
	// it survives both the default and normalized URL branches above.
	if tok := strings.TrimSpace(cfg.AgentToken); tok != "" && cfg.RoomID != "" {
		cfg.RelayURL = appendQueryParam(cfg.RelayURL, "agent_token", tok)
	}

	// Executor mode: ensure there's a token so commands aren't accepted
	// from anyone in the room by accident. Skip if the operator opted into
	// -exec-allow-unauthenticated explicitly.
	if strings.EqualFold(strings.TrimSpace(cfg.BridgeMode), "executor") &&
		strings.TrimSpace(cfg.Exec.Token) == "" &&
		!cfg.Exec.AllowUnauthenticated {
		tok, err := resolveCachedExecToken()
		if err != nil {
			return err
		}
		cfg.Exec.Token = tok
	}

	return nil
}

func promptBridgeModeFromTTY(in *os.File, out *os.File) (string, bool) {
	info, err := in.Stat()
	if err != nil || (info.Mode()&os.ModeCharDevice) == 0 {
		return "", false
	}
	fmt.Fprint(out, "Bridge 模式 [agent/executor] (默认 agent): ")
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "a", "agent":
		return "agent", true
	case "e", "executor", "exec", "slave":
		return "executor", true
	default:
		fmt.Fprintf(out, "未识别模式 %q，使用 agent。需要被动执行命令可传 -mode executor 或 -bridge-mode executor。\n", strings.TrimSpace(line))
		return "agent", true
	}
}

// resolveCachedExecToken returns a stable shared secret for this executor.
// On first run a 24-byte random token is written to ~/.agent-room/exec-token
// with mode 0600; subsequent runs reuse it so previously-authorised operators
// keep working after a restart.
func resolveCachedExecToken() (string, error) {
	stateDir, err := bridgeStateDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	file := filepath.Join(stateDir, "exec-token")
	if data, err := os.ReadFile(file); err == nil {
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
	}
	tok, err := generateToken(24)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(file, []byte(tok+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "warn: failed to cache exec token at %s: %v\n", file, err)
	}
	return tok, nil
}

// promptRoomFromTTY reads a single line from stdin and extracts a room id.
// Accepts a raw id, a `?room=…` query string, or a full room URL.
func promptRoomFromTTY(in *os.File, out *os.File) (string, error) {
	info, err := in.Stat()
	if err != nil {
		return "", fmt.Errorf("stat stdin: %w", err)
	}
	if (info.Mode() & os.ModeCharDevice) == 0 {
		return "", errors.New("no room provided and stdin is not interactive (pass -room or set AGENT_ROOM_ROOM_ID)")
	}
	fmt.Fprint(out, "房间 ID (粘贴房间 ID 或完整链接，然后回车): ")
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("read room id: %w", err)
	}
	return parseRoomIDFromInput(strings.TrimSpace(line)), nil
}

// parseRoomIDFromInput accepts a raw id, a room URL, or anything containing
// ?room=… and returns just the room id.
func parseRoomIDFromInput(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if u, err := url.Parse(value); err == nil && (u.Scheme == "http" || u.Scheme == "https" || u.Host != "") {
		if r := strings.TrimSpace(u.Query().Get("room")); r != "" {
			return sanitizeRoomID(r)
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		// /rooms/<id> or /v1/rooms/<id>/ws
		for i, part := range parts {
			if part == "rooms" && i+1 < len(parts) {
				return sanitizeRoomID(parts[i+1])
			}
		}
	}
	return sanitizeRoomID(value)
}

// sanitizeRoomID normalises a room id to lowercase alphanumerics + dashes,
// matching the relay's server-side cleanRoomID so the URL we build is the
// same as what the relay would compute.
func sanitizeRoomID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	dash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// normalizeRelayWSURL accepts whatever the operator passed to -relay and
// returns a full room websocket URL. A value that already points at a room ws
// endpoint (path contains "/rooms/") is used as-is; a bare origin like
// "wss://relay.example.com" (or one with a trailing slash / partial path) gets
// "/v1/rooms/<room>/ws" appended. Without this, dialing a bare origin hits the
// relay root handler, which doesn't upgrade and returns "websocket: bad
// handshake". The ws/wss scheme is preserved; http(s) is upgraded to ws(s).
func normalizeRelayWSURL(raw, roomID string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw // can't parse; leave untouched so the dialer surfaces the error
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	// Already a room ws path (e.g. .../rooms/<id>/ws or .../rooms/<id>) — trust it.
	if strings.Contains(u.Path, "/rooms/") {
		return u.String()
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/rooms/" + url.PathEscape(roomID) + "/ws"
	return u.String()
}

// appendQueryParam adds (or overwrites) a single query parameter on a URL,
// preserving the rest of the URL. On a parse failure it returns the input
// unchanged so the dialer surfaces the original error rather than this helper
// silently dropping the value.
func appendQueryParam(raw, key, value string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// normalizeWSScheme converts http(s) to ws(s) and leaves ws/wss unchanged.
// Used when no room path needs to be appended (token mode, no initial room).
func normalizeWSScheme(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return raw
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	return u.String()
}

// resolveCachedAgentID returns a stable agent id for this user/host. On first
// run it generates a fresh id (agent-<host>-<user>-<4hex>) and writes it to
// ~/.agent-room/{mode}-id; subsequent runs reuse the same id so the agent
// appears as the same participant across restarts.
func resolveCachedAgentID(mode string) (string, error) {
	prefix := "agent"
	if strings.EqualFold(strings.TrimSpace(mode), "executor") {
		prefix = "executor"
	}
	stateDir, err := bridgeStateDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	file := filepath.Join(stateDir, prefix+"-id")
	if data, err := os.ReadFile(file); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
	}
	id, err := generateLocalID(prefix)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(file, []byte(id+"\n"), 0o600); err != nil {
		// Cache failure isn't fatal — proceed with the generated id but warn.
		fmt.Fprintf(os.Stderr, "warn: failed to cache agent id at %s: %v\n", file, err)
	}
	return id, nil
}

func bridgeStateDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("AGENT_ROOM_STATE_DIR")); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate user home dir: %w", err)
	}
	return filepath.Join(home, ".agent-room"), nil
}

func runGenerateID(args []string) error {
	fs := flag.NewFlagSet("generate-id", flag.ExitOnError)
	prefix := fs.String("prefix", "agent", "id prefix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := generateLocalID(*prefix)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func runGenerateToken(args []string) error {
	fs := flag.NewFlagSet("generate-token", flag.ExitOnError)
	size := fs.Int("bytes", 24, "random byte count")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *size <= 0 {
		return fmt.Errorf("bytes must be positive")
	}
	token, err := generateToken(*size)
	if err != nil {
		return err
	}
	fmt.Println(token)
	return nil
}

func generateLocalID(prefix string) (string, error) {
	prefix = sanitizeIDPart(prefix)
	if prefix == "" {
		prefix = "agent"
	}
	seed := firstNonEmpty(os.Getenv("COMPUTERNAME"), mustHostname())
	user := firstNonEmpty(os.Getenv("USERNAME"), os.Getenv("USER"), "local")
	base := sanitizeIDPart(seed + "-" + user)
	if base == "" {
		base = "local"
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generate id suffix: %w", err)
	}
	return fmt.Sprintf("%s-%s-%s", prefix, base, hex.EncodeToString(suffix)), nil
}

func generateToken(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(bytes), "="), nil
}

func sanitizeIDPart(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func mustHostname() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func usage() {
	fmt.Fprint(os.Stderr, `agent-room is a local AI agent chat bridge.

Quick start (connects to the local relay at ws://127.0.0.1:8080, prompts for room,
auto-generates and caches an agent id):
  agent-room bridge

Run a local relay instead:
  agent-room relay -addr :8080

Self-hosted bridge or scripted invocation (all flags optional):
  agent-room bridge -relay wss://your-relay/v1/rooms/<room>/ws -room <room> -agent-id <id>
  agent-room bridge -mode executor -room <room> -exec-token <secret>
  agent-room bridge -bridge-mode executor -room <room> -exec-token <secret>

Direct AI helper (message-level access, no bridge/provider/tools):
  agent-room direct-ai -room https://relay/?room=<room> -agent-id ai-you -hello
  agent-room direct-ai -room https://relay/?room=<room> -agent-id ai-you -tail 40
  agent-room direct-ai -room https://relay/?room=<room> -agent-id ai-you -say "message"

Other:
  agent-room generate-id -prefix executor
  agent-room generate-token
`)
}
