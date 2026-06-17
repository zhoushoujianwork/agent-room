package models

import "time"

// Agent is the durable binding between a local bridge agent and the GitHub
// user (owner_login) who generated the token it connected with. Agents are
// otherwise transient (they exist only while a bridge holds a websocket);
// this row lets the owner see and manage them across restarts and while
// offline. Online and Rooms are runtime-only fields merged from hub presence
// and are never persisted.
type Agent struct {
	AgentID    string    `json:"agent_id"`
	OwnerLogin string    `json:"owner_login"`
	Label      string    `json:"label"`
	Provider   string    `json:"provider"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Revoked    bool      `json:"revoked"`

	// Online reports whether at least one live connection for this agent id
	// is currently registered in the hub. Merged at read time.
	Online bool `json:"online"`
	// Rooms lists the room ids this agent is currently connected to (online
	// only). Merged at read time.
	Rooms []string `json:"rooms,omitempty"`
}

// AgentConfig is the server-managed startup configuration for one agent:
// which model it runs and (optionally) a custom API endpoint + key, so an
// owner can steer their agent's `claude -p` invocation centrally instead of
// editing the bridge host. All three runtime fields are optional and may be
// cleared independently; an empty value means "fall back to the bridge's
// local default" rather than "force empty". The API key is only ever stored
// as AES-256-GCM ciphertext (APIKeyCipher) — plaintext lives solely in the
// PUT request body, relay memory, the targeted control message, and bridge
// memory, never on disk or in logs.
type AgentConfig struct {
	AgentID    string `json:"agent_id"`
	Model      string `json:"model"`
	APIBaseURL string `json:"api_base_url"`
	// APIKeyCipher is the base64 AES-256-GCM ciphertext of the API key.
	// Never serialized to clients; the GET endpoint returns a masked form.
	APIKeyCipher string    `json:"-"`
	UpdatedAt    time.Time `json:"updated_at"`
	UpdatedBy    string    `json:"updated_by"`
}

// AgentToken is a long-lived, revocable credential a user generates to bind
// bridges to their account. The server only ever stores the SHA-256 hash of
// the token (hex). The plaintext is shown exactly once, at creation. HashPrefix
// is a short, non-secret handle (first bytes of the hash) used to address a
// token for revocation without exposing the full hash.
type AgentToken struct {
	// TokenHash is the hex SHA-256 of the plaintext token. Never serialized.
	TokenHash  string     `json:"-"`
	HashPrefix string     `json:"hash_prefix"`
	OwnerLogin string     `json:"owner_login"`
	Note       string     `json:"note"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RoomID     string     `json:"room_id,omitempty"`
	Temporary  bool       `json:"temporary,omitempty"`
	Revoked    bool       `json:"revoked"`
}
