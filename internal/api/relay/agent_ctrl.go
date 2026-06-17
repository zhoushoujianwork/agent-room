package relay

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agent-room/internal/models"
	"agent-room/pkg/id"

	"github.com/gorilla/websocket"
)

// handleAgentCtrlWS serves GET /v1/agents/ws — the agent-level control
// WebSocket. Unlike room WebSockets this endpoint is not bound to any room;
// it is used to send join_room/leave_room directives and config_update to
// the bridge, and to receive room_state_report from the bridge.
//
// Auth: agent_token query param is mandatory (no anonymous control connections).
// client_id query param carries the agentID reported by the bridge.
func (s *Server) handleAgentCtrlWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.agents == nil {
		writeError(w, http.StatusInternalServerError, "agent storage not configured")
		return
	}

	token := strings.TrimSpace(r.URL.Query().Get("agent_token"))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "agent_token required")
		return
	}
	resolved, ok := s.resolveAgentToken(r.Context(), token)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid or revoked agent token")
		return
	}
	ownerLogin := resolved.OwnerLogin

	agentID := strings.TrimSpace(r.URL.Query().Get("client_id"))
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "client_id required")
		return
	}

	// Check revocation before upgrading.
	if existing, err := s.agents.GetAgent(r.Context(), agentID); err == nil && existing != nil && existing.Revoked {
		writeError(w, http.StatusUnauthorized, "agent revoked")
		return
	}

	// Upsert agent binding (same semantics as bindAgentOwner).
	label := strings.TrimSpace(r.URL.Query().Get("client_label"))
	if label == "" {
		label = agentID
	}
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	if err := s.agents.UpsertAgent(r.Context(), models.Agent{
		AgentID:    agentID,
		OwnerLogin: ownerLogin,
		Label:      label,
		Provider:   provider,
		LastSeenAt: time.Now().UTC(),
	}); err != nil {
		s.logger.Warn("upsert agent binding failed (ctrl)",
			slog.String("agent_id", agentID), slog.Any("error", err))
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Warn("ctrl ws upgrade failed", slog.String("agent_id", agentID), slog.Any("error", err))
		return
	}

	now := time.Now().UTC()
	connID := id.New("ctrl")
	c := &client{
		hub:  s.hub,
		ctrl: true,
		conn: conn,
		send: make(chan outgoing, 64),
		participant: models.Participant{
			ID:           agentID,
			Kind:         models.SenderKindAgent,
			Label:        label,
			ConnectionID: connID,
			ConnectedAt:  now,
			LastSeenAt:   now,
			Metadata: map[string]string{
				"owner_login":   ownerLogin,
				"connection_id": connID,
			},
		},
		owner:          ownerLogin,
		tokenRoomID:    resolved.RoomID,
		tokenTemporary: resolved.Temporary,
	}

	s.hub.register(c)

	// After registration: push stored config and replay desired rooms.
	if !resolved.Temporary {
		go s.hub.pushAgentConfig(r.Context(), c, agentID)
	}
	go s.hub.replayDesiredRooms(r.Context(), c, agentID)

	go c.writeLoop()
	c.ctrlReadLoop(r.Context(), s)
}

// replayDesiredRooms reads ListAgentRooms and sends join_room for each entry.
func (h *hub) replayDesiredRooms(ctx context.Context, c *client, agentID string) {
	if h.agents == nil {
		return
	}
	rooms, err := h.agents.ListAgentRooms(ctx, agentID)
	if err != nil {
		h.logger.Warn("list agent rooms for replay failed",
			slog.String("agent_id", agentID), slog.Any("error", err))
		return
	}
	for _, roomID := range rooms {
		if c.tokenRoomID != "" && roomID != c.tokenRoomID {
			continue
		}
		h.sendJoinRoom(c, agentID, roomID)
	}
}

