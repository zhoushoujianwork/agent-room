package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"agent-room/internal/models"
)

// ErrNotFound is returned when a lookup misses. Mirrors the SQLite
// store's sentinel so the relay handlers can treat both stores
// identically.
var ErrNotFound = errors.New("not found")

type Store struct {
	mu        sync.RWMutex
	messages  map[string][]models.ChatMessage
	rooms     map[string]models.Room
	requests  map[string]models.AccessRequest
	users     map[string]models.UserActivity
	userRooms map[string]map[string]userRoomRecord
	summaries map[string]models.RoomSummary
	// attachments 以 roomID -> attachmentID 双键存,镜像 SQLite 的
	// (room_id, id) 查询口径:不知道房间 id 拿不到字节。
	attachments map[string]map[string]models.Attachment
	// seq assigns a monotonically increasing sequence per appended message,
	// mirroring SQLite's AUTOINCREMENT, so MessageQuery.SinceSeq works.
	seq    int64
	msgSeq map[string]int64
	// agents / agentTokens mirror the SQLite agent-binding tables. Created
	// lazily (see ensureAgentMaps) so this optional feature stays out of the
	// hot NewStore path.
	agents       map[string]models.Agent
	agentTokens  map[string]models.AgentToken
	agentConfigs map[string]models.AgentConfig
	// agentRooms mirrors agent_rooms: agent_id -> set of room_ids (desired state).
	agentRooms map[string]map[string]struct{}
}

func NewStore() *Store {
	return &Store{
		messages:    make(map[string][]models.ChatMessage),
		rooms:       make(map[string]models.Room),
		requests:    make(map[string]models.AccessRequest),
		users:       make(map[string]models.UserActivity),
		userRooms:   make(map[string]map[string]userRoomRecord),
		summaries:   make(map[string]models.RoomSummary),
		attachments: make(map[string]map[string]models.Attachment),
		msgSeq:      make(map[string]int64),
	}
}

type userRoomRecord struct {
	lastOpenedAt time.Time
	hidden       bool
}

func (s *Store) Append(_ context.Context, message models.ChatMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Idempotent on non-empty ID: a bridge seeds room history on join and
	// then also receives its own messages echoed back over the broadcast,
	// so the same ID can arrive twice. SQLite enforces this with a UNIQUE
	// id column; mirror that here. Empty IDs (used by some tests and not
	// produced by the normalized network path) always append.
	if message.ID != "" {
		for _, existing := range s.messages[message.RoomID] {
			if existing.ID == message.ID {
				return nil
			}
		}
	}
	s.messages[message.RoomID] = append(s.messages[message.RoomID], message)
	s.seq++
	if message.ID != "" {
		s.msgSeq[message.ID] = s.seq
	}
	return nil
}

func (s *Store) List(_ context.Context, roomID string, limit int) ([]models.ChatMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := s.messages[roomID]
	if limit > 0 && len(items) > limit {
		items = items[len(items)-limit:]
	}

	out := make([]models.ChatMessage, len(items))
	copy(out, items)
	return out, nil
}

func (s *Store) GetRoom(_ context.Context, roomID string) (*models.Room, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rooms[roomID]
	if !ok {
		return nil, ErrNotFound
	}
	c := r
	return &c, nil
}

func (s *Store) CreateRoom(_ context.Context, room models.Room) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rooms[room.ID] = room
	return nil
}

func (s *Store) UpdateRoom(_ context.Context, room models.Room) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rooms[room.ID]; !ok {
		return ErrNotFound
	}
	s.rooms[room.ID] = room
	return nil
}

func (s *Store) ListRooms(_ context.Context, limit, offset int) ([]models.Room, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]models.Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if offset > 0 {
		if offset >= len(out) {
			return []models.Room{}, nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) RecordUserActivity(_ context.Context, user models.UserActivity, incrementLogin bool) error {
	login := strings.TrimSpace(user.Login)
	if login == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := user.LastLoginAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	existing, ok := s.users[login]
	if !ok {
		firstSeen := user.FirstSeenAt
		if firstSeen.IsZero() {
			firstSeen = now
		}
		user.Login = login
		user.FirstSeenAt = firstSeen
		user.LastLoginAt = now
		if user.LoginCount <= 0 {
			user.LoginCount = 1
		}
		s.users[login] = user
		return nil
	}
	if strings.TrimSpace(user.Name) != "" {
		existing.Name = strings.TrimSpace(user.Name)
	}
	if strings.TrimSpace(user.Email) != "" {
		existing.Email = strings.TrimSpace(user.Email)
	}
	if strings.TrimSpace(user.AvatarURL) != "" {
		existing.AvatarURL = strings.TrimSpace(user.AvatarURL)
	}
	if incrementLogin {
		existing.LastLoginAt = now
		existing.LoginCount++
	}
	s.users[login] = existing
	return nil
}

func (s *Store) ListUserActivities(_ context.Context) ([]models.UserActivity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]models.UserActivity, 0, len(s.users))
	for _, user := range s.users {
		out = append(out, user)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastLoginAt.After(out[j].LastLoginAt)
	})
	return out, nil
}

