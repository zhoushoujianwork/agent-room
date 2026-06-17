package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr          string
	DBPath        string
	RelayURL      string
	PublicBaseURL string
	RoomID        string
	BridgeMode    string
	AgentID       string
	AgentLabel    string
	// AgentToken is the long-lived credential (bridge side) that binds this
	// agent to a user account on the relay. Sent as the agent_token WS query
	// param at handshake. Empty = anonymous agent (backward compatible).
	AgentToken   string
	Capabilities string
	Provider     string
	SystemPrompt string
	// ContextFile is an optional path to a local file (e.g. an incident
	// handoff note) whose contents are injected into the agent prompt at
	// startup as trusted operator-provided background. Agent mode only;
	// ignored by executor mode. Empty = no injected document.
	ContextFile   string
	HistoryLimit  int
	BackfillLimit int
	TurnBudget    int
	// Admins lists the session logins (the JWT subject minted by whichever
	// auth provider signed the user in) that get cross-room admin powers:
	// listing every room and managing/entering any of them as if they owned
	// it. Loaded from AGENT_ROOM_ADMINS (comma or space separated) and
	// overridable via the relay -admins flag. Empty = no admins.
	Admins []string
	// SecretKey (relay side) is the AGENT_ROOM_SECRET_KEY used to AES-256-GCM
	// encrypt per-agent api keys at rest. Empty disables api_key persistence
	// (model / api_base_url are unaffected).
	SecretKey string
	Claude    ClaudeConfig
	OpenCode  OpenCodeConfig
	Exec      ExecConfig
	GitHub    GitHubConfig
	SSO       ExternalSSOConfig
	LLM       LLMConfig
}

// IsAdmin reports whether the given session login has cross-room admin
// powers. Matching is case-insensitive and trims surrounding whitespace so
// the env list is forgiving. An empty login is never an admin.
func (c Config) IsAdmin(login string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	for _, a := range c.Admins {
		if strings.EqualFold(strings.TrimSpace(a), login) {
			return true
		}
	}
	return false
}

// GitHubConfig holds the optional GitHub OAuth + session configuration.
// Auth is opt-in at deploy time: when ClientID, ClientSecret, and
// SessionSecret are all set, the auth endpoints come online and room access
// requires a signed-in user. Otherwise auth-related routes return 404 and the
// relay serves anonymous rooms.
type GitHubConfig struct {
	ClientID      string
	ClientSecret  string
	RedirectURI   string
	SessionSecret string
	CookieName    string
}

// Enabled reports whether GitHub OAuth + session signing are fully
// configured. The relay must keep working when this returns false.
func (g GitHubConfig) Enabled() bool {
	return strings.TrimSpace(g.ClientID) != "" &&
		strings.TrimSpace(g.ClientSecret) != "" &&
		strings.TrimSpace(g.SessionSecret) != ""
}

// ExternalSSOConfig holds the optional external SSO configuration.
// Like GitHub OAuth it is opt-in at deploy time and additive: when the
// three AGENT_ROOM_SSO_* values are set (and a session secret exists to sign
// our own cookie), the /auth/sso/* routes come online as a SECOND login
// provider.
type ExternalSSOConfig struct {
	AppID        string // AGENT_ROOM_SSO_APP_ID (also the JWT audience)
	AppSecret    string // AGENT_ROOM_SSO_APP_SECRET (HS256 verify key for app_token)
	AuthorizeURL string // AGENT_ROOM_SSO_AUTHORIZE_URL (provider authorize endpoint)
}

// Enabled reports whether external SSO is fully configured.
func (s ExternalSSOConfig) Enabled() bool {
	return strings.TrimSpace(s.AppID) != "" &&
		strings.TrimSpace(s.AppSecret) != "" &&
		strings.TrimSpace(s.AuthorizeURL) != ""
}

