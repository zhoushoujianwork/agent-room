package relay

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agent-room/internal/models"
)

// agentConfigResponse is the GET payload: never the ciphertext or plaintext
// key, only a masked rendering so an owner can confirm which key is set.
type agentConfigResponse struct {
	Model             string    `json:"model"`
	APIBaseURL        string    `json:"api_base_url"`
	APIKeyMasked      string    `json:"api_key_masked"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
	UpdatedBy         string    `json:"updated_by,omitempty"`
	RuntimeProvider   string    `json:"runtime_provider,omitempty"`
	RuntimeModel      string    `json:"runtime_model,omitempty"`
	RuntimeAPIBaseURL string    `json:"runtime_api_base_url,omitempty"`
	RuntimeAPIKeySet  bool      `json:"runtime_api_key_set,omitempty"`
	RuntimeUpdatedAt  time.Time `json:"runtime_updated_at,omitempty"`
}

// agentConfigUpdate is the PUT body. Pointer fields distinguish "absent (keep
// current)" from "present empty string (clear)". api_key present+non-empty
// overwrites; present+empty clears; absent keeps.
// expect_updated_at is an optional optimistic-lock guard: when present, the
// request is rejected with 409 if the stored updated_at does not match,
// preventing silent last-write-wins overwrites in concurrent-edit scenarios.
type agentConfigUpdate struct {
	Model           *string    `json:"model"`
	APIBaseURL      *string    `json:"api_base_url"`
	APIKey          *string    `json:"api_key"`
	ExpectUpdatedAt *time.Time `json:"expect_updated_at,omitempty"`
}

type agentRuntimeConfig struct {
	Provider   string
	Model      string
	APIBaseURL string
	APIKeySet  bool
	UpdatedAt  time.Time
}

// handleAgentConfig serves GET/PUT /v1/agents/{agent_id}/config. Owner-or-admin
// only; the agent must exist (be bound to an owner) or we 404. Dispatched from
// handleAgentItem when the path carries a /config suffix.
func (s *Server) handleAgentConfig(w http.ResponseWriter, r *http.Request, agentID string) {
	login := s.requireSession(w, r)
	if login == "" {
		return
	}
	if s.agents == nil {
		writeError(w, http.StatusInternalServerError, "agent storage not configured")
		return
	}
	agent, err := s.agents.GetAgent(r.Context(), agentID)
	if err != nil || agent == nil {
		// Unbound / unknown agents have no owner to authorize against.
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if agent.OwnerLogin != login && !s.isAdminRequest(r) {
		writeError(w, http.StatusForbidden, "not your agent")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getAgentConfig(w, r, agentID)
	case http.MethodPut:
		s.putAgentConfig(w, r, agentID, login)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) getAgentConfig(w http.ResponseWriter, r *http.Request, agentID string) {
	cfg, err := s.agents.GetAgentConfig(r.Context(), agentID)
	if err != nil {
		if isNotFound(err) {
			// No config saved yet: return an empty, non-error shape.
			writeJSON(w, http.StatusOK, s.agentConfigResponse(agentID, nil))
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.agentConfigResponse(agentID, cfg))
}

func (s *Server) agentConfigResponse(agentID string, cfg *models.AgentConfig) agentConfigResponse {
	masked := ""
	resp := agentConfigResponse{}
	if cfg != nil && strings.TrimSpace(cfg.APIKeyCipher) != "" {
		// Decrypt only to mask; the plaintext never leaves this function.
		if plain, derr := decryptSecret(s.hub.secretKey, cfg.APIKeyCipher); derr == nil {
			masked = maskAPIKey(plain)
		} else {
			masked = "***"
		}
	}
	if cfg != nil {
		resp.Model = cfg.Model
		resp.APIBaseURL = cfg.APIBaseURL
		resp.APIKeyMasked = masked
		resp.UpdatedAt = cfg.UpdatedAt
		resp.UpdatedBy = cfg.UpdatedBy
	}
	if runtimeCfg, ok := s.hub.agentRuntimeConfig(agentID); ok {
		resp.RuntimeProvider = runtimeCfg.Provider
		resp.RuntimeModel = runtimeCfg.Model
		resp.RuntimeAPIBaseURL = runtimeCfg.APIBaseURL
		resp.RuntimeAPIKeySet = runtimeCfg.APIKeySet
		resp.RuntimeUpdatedAt = runtimeCfg.UpdatedAt
	}
	return resp
}

func (s *Server) putAgentConfig(w http.ResponseWriter, r *http.Request, agentID, login string) {
	var body agentConfigUpdate
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}

	// Start from the current row so omitted fields are preserved.
	current, err := s.agents.GetAgentConfig(r.Context(), agentID)
	if err != nil && !isNotFound(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Optimistic-lock guard: if the caller supplied expect_updated_at, reject
	// the write when the stored timestamp doesn't match. This prevents a
	// concurrent PUT from silently overwriting another writer's changes.
	if body.ExpectUpdatedAt != nil {
		var storedAt time.Time
		if current != nil {
			storedAt = current.UpdatedAt
		}
		if !storedAt.Equal(*body.ExpectUpdatedAt) {
			writeError(w, http.StatusConflict, "config was modified by another request; fetch the latest version and retry")
			return
		}
	}

	next := models.AgentConfig{AgentID: agentID}
	if current != nil {
		next.Model = current.Model
		next.APIBaseURL = current.APIBaseURL
		next.APIKeyCipher = current.APIKeyCipher
	}

	if body.Model != nil {
		next.Model = strings.TrimSpace(*body.Model)
	}
	if body.APIBaseURL != nil {
		next.APIBaseURL = strings.TrimSpace(*body.APIBaseURL)
	}
	if body.APIKey != nil {
		key := strings.TrimSpace(*body.APIKey)
		if key == "" {
			next.APIKeyCipher = "" // explicit clear
		} else {
			cipher, cerr := encryptSecret(s.hub.secretKey, key)
			if cerr != nil {
				if errors.Is(cerr, errSecretKeyUnset) {
					// model / api_base_url still saved below would be confusing
					// on a partial failure; reject the whole api_key change but
					// let a caller retry without it. Per criterion 6 the message
					// must be explicit.
					writeError(w, http.StatusBadRequest, cerr.Error())
					return
				}
				writeError(w, http.StatusInternalServerError, "encrypt api_key failed")
				return
			}
			next.APIKeyCipher = cipher
		}
	}
	next.UpdatedAt = time.Now().UTC()
	next.UpdatedBy = login

	if err := s.agents.UpsertAgentConfig(r.Context(), next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Push to the agent if it is online; offline agents pick the config up on
	// their next presence/bind. Failure to deliver is non-fatal.
	s.hub.sendConfigUpdate(agentID, next)

	// Echo back the masked view (same shape as GET) so the client can refresh.
	writeJSON(w, http.StatusOK, s.agentConfigResponse(agentID, &next))
}

func (h *hub) updateAgentRuntimeConfig(agentID string, metadata map[string]string) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return
	}
	cfg := agentRuntimeConfig{
		Provider:   strings.TrimSpace(metadata["provider"]),
		Model:      strings.TrimSpace(metadata["model"]),
		APIBaseURL: strings.TrimSpace(metadata["api_base_url"]),
		APIKeySet:  strings.EqualFold(strings.TrimSpace(metadata["api_key_set"]), "true"),
		UpdatedAt:  time.Now().UTC(),
	}
	h.mu.Lock()
	h.agentRuntime[agentID] = cfg
	h.mu.Unlock()
	h.logger.Info("agent config report received",
		slog.String("agent_id", agentID),
		slog.String("provider", cfg.Provider),
		slog.String("model", cfg.Model),
		slog.String("api_base_url", cfg.APIBaseURL),
		slog.Bool("api_key_set", cfg.APIKeySet))
}

func (h *hub) agentRuntimeConfig(agentID string) (agentRuntimeConfig, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	cfg, ok := h.agentRuntime[agentID]
	return cfg, ok
}

// --- downstream delivery (relay -> bridge) ---

// pushAgentConfig delivers the stored config (if any) to a single freshly
// bound connection. Called from bindAgentOwner. No-op when no config exists.
func (h *hub) pushAgentConfig(ctx context.Context, c *client, agentID string) {
	if h.agents == nil {
		return
	}
	cfg, err := h.agents.GetAgentConfig(ctx, agentID)
	if err != nil {
		if !isNotFound(err) {
			h.logger.Warn("load agent config for downstream failed",
				slog.String("agent_id", agentID), slog.Any("error", err))
		}
		return
	}
	h.sendConfigUpdateToClient(c, agentID, *cfg)
}

// sendConfigUpdate delivers cfg to agentID. It first tries the agent-level
// control connection; if none is available it falls back to every live room
// connection for that agent. Neither path goes through Publish or history.
func (h *hub) sendConfigUpdate(agentID string, cfg models.AgentConfig) {
	// Build the message once so both paths share the same payload.
	apiKey := ""
	if strings.TrimSpace(cfg.APIKeyCipher) != "" {
		plain, err := decryptSecret(h.secretKey, cfg.APIKeyCipher)
		if err != nil {
			h.logger.Warn("decrypt agent api_key for downstream failed",
				slog.String("agent_id", agentID), slog.Any("error", err))
		} else {
			apiKey = plain
		}
	}
	meta := map[string]string{
		"operation":    models.ControlOperationConfigUpdate,
		"model":        cfg.Model,
		"api_base_url": cfg.APIBaseURL,
	}
	if apiKey != "" {
		meta["api_key"] = apiKey
	}
	msg := models.ChatMessage{
		Type:       models.MessageTypeControl,
		SenderKind: models.SenderKindSystem,
		TargetID:   agentID,
		CreatedAt:  time.Now().UTC(),
		Metadata:   meta,
	}
	// Prefer control connection; fall back to room connections.
	if h.sendToAgentCtrl(agentID, msg) {
		h.logger.Info("config_update delivered via ctrl",
			slog.String("agent_id", agentID),
			slog.String("model", cfg.Model))
		return
	}
	// Fallback: deliver over every room connection.
	h.mu.RLock()
	var targets []*client
	for _, clients := range h.rooms {
		for c := range clients {
			if c.audit {
				continue
			}
			if c.participant.Kind == models.SenderKindAgent && c.participant.ID == agentID {
				targets = append(targets, c)
			}
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		if c.tokenTemporary {
			continue
		}
		h.sendConfigUpdateToClient(c, agentID, cfg)
	}
}

// sendConfigUpdateToClient builds the config_update control message (decrypting
// the api key into the targeted channel only) and pushes it onto one client's
// send queue. It NEVER goes through Publish, so it is neither persisted to room
// history nor broadcast to other participants. The api key is never logged.
func (h *hub) sendConfigUpdateToClient(c *client, agentID string, cfg models.AgentConfig) {
	if c.tokenTemporary {
		return
	}
	apiKey := ""
	if strings.TrimSpace(cfg.APIKeyCipher) != "" {
		plain, err := decryptSecret(h.secretKey, cfg.APIKeyCipher)
		if err != nil {
			h.logger.Warn("decrypt agent api_key for downstream failed",
				slog.String("agent_id", agentID), slog.Any("error", err))
			// Deliver model/base_url anyway; the key just stays at local default.
		} else {
			apiKey = plain
		}
	}
	meta := map[string]string{
		"operation":    models.ControlOperationConfigUpdate,
		"model":        cfg.Model,
		"api_base_url": cfg.APIBaseURL,
	}
	if apiKey != "" {
		meta["api_key"] = apiKey
	}
	msg := models.ChatMessage{
		RoomID:     c.roomID,
		Type:       models.MessageTypeControl,
		SenderKind: models.SenderKindSystem,
		TargetID:   agentID,
		CreatedAt:  time.Now().UTC(),
		Metadata:   meta,
	}
	select {
	case c.send <- outgoing(msg):
		h.logger.Info("config_update delivered",
			slog.String("agent_id", agentID),
			slog.String("room_id", c.roomID),
			slog.String("model", cfg.Model),
			slog.String("api_base_url", cfg.APIBaseURL),
			slog.Bool("api_key_set", apiKey != ""))
	default:
		h.logger.Warn("dropping config_update for slow client",
			slog.String("agent_id", agentID), slog.String("room_id", c.roomID))
	}
}

// sendToAgentCtrl delivers msg to all control connections for agentID.
// Non-blocking per connection; returns true if at least one delivery succeeded.
// Messages sent via this path NEVER go through Publish and NEVER enter history.
func (h *hub) sendToAgentCtrl(agentID string, msg models.ChatMessage) bool {
	h.mu.RLock()
	set := h.agentCtrl[agentID]
	targets := make([]*client, 0, len(set))
	for c := range set {
		targets = append(targets, c)
	}
	h.mu.RUnlock()
	delivered := false
	for _, c := range targets {
		if !c.allowsControlMessage(msg) {
			continue
		}
		select {
		case c.send <- outgoing(msg):
			delivered = true
		default:
			h.logger.Warn("dropping ctrl msg for slow client", slog.String("agent_id", agentID))
		}
	}
	return delivered
}

func (c *client) allowsControlMessage(msg models.ChatMessage) bool {
	if c == nil || c.tokenRoomID == "" || msg.Type != models.MessageTypeControl {
		return true
	}
	switch strings.TrimSpace(msg.Metadata["operation"]) {
	case models.ControlOperationConfigUpdate:
		return !c.tokenTemporary
	case models.ControlOperationJoinRoom, models.ControlOperationLeaveRoom:
		return strings.TrimSpace(msg.Metadata["room_id"]) == c.tokenRoomID
	default:
		return true
	}
}

// closeAgentConnections closes every live connection whose participant ID
// matches agentID. Used when an agent is unbound (DELETE /v1/agents/{id}) so
// the bridge is kicked immediately rather than lingering until natural
// reconnect. The bridge's own reconnect logic will then re-authenticate and
// receive a 401 / anonymous downgrade as appropriate.
func (h *hub) closeAgentConnections(agentID string) {
	h.mu.RLock()
	var targets []*client
	for _, clients := range h.rooms {
		for c := range clients {
			if c.audit {
				continue
			}
			if c.participant.Kind == models.SenderKindAgent && c.participant.ID == agentID {
				targets = append(targets, c)
			}
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		h.logger.Info("closing agent connection: agent unbound",
			slog.String("agent_id", agentID),
			slog.String("room_id", c.roomID))
		_ = c.conn.Close()
	}
}

// closeOwnerConnections closes every live agent connection whose resolved
// owner matches ownerLogin. Used when a token is revoked so any bridge that
// authenticated with that owner's token is kicked immediately.
func (h *hub) closeOwnerConnections(ownerLogin string) {
	h.mu.RLock()
	var targets []*client
	for _, clients := range h.rooms {
		for c := range clients {
			if c.audit {
				continue
			}
			if c.participant.Kind == models.SenderKindAgent && c.owner == ownerLogin {
				targets = append(targets, c)
			}
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		h.logger.Info("closing agent connection: token revoked",
			slog.String("owner", ownerLogin),
			slog.String("agent_id", c.participant.ID),
			slog.String("room_id", c.roomID))
		_ = c.conn.Close()
	}
}