// sendJoinRoom sends a join_room directive on a specific control connection.
func (h *hub) sendJoinRoom(c *client, agentID, roomID string) {
	if c.tokenRoomID != "" && roomID != c.tokenRoomID {
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
	select {
	case c.send <- outgoing(msg):
		h.logger.Info("join_room sent", slog.String("agent_id", agentID), slog.String("room_id", roomID))
	default:
		h.logger.Warn("dropping join_room for slow ctrl client",
			slog.String("agent_id", agentID), slog.String("room_id", roomID))
	}
}

// sendLeaveRoom sends a leave_room directive on all control connections for agentID.
func (h *hub) sendLeaveRoom(agentID, roomID string) {
	msg := models.ChatMessage{
		Type:       models.MessageTypeControl,
		SenderKind: models.SenderKindSystem,
		TargetID:   agentID,
		CreatedAt:  time.Now().UTC(),
		Metadata: map[string]string{
			"operation": models.ControlOperationLeaveRoom,
			"room_id":   roomID,
		},
	}
	h.sendToAgentCtrl(agentID, msg)
}

// ctrlReadLoop reads messages from a control connection. It accepts lightweight
// reports from the bridge and never writes them to room history.
func (c *client) ctrlReadLoop(ctx context.Context, s *Server) {
	defer func() {
		c.hub.unregister(c)
		_ = c.conn.Close()
	}()

	_ = c.conn.SetReadDeadline(time.Now().Add(clientPongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(clientPongWait))
	})

	agentID := c.participant.ID

	for {
		var msg models.ChatMessage
		if err := c.conn.ReadJSON(&msg); err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.hub.logger.Debug("ctrl ws read closed",
					slog.String("agent_id", agentID), slog.Any("error", err))
			}
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(clientPongWait))

		op := strings.TrimSpace(msg.Metadata["operation"])
		if op == models.ControlOperationConfigReport {
			c.hub.updateAgentRuntimeConfig(agentID, msg.Metadata)
			continue
		}
		if op != models.ControlOperationRoomStateReport {
			c.hub.logger.Debug("ctrl ignoring non-report message",
				slog.String("agent_id", agentID), slog.String("operation", op))
			continue
		}
		if s.agents == nil {
			continue
		}
		c.hub.reconcileRoomState(ctx, s.agents, agentID, msg.Metadata["rooms"], c.tokenRoomID)
	}
}

// reconcileRoomState compares actual (bridge-reported) vs desired (DB) room sets
// and issues the necessary join_room directives or absorbs unknown rooms.
//
// Rules (from issue spec §6):
//   - desired has room, actual doesn't → send join_room
//   - actual has room, desired doesn't → AddAgentRoom("bridge-local") — absorb, no leave_room
func (h *hub) reconcileRoomState(ctx context.Context, agents AgentStore, agentID, roomsStr, allowedRoomID string) {
	// Parse actual rooms from comma-separated list.
	actual := make(map[string]struct{})
	for _, r := range strings.Split(roomsStr, ",") {
		r = strings.TrimSpace(r)
		if r != "" {
			if allowedRoomID != "" && r != allowedRoomID {
				continue
			}
			actual[r] = struct{}{}
		}
	}

	// Load desired rooms from DB.
	desired, err := agents.ListAgentRooms(ctx, agentID)
	if err != nil {
		h.logger.Warn("list agent rooms for reconcile failed",
			slog.String("agent_id", agentID), slog.Any("error", err))
		return
	}
	desiredSet := make(map[string]struct{}, len(desired))
	for _, r := range desired {
		if allowedRoomID != "" && r != allowedRoomID {
			continue
		}
		desiredSet[r] = struct{}{}
	}

	// Desired has room but actual doesn't → send join_room.
	h.mu.RLock()
	ctrlSet := h.agentCtrl[agentID]
	ctrlClients := make([]*client, 0, len(ctrlSet))
	for c := range ctrlSet {
		ctrlClients = append(ctrlClients, c)
	}
	h.mu.RUnlock()

	for _, roomID := range desired {
		if allowedRoomID != "" && roomID != allowedRoomID {
			continue
		}
		if _, inActual := actual[roomID]; !inActual {
			for _, c := range ctrlClients {
				h.sendJoinRoom(c, agentID, roomID)
			}
		}
	}

	// Actual has room but desired doesn't → absorb into desired (no leave_room).
	for roomID := range actual {
		if _, inDesired := desiredSet[roomID]; !inDesired {
			if err := agents.AddAgentRoom(ctx, agentID, roomID, "bridge-local"); err != nil {
				h.logger.Warn("absorb bridge-local room failed",
					slog.String("agent_id", agentID), slog.String("room_id", roomID),
					slog.Any("error", err))
			}
		}
	}
}