// LLMConfig holds the optional external LLM HTTP API used by the relay to
// summarize long room histories. It is opt-in and additive, exactly like
// the auth providers: when APIKey is empty, all summary generation is a
// no-op and the relay behaves as before. The endpoint is Anthropic-
// compatible (POST {BaseURL}/v1/messages, x-api-key header).
type LLMConfig struct {
	APIKey  string
	BaseURL string
	Model   string
	// SummaryEvery triggers a room summary after this many new persisted
	// messages since the last summary. SummaryInterval is a wall-clock
	// fallback so quiet-but-active rooms still get refreshed. Whichever
	// fires first wins; both are clamped to sane minimums at use sites.
	SummaryEvery    int
	SummaryInterval time.Duration
}

// Enabled reports whether the relay can call the company LLM. When false
// the relay still serves rooms exactly as before, just without summaries.
func (l LLMConfig) Enabled() bool {
	return strings.TrimSpace(l.APIKey) != "" &&
		strings.TrimSpace(l.BaseURL) != "" &&
		strings.TrimSpace(l.Model) != ""
}

type ClaudeConfig struct {
	Command    string
	WorkingDir string
	// Model / APIBaseURL / APIKey are bridge-local defaults for the agent's
	// `claude -p` invocation (AGENT_ROOM_CLAUDE_MODEL / _API_BASE_URL /
	// _API_KEY). The relay can override any of them at runtime via a
	// config_update control message; a non-empty server value wins, an empty
	// server value falls back to these local defaults.
	Model                string
	APIBaseURL           string
	APIKey               string
	Timeout              time.Duration // 流式空闲超时:距上次输出超过此时长才判定卡死
	MaxTurns             int
	DisableTools         bool
	NoSessionPersistence bool
	SkipPermissions      bool
}

// OpenCodeConfig 配置 OpenCode provider(`-provider opencode`)。OpenCode 通过
// 外部已启动的 `opencode serve` HTTP server + SSE 事件总线驱动对话, 其价值在于
// 权限审批不绑死 TTY: 默认 SkipPermissions=false 以启用"房间在线审批"回路。
type OpenCodeConfig struct {
	ServerURL       string        // 已启动的 opencode server 基地址(http://host:port)
	ProviderID      string        // POST .../message 时 body.model.providerID
	ModelID         string        // POST .../message 时 body.model.modelID
	Timeout         time.Duration // 单次 Complete 整体超时
	SkipPermissions bool          // true=无人值守自动放行; false=启用审批回路(默认)
}

type ExecConfig struct {
	Token                string
	AllowUnauthenticated bool
	WorkingDir           string
	Timeout              time.Duration
	MaxOutputBytes       int
	Shell                string
}

