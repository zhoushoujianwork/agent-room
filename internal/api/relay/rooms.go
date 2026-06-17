package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agent-room/internal/io/memory"
	"agent-room/internal/io/sqlite"
	"agent-room/internal/models"
)

const (
	myRoomsDefaultLimit = 50
	myRoomsMaxLimit     = 200
)

// handleMyRooms serves GET /v1/me/rooms. It returns the signed-in user's
// durable room list (created or joined/opened), newest opened first.
func (s *Server) handleMyRooms(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeError(w, http.StatusNotFound, "auth disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	login := s.loginFromRequest(r)
	if login == "" {
		writeError(w, http.StatusForbidden, "sign in required")
		return
	}
	if s.rooms == nil {
		writeError(w, http.StatusInternalServerError, "no room storage configured")
		return
	}
	limit := myRoomsDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(n, myRoomsMaxLimit)
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		offset = n
	}
	rooms, err := s.rooms.ListUserRooms(r.Context(), login, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rooms)
}

// handleMyRoom serves PUT/DELETE /v1/me/rooms/:id. PUT records that the
// signed-in user opened/joined this room; DELETE hides it from their list
// without deleting the room itself.
func (s *Server) handleMyRoom(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeError(w, http.StatusNotFound, "auth disabled")
		return
	}
	roomID, ok := parseMyRoomPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown route")
		return
	}
	login := s.loginFromRequest(r)
	if login == "" {
		writeError(w, http.StatusForbidden, "sign in required")
		return
	}
	if s.rooms == nil {
		writeError(w, http.StatusInternalServerError, "no room storage configured")
		return
	}

	switch r.Method {
	case http.MethodPut:
		if _, err := s.ensureRoom(r.Context(), roomID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.rooms.RecordUserRoom(r.Context(), login, roomID, time.Now().UTC()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodDelete:
		if err := s.rooms.ForgetUserRoom(r.Context(), login, roomID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleRoomCRUD dispatches GET/PATCH/DELETE on /v1/rooms/:id.
func (s *Server) handleRoomCRUD(w http.ResponseWriter, r *http.Request, roomID string) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetRoom(w, r, roomID)
	case http.MethodPatch:
		s.handlePatchRoom(w, r, roomID)
	case http.MethodDelete:
		s.handleDeleteRoom(w, r, roomID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func parseMyRoomPath(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/v1/me/rooms/")
	if rest == path {
		return "", false
	}
	rest = strings.Trim(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// handleGetRoom returns the room record, lazy-creating it if missing.
// This preserves the existing "just hit a URL and a room exists"
// behaviour for anonymous links.
func (s *Server) handleGetRoom(w http.ResponseWriter, r *http.Request, roomID string) {
	if s.authEnabled() && s.loginFromRequest(r) == "" {
		writeError(w, http.StatusForbidden, "sign in required")
		return
	}
	room, err := s.ensureRoom(r.Context(), roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// ensureRoom looks up a room and creates an anonymous placeholder if
// it doesn't exist yet. This lets visitors hit /v1/rooms/<id> without
// having posted to /v1/rooms first — the legacy flow.
func (s *Server) ensureRoom(ctx context.Context, roomID string) (*models.Room, error) {
	if s.rooms == nil {
		return &models.Room{
			ID:        roomID,
			CreatedAt: time.Now().UTC(),
		}, nil
	}
	room, err := s.rooms.GetRoom(ctx, roomID)
	if err == nil {
		return room, nil
	}
	if !isNotFound(err) {
		return nil, err
	}
	placeholder := models.Room{
		ID:        roomID,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.rooms.CreateRoom(ctx, placeholder); err != nil {
		return nil, err
	}
	return &placeholder, nil
}

// requireOwner pulls the cookie + claims and returns the owner login
// when the caller matches room.OwnerLogin. A configured cross-room admin
// (AGENT_ROOM_ADMINS) passes for any room regardless of ownership — admins
// manage and enter every room as if they owned it. Writes 403 (and returns
// ok=false) when:
//   - auth is disabled
//   - no session cookie / invalid session
//   - the caller is neither the room owner nor an admin
func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request, roomID string) (string, bool) {
	if !s.authEnabled() {
		writeError(w, http.StatusForbidden, "auth disabled — room has no owner")
		return "", false
	}
	login := s.loginFromRequest(r)
	if login == "" {
		writeError(w, http.StatusForbidden, "sign in required")
		return "", false
	}
	if s.rooms == nil {
		writeError(w, http.StatusForbidden, "no room storage")
		return "", false
	}
	// Admins skip the ownership check entirely; the room may not even need to
	// exist yet for them to manage it once it does, but we still confirm it.
	if s.cfg.IsAdmin(login) {
		return login, true
	}
	room, err := s.rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "room not found")
			return "", false
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return "", false
	}
	if room.OwnerLogin == nil || *room.OwnerLogin != login {
		writeError(w, http.StatusForbidden, "not room owner")
		return "", false
	}
	return login, true
}

func (s *Server) handlePatchRoom(w http.ResponseWriter, r *http.Request, roomID string) {
	if _, ok := s.requireOwner(w, r, roomID); !ok {
		return
	}
	var body struct {
		Title *string `json:"title"`
		Gated *bool   `json:"gated"`
		Ended *bool   `json:"ended"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	room, err := s.rooms.GetRoom(r.Context(), roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.Title != nil {
		title := strings.TrimSpace(*body.Title)
		if title == "" {
			room.Title = nil
		} else {
			room.Title = &title
		}
	}
	if body.Gated != nil {
		room.Gated = *body.Gated
	}
	if body.Ended != nil {
		room.Ended = *body.Ended
	}
	if err := s.rooms.UpdateRoom(r.Context(), *room); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) handleDeleteRoom(w http.ResponseWriter, r *http.Request, roomID string) {
	if _, ok := s.requireOwner(w, r, roomID); !ok {
		return
	}
	if err := s.rooms.DeleteAccessRequestsByRoom(r.Context(), roomID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.rooms.DeleteMessagesByRoom(r.Context(), roomID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 附件寿命 = 房间寿命:随消息一起级联清理。
	if s.attachments != nil {
		if err := s.attachments.DeleteAttachmentsByRoom(r.Context(), roomID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.rooms.DeleteUserRoomsByRoom(r.Context(), roomID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.rooms.DeleteRoom(r.Context(), roomID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// isNotFound checks either store's NotFound sentinel without forcing
// callers to know which one they hold.
func isNotFound(err error) bool {
	return errors.Is(err, sqlite.ErrNotFound) || errors.Is(err, memory.ErrNotFound)
}
