package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agent-room/internal/models"
)

const (
	agentJoinCodeDefaultTTL = 10 * time.Minute
	agentJoinCodeMaxTTL     = time.Hour
	agentJoinTokenTTL       = 12 * time.Hour
)

var (
	errAgentJoinCodeInvalid = errors.New("invalid join code")
	errAgentJoinCodeExpired = errors.New("join code expired")
)

type agentJoinCode struct {
	ID        string
	RoomID    string
	CodeHash  string
	IssuedBy  string
	Note      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

func (s *Server) handleAgentJoinCodes(w http.ResponseWriter, r *http.Request, roomID, action string) {
	switch {
	case action == "" && r.Method == http.MethodPost:
		s.createAgentJoinCode(w, r, roomID)
	case action == "redeem" && r.Method == http.MethodPost:
		s.redeemAgentJoinCode(w, r, roomID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) createAgentJoinCode(w http.ResponseWriter, r *http.Request, roomID string) {
	login, ok := s.requireOwner(w, r, roomID)
	if !ok {
		return
	}
	if s.agents == nil {
		writeError(w, http.StatusInternalServerError, "agent storage not configured")
		return
	}

	var body struct {
		Note       string `json:"note"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	ttl := agentJoinCodeDefaultTTL
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	if ttl > agentJoinCodeMaxTTL {
		ttl = agentJoinCodeMaxTTL
	}

	code, err := randomToken(18)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate join code failed")
		return
	}
	now := time.Now().UTC()
	rec := agentJoinCode{
		ID:        "join_" + shortRandom(8),
		RoomID:    roomID,
		CodeHash:  hashAgentToken(code),
		IssuedBy:  login,
		Note:      strings.TrimSpace(body.Note),
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	s.storeAgentJoinCode(rec)

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          rec.ID,
		"room_id":     roomID,
		"code":        code,
		"expires_at":  rec.ExpiresAt,
		"ttl_seconds": int(ttl.Seconds()),
		"note":        rec.Note,
	})
}

func (s *Server) redeemAgentJoinCode(w http.ResponseWriter, r *http.Request, roomID string) {
	if s.agents == nil {
		writeError(w, http.StatusInternalServerError, "agent storage not configured")
		return
	}
	if s.rooms == nil {
		writeError(w, http.StatusInternalServerError, "room storage not configured")
		return
	}

	var body struct {
		Code       string `json:"code"`
		AgentID    string `json:"agent_id"`
		AgentLabel string `json:"agent_label"`
		Provider   string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	code := strings.TrimSpace(body.Code)
	agentID := strings.TrimSpace(body.AgentID)
	if code == "" {
		writeError(w, http.StatusBadRequest, "code required")
		return
	}
	if !validAgentJoinID(agentID) {
		writeError(w, http.StatusBadRequest, "agent_id required")
		return
	}

	rec, err := s.consumeAgentJoinCode(roomID, code)
	if err != nil {
		switch {
		case errors.Is(err, errAgentJoinCodeExpired):
			writeError(w, http.StatusGone, err.Error())
		default:
			writeError(w, http.StatusForbidden, errAgentJoinCodeInvalid.Error())
		}
		return
	}

	room, err := s.rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "room not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if room.Ended {
		writeError(w, http.StatusConflict, "room has ended")
		return
	}

	token, err := randomToken(agentTokenBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate agent token failed")
		return
	}
	now := time.Now().UTC()
	tokenExpiresAt := now.Add(agentJoinTokenTTL)
	note := "temporary room join: " + roomID
	if rec.Note != "" {
		note += " · " + rec.Note
	}
	if err := s.agents.InsertAgentToken(r.Context(), models.AgentToken{
		TokenHash:  hashAgentToken(token),
		OwnerLogin: rec.IssuedBy,
		Note:       note,
		CreatedAt:  now,
		ExpiresAt:  &tokenExpiresAt,
		RoomID:     roomID,
		Temporary:  true,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	label := strings.TrimSpace(body.AgentLabel)
	if label == "" {
		label = agentID
	}
	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		provider = "codex-skill"
	}
	if err := s.agents.UpsertAgent(r.Context(), models.Agent{
		AgentID:    agentID,
		OwnerLogin: rec.IssuedBy,
		Label:      label,
		Provider:   provider,
		LastSeenAt: now,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.agents.AddAgentRoom(r.Context(), agentID, roomID, "join-code:"+rec.IssuedBy); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	msg := models.ChatMessage{
		Type:       models.MessageTypeControl,
		SenderKind: models.SenderKindSystem,
		TargetID:   agentID,
		CreatedAt:  time.Now().UTC(),
		Metadata: map[string]string{
			"operation": models.ControlOperationJoinRoom,
			"room_id":   roomID,
		},
	}
	delivered := s.hub.sendToAgentCtrl(agentID, msg)

	relay := s.publicBaseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"room_id":          roomID,
		"agent_id":         agentID,
		"agent_token":      token,
		"token_expires_at": tokenExpiresAt,
		"relay":            relay,
		"relay_ws":         httpBaseToWS(relay),
		"delivered":        delivered,
		"command":          fmt.Sprintf("agent-room bridge -relay %s -agent-token %s -agent-id %s", relay, token, agentID),
	})
}

func (s *Server) storeAgentJoinCode(rec agentJoinCode) {
	now := time.Now().UTC()
	s.agentJoinCodesMu.Lock()
	defer s.agentJoinCodesMu.Unlock()
	for hash, existing := range s.agentJoinCodes {
		if !existing.ExpiresAt.After(now) {
			delete(s.agentJoinCodes, hash)
		}
	}
	s.agentJoinCodes[rec.CodeHash] = rec
}

func (s *Server) consumeAgentJoinCode(roomID, code string) (agentJoinCode, error) {
	hash := hashAgentToken(strings.TrimSpace(code))
	now := time.Now().UTC()
	s.agentJoinCodesMu.Lock()
	defer s.agentJoinCodesMu.Unlock()
	rec, ok := s.agentJoinCodes[hash]
	if !ok || rec.RoomID != roomID {
		return agentJoinCode{}, errAgentJoinCodeInvalid
	}
	delete(s.agentJoinCodes, hash)
	if !rec.ExpiresAt.After(now) {
		return agentJoinCode{}, errAgentJoinCodeExpired
	}
	return rec, nil
}

func validAgentJoinID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func httpBaseToWS(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
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
