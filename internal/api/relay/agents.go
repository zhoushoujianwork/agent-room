package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"agent-room/internal/models"
)

// AgentStore is the optional persistence capability backing agent ownership:
// the binding between an agent id and the user (owner_login) whose token it
// connected with, plus the long-lived agent tokens themselves. Both the SQLite
// and in-memory stores implement it; NewServer type-asserts the room store to
// discover it, so legacy/test stores without agent support simply leave the
// feature off (token-carrying connections are then treated as anonymous and
// the /v1/agents* endpoints 500 with a clear message).
type AgentStore interface {
	UpsertAgent(ctx context.Context, agent models.Agent) error
	GetAgent(ctx context.Context, agentID string) (*models.Agent, error)
	ListAgentsByOwner(ctx context.Context, owner string) ([]models.Agent, error)
	ListAllAgents(ctx context.Context) ([]models.Agent, error)
	RevokeAgent(ctx context.Context, agentID string) error

	InsertAgentToken(ctx context.Context, token models.AgentToken) error
	LookupAgentToken(ctx context.Context, hash string) (*models.AgentToken, error)
	TouchAgentTokenLastUsed(ctx context.Context, hash string, t time.Time) error
	ListAgentTokensByOwner(ctx context.Context, owner string) ([]models.AgentToken, error)
	RevokeAgentTokenByPrefix(ctx context.Context, owner, prefix string) (int, error)

	GetAgentConfig(ctx context.Context, agentID string) (*models.AgentConfig, error)
	UpsertAgentConfig(ctx context.Context, cfg models.AgentConfig) error

	AddAgentRoom(ctx context.Context, agentID, roomID, addedBy string) error
	RemoveAgentRoom(ctx context.Context, agentID, roomID string) error
	ListAgentRooms(ctx context.Context, agentID string) ([]string, error)
}

// agentTokenBytes is the random-token size in bytes before base64url encoding.
const agentTokenBytes = 32

// hashAgentToken returns the hex SHA-256 of a plaintext agent token. This is
// the only form ever persisted; the plaintext is shown once at creation and
// never stored.
func hashAgentToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// validateAgentToken resolves a plaintext agent token to its owner login. It
// returns ("", false) when the feature is unavailable, the token is unknown, or
// the token has been revoked. On success it bumps last_used_at. Callers that
// receive a non-empty token but get ok=false should reject the connection;
// callers with no token at all must not call this (anonymous stays allowed).
func (s *Server) validateAgentToken(ctx context.Context, token string) (string, bool) {
	resolved, ok := s.resolveAgentToken(ctx, token)
	if !ok {
		return "", false
	}
	return resolved.OwnerLogin, true
}

type validatedAgentToken struct {
	OwnerLogin string
	RoomID     string
	ExpiresAt  *time.Time
	Temporary  bool
}

func (s *Server) resolveAgentToken(ctx context.Context, token string) (validatedAgentToken, bool) {
	if s.agents == nil {
		return validatedAgentToken{}, false
	}
	hash := hashAgentToken(token)
	rec, err := s.agents.LookupAgentToken(ctx, hash)
	if err != nil || rec == nil || rec.Revoked {
		return validatedAgentToken{}, false
	}
	if rec.ExpiresAt != nil && !rec.ExpiresAt.After(time.Now().UTC()) {
		return validatedAgentToken{}, false
	}
	owner := strings.TrimSpace(rec.OwnerLogin)
	if owner == "" {
		return validatedAgentToken{}, false
	}
	if err := s.agents.TouchAgentTokenLastUsed(ctx, hash, time.Now().UTC()); err != nil {
		s.logger.Warn("touch agent token last_used failed", slog.Any("error", err))
	}
	return validatedAgentToken{
		OwnerLogin: owner,
		RoomID:     strings.TrimSpace(rec.RoomID),
		ExpiresAt:  rec.ExpiresAt,
		Temporary:  rec.Temporary,
	}, true
}