func Load() Config {
	return Config{
		Addr:          envString("AGENT_ROOM_ADDR", ":8080"),
		DBPath:        envString("AGENT_ROOM_DB_PATH", ""),
		RelayURL:      envString("AGENT_ROOM_RELAY_URL", ""),
		PublicBaseURL: envString("AGENT_ROOM_PUBLIC_BASE_URL", ""),
		RoomID:        envString("AGENT_ROOM_ROOM_ID", ""),
		BridgeMode:    envString("AGENT_ROOM_BRIDGE_MODE", "agent"),
		AgentID:       envString("AGENT_ROOM_AGENT_ID", ""),
		AgentLabel:    envString("AGENT_ROOM_AGENT_LABEL", ""),
		AgentToken:    envString("AGENT_ROOM_AGENT_TOKEN", ""),
		Capabilities:  envString("AGENT_ROOM_AGENT_CAPABILITIES", ""),
		Provider:      envString("AGENT_ROOM_PROVIDER", "claude"),
		SystemPrompt:  envString("AGENT_ROOM_SYSTEM_PROMPT", ""),
		ContextFile:   envString("AGENT_ROOM_CONTEXT_FILE", ""),
		HistoryLimit:  envInt("AGENT_ROOM_HISTORY_LIMIT", 30),
		BackfillLimit: envInt("AGENT_ROOM_BACKFILL_LIMIT", 100),
		TurnBudget:    envInt("AGENT_ROOM_TURN_BUDGET", 1),
		Admins:        SplitList(os.Getenv("AGENT_ROOM_ADMINS")),
		SecretKey:     envString("AGENT_ROOM_SECRET_KEY", ""),
		Claude: ClaudeConfig{
			Command:    envString("AGENT_ROOM_CLAUDE_COMMAND", "claude"),
			WorkingDir: envString("AGENT_ROOM_CLAUDE_WORKDIR", ""),
			Model:      envString("AGENT_ROOM_CLAUDE_MODEL", ""),
			APIBaseURL: envString("AGENT_ROOM_CLAUDE_API_BASE_URL", ""),
			APIKey:     envString("AGENT_ROOM_CLAUDE_API_KEY", ""),
			Timeout:    envDuration("AGENT_ROOM_CLAUDE_TIMEOUT", 10*time.Minute),
			// 默认 100:executor 委派 + 长采样排查(发命令 → sleep 采样 → 再采样
			// → 聚合分析)一轮就能吃掉十几到几十个回合,24 仍会在写出最终结论前被
			// --max-turns 掐断,房间里表现为 "reply failed"。给足回合,真卡死另有
			// 流式空闲超时(默认 10m)兜底,不靠回合数当安全阀。
			MaxTurns:             envInt("AGENT_ROOM_CLAUDE_MAX_TURNS", 100),
			DisableTools:         envBool("AGENT_ROOM_CLAUDE_DISABLE_TOOLS", false),
			NoSessionPersistence: envBool("AGENT_ROOM_CLAUDE_NO_SESSION_PERSISTENCE", true),
			SkipPermissions:      envBool("AGENT_ROOM_CLAUDE_SKIP_PERMISSIONS", true),
		},
		OpenCode: OpenCodeConfig{
			ServerURL:       envString("AGENT_ROOM_OPENCODE_SERVER", ""),
			ProviderID:      envString("AGENT_ROOM_OPENCODE_PROVIDER_ID", ""),
			ModelID:         envString("AGENT_ROOM_OPENCODE_MODEL_ID", ""),
			Timeout:         envDuration("AGENT_ROOM_OPENCODE_TIMEOUT", 10*time.Minute),
			SkipPermissions: envBool("AGENT_ROOM_OPENCODE_SKIP_PERMISSIONS", false),
		},
		Exec: ExecConfig{
			Token:                envString("AGENT_ROOM_EXEC_TOKEN", ""),
			AllowUnauthenticated: envBool("AGENT_ROOM_EXEC_ALLOW_UNAUTHENTICATED", false),
			WorkingDir:           envString("AGENT_ROOM_EXEC_WORKDIR", ""),
			Timeout:              envDuration("AGENT_ROOM_EXEC_TIMEOUT", 30*time.Second),
			MaxOutputBytes:       envInt("AGENT_ROOM_EXEC_MAX_OUTPUT_BYTES", 64*1024),
			Shell:                envString("AGENT_ROOM_EXEC_SHELL", ""),
		},
		GitHub: GitHubConfig{
			ClientID:      envString("GITHUB_OAUTH_CLIENT_ID", ""),
			ClientSecret:  envString("GITHUB_OAUTH_CLIENT_SECRET", ""),
			RedirectURI:   envString("GITHUB_OAUTH_REDIRECT_URI", ""),
			SessionSecret: envString("AGENT_ROOM_SESSION_SECRET", ""),
			CookieName:    envString("AGENT_ROOM_COOKIE_NAME", "agent_room_session"),
		},
		SSO: ExternalSSOConfig{
			AppID:        envString("AGENT_ROOM_SSO_APP_ID", ""),
			AppSecret:    envString("AGENT_ROOM_SSO_APP_SECRET", ""),
			AuthorizeURL: envString("AGENT_ROOM_SSO_AUTHORIZE_URL", ""),
		},
		LLM: LLMConfig{
			APIKey:          envString("LLM_API_KEY", ""),
			BaseURL:         envString("LLM_API_BASE_URL", ""),
			Model:           envString("LLM_API_MODEL", ""),
			SummaryEvery:    envInt("AGENT_ROOM_SUMMARY_EVERY", 20),
			SummaryInterval: envDuration("AGENT_ROOM_SUMMARY_INTERVAL", 5*time.Minute),
		},
	}
}

// SplitList parses a comma/space/semicolon separated list into trimmed,
// non-empty entries. Used for AGENT_ROOM_ADMINS and the relay -admins flag so
// both env and flag accept the same forgiving syntax.
func SplitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func envString(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func envBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return b
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return d
}