func (s *Store) RecordUserRoom(_ context.Context, login, roomID string, openedAt time.Time) error {
	login = canonicalUserRoomLogin(login)
	roomID = strings.TrimSpace(roomID)
	if login == "" || roomID == "" {
		return nil
	}
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.userRooms[login] == nil {
		s.userRooms[login] = make(map[string]userRoomRecord)
	}
	s.userRooms[login][roomID] = userRoomRecord{lastOpenedAt: openedAt.UTC()}
	return nil
}

func (s *Store) ForgetUserRoom(_ context.Context, login, roomID string) error {
	login = canonicalUserRoomLogin(login)
	roomID = strings.TrimSpace(roomID)
	if login == "" || roomID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rooms[roomID]; !ok {
		return nil
	}
	if s.userRooms[login] == nil {
		s.userRooms[login] = make(map[string]userRoomRecord)
	}
	rec := s.userRooms[login][roomID]
	rec.hidden = true
	if rec.lastOpenedAt.IsZero() {
		rec.lastOpenedAt = time.Now().UTC()
	}
	s.userRooms[login][roomID] = rec
	return nil
}

func (s *Store) ListUserRooms(_ context.Context, login string, limit, offset int) ([]models.UserRoom, error) {
	login = canonicalUserRoomLogin(login)
	if login == "" {
		return []models.UserRoom{}, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := s.userRooms[login]
	out := make([]models.UserRoom, 0, len(records))
	for roomID, rec := range records {
		if rec.hidden {
			continue
		}
		room, ok := s.rooms[roomID]
		if !ok {
			continue
		}
		lastOpenedAt := rec.lastOpenedAt
		if lastOpenedAt.IsZero() {
			lastOpenedAt = room.CreatedAt
		}
		out = append(out, models.UserRoom{
			Room:         room,
			LastOpenedAt: lastOpenedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastOpenedAt.Equal(out[j].LastOpenedAt) {
			return out[i].LastOpenedAt.After(out[j].LastOpenedAt)
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if offset > 0 {
		if offset >= len(out) {
			return []models.UserRoom{}, nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) DeleteRoom(_ context.Context, roomID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rooms, roomID)
	return nil
}

func (s *Store) DeleteUserRoomsByRoom(_ context.Context, roomID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for login, rooms := range s.userRooms {
		delete(rooms, roomID)
		if len(rooms) == 0 {
			delete(s.userRooms, login)
		}
	}
	return nil
}

func (s *Store) DeleteMessagesByRoom(_ context.Context, roomID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.messages, roomID)
	return nil
}

func (s *Store) DeleteAccessRequestsByRoom(_ context.Context, roomID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, req := range s.requests {
		if req.RoomID == roomID {
			delete(s.requests, id)
		}
	}
	return nil
}

func (s *Store) CreateAccessRequest(_ context.Context, req models.AccessRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[req.ID] = req
	return nil
}

func (s *Store) GetAccessRequest(_ context.Context, requestID string) (*models.AccessRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.requests[requestID]
	if !ok {
		return nil, ErrNotFound
	}
	c := r
	return &c, nil
}

func (s *Store) UpdateAccessRequest(_ context.Context, req models.AccessRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.requests[req.ID]; !ok {
		return ErrNotFound
	}
	s.requests[req.ID] = req
	return nil
}

func (s *Store) ListAccessRequests(_ context.Context, roomID string, limit int) ([]models.AccessRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]models.AccessRequest, 0)
	for _, r := range s.requests {
		if r.RoomID == roomID {
			out = append(out, r)
		}
	}
	// Newest first matches the SQLite ORDER BY.
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) FindPendingByRequester(_ context.Context, roomID, login, anonID string) (*models.AccessRequest, error) {
	return s.findRequester(roomID, login, anonID, models.AccessRequestStatusPending)
}

func (s *Store) FindApprovedByRequester(_ context.Context, roomID, login, anonID string) (*models.AccessRequest, error) {
	return s.findRequester(roomID, login, anonID, models.AccessRequestStatusApproved)
}

func (s *Store) findRequester(roomID, login, anonID string, status models.AccessRequestStatus) (*models.AccessRequest, error) {
	if login == "" && anonID == "" {
		return nil, ErrNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var hit *models.AccessRequest
	for _, r := range s.requests {
		if r.RoomID != roomID || r.Status != status {
			continue
		}
		if login != "" && r.RequesterLogin != nil && *r.RequesterLogin == login {
			c := r
			if hit == nil || c.CreatedAt.After(hit.CreatedAt) {
				hit = &c
			}
			continue
		}
		if anonID != "" && r.RequesterAnonID == anonID {
			c := r
			if hit == nil || c.CreatedAt.After(hit.CreatedAt) {
				hit = &c
			}
		}
	}
	if hit == nil {
		return nil, ErrNotFound
	}
	return hit, nil
}

func canonicalUserRoomLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}