// requireSession returns the authenticated login or writes a 403 and returns
// "". Used by every /v1/agents* endpoint: these are user-scoped and always
// require a signed-in session.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) string {
	if !s.authEnabled() {
		writeError(w, http.StatusNotFound, "auth disabled")
		return ""
	}
	login := s.loginFromRequest(r)
	if login == "" {
		writeError(w, http.StatusForbidden, "sign in required")
		return ""
	}
	return login
}

// handleAgentsList serves GET /v1/agents — the caller's own agents (online
// status merged from hub presence). Admins see every bound agent.
func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	login := s.requireSession(w, r)
	if login == "" {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.agents == nil {
		writeError(w, http.StatusInternalServerError, "agent storage not configured")
		return
	}
	var (
		list []models.Agent
		err  error
	)
	if s.isAdminRequest(r) {
		list, err = s.agents.ListAllAgents(r.Context())
	} else {
		list, err = s.agents.ListAgentsByOwner(r.Context(), login)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	presence := s.hub.AgentPresence()
	for i := range list {
		if p, ok := presence[list[i].AgentID]; ok {
			list[i].Online = p.Online
			list[i].Rooms = p.Rooms
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": list})
}

// handleAgentItem serves DELETE /v1/agents/{agent_id} — unbind (revoke) one of
// the caller's agents. Room/admin scoping: only the owner or an admin may
// unbind. A revoked agent reconnects as anonymous next time.
func (s *Server) handleAgentItem(w http.ResponseWriter, r *http.Request) {
	login := s.requireSession(w, r)
	if login == "" {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/agents/"), "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "unknown route")
		return
	}
	// /v1/agents/{agent_id}/config is the per-agent startup config endpoint.
	if agentID, ok := strings.CutSuffix(rest, "/config"); ok {
		if agentID == "" || strings.Contains(agentID, "/") {
			writeError(w, http.StatusNotFound, "unknown route")
			return
		}
		s.handleAgentConfig(w, r, agentID)
		return
	}
	agentID := rest
	if strings.Contains(agentID, "/") {
		writeError(w, http.StatusNotFound, "unknown route")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.agents == nil {
		writeError(w, http.StatusInternalServerError, "agent storage not configured")
		return
	}
	agent, err := s.agents.GetAgent(r.Context(), agentID)
	if err != nil || agent == nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if agent.OwnerLogin != login && !s.isAdminRequest(r) {
		writeError(w, http.StatusForbidden, "not your agent")
		return
	}
	if err := s.agents.RevokeAgent(r.Context(), agentID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Kick any live connections for this agent so the bridge re-authenticates
	// immediately rather than staying online until natural disconnect.
	s.hub.closeAgentConnections(agentID)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// tokenResponse is a token row safe to return: no plaintext, no full hash.
type tokenResponse struct {
	HashPrefix string     `json:"hash_prefix"`
	Note       string     `json:"note"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RoomID     string     `json:"room_id,omitempty"`
	Temporary  bool       `json:"temporary,omitempty"`
}

// handleAgentTokens serves POST (generate) and GET (list) /v1/agents/tokens.
func (s *Server) handleAgentTokens(w http.ResponseWriter, r *http.Request) {
	login := s.requireSession(w, r)
	if login == "" {
		return
	}
	if s.agents == nil {
		writeError(w, http.StatusInternalServerError, "agent storage not configured")
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.createAgentToken(w, r, login)
	case http.MethodGet:
		s.listAgentTokens(w, r, login)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) createAgentToken(w http.ResponseWriter, r *http.Request, login string) {
	var body struct {
		Note string `json:"note"`
	}
	// Body is optional; ignore decode errors on an empty/garbage body so a
	// note-less POST still works.
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	plaintext, err := randomToken(agentTokenBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate token failed")
		return
	}
	hash := hashAgentToken(plaintext)
	now := time.Now().UTC()
	rec := models.AgentToken{
		TokenHash:  hash,
		HashPrefix: tokenHashPrefix(hash),
		OwnerLogin: login,
		Note:       strings.TrimSpace(body.Note),
		CreatedAt:  now,
	}
	if err := s.agents.InsertAgentToken(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The plaintext is returned exactly once, here. It is never logged or
	// persisted in plaintext form.
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":       plaintext,
		"hash_prefix": rec.HashPrefix,
		"note":        rec.Note,
		"created_at":  now,
	})
}

func (s *Server) listAgentTokens(w http.ResponseWriter, r *http.Request, login string) {
	tokens, err := s.agents.ListAgentTokensByOwner(r.Context(), login)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]tokenResponse, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, tokenResponse{
			HashPrefix: t.HashPrefix,
			Note:       t.Note,
			CreatedAt:  t.CreatedAt,
			LastUsedAt: t.LastUsedAt,
			ExpiresAt:  t.ExpiresAt,
			RoomID:     t.RoomID,
			Temporary:  t.Temporary,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// handleAgentTokenItem serves DELETE /v1/agents/tokens/{hash_prefix} — revoke a
// token by its public prefix, scoped to the caller so one user can never revoke
// another's token.
func (s *Server) handleAgentTokenItem(w http.ResponseWriter, r *http.Request) {
	login := s.requireSession(w, r)
	if login == "" {
		return
	}
	prefix := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/agents/tokens/"), "/")
	if prefix == "" || strings.Contains(prefix, "/") {
		writeError(w, http.StatusNotFound, "unknown route")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.agents == nil {
		writeError(w, http.StatusInternalServerError, "agent storage not configured")
		return
	}
	n, err := s.agents.RevokeAgentTokenByPrefix(r.Context(), login, strings.ToLower(prefix))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch {
	case n == 0:
		writeError(w, http.StatusNotFound, "token not found")
	case n > 1:
		// Ambiguous prefix matched multiple tokens; nothing was revoked.
		// The caller should retry with a longer prefix.
		writeError(w, http.StatusConflict, "ambiguous hash prefix matched multiple tokens (nothing revoked; use a longer prefix)")
	default:
		// Token revoked: kick any live connections that authenticated with
		// this owner's token so they re-authenticate immediately.
		s.hub.closeOwnerConnections(login)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// agentPresence is the runtime online view for a single agent id, merged from
// the hub's live room registrations.
type agentPresence struct {
	Online bool
	Rooms  []string
}

// AgentPresence returns, per agent id, whether it currently has any live
// connection and which rooms it is in. Mirrors UserPresence but keyed on agent
// participants. Used by GET /v1/agents to merge online state onto the durable
// rows.
func (h *hub) AgentPresence() map[string]agentPresence {
	h.mu.RLock()
	defer h.mu.RUnlock()
	roomSets := make(map[string]map[string]struct{})
	online := make(map[string]struct{})
	for agentID := range h.agentCtrl {
		if strings.TrimSpace(agentID) != "" {
			online[agentID] = struct{}{}
		}
	}
	for roomID, clients := range h.rooms {
		for c := range clients {
			if c.audit {
				continue
			}
			p := c.participant
			if p.Kind != models.SenderKindAgent {
				continue
			}
			id := strings.TrimSpace(p.ID)
			if id == "" {
				continue
			}
			online[id] = struct{}{}
			if roomSets[id] == nil {
				roomSets[id] = make(map[string]struct{})
			}
			roomSets[id][roomID] = struct{}{}
		}
	}
	out := make(map[string]agentPresence, len(online))
	for id := range online {
		rooms := roomSets[id]
		list := make([]string, 0, len(rooms))
		for roomID := range rooms {
			list = append(list, roomID)
		}
		sort.Strings(list)
		out[id] = agentPresence{Online: true, Rooms: list}
	}
	return out
}

// tokenHashPrefix mirrors the SQLite helper so the relay can compute a prefix
// without a store round-trip (used when constructing the create response).
func tokenHashPrefix(hash string) string {
	const n = 12
	if len(hash) <= n {
		return hash
	}
	return hash[:n]
}
