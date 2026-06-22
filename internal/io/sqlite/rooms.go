package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-room/internal/models"
)

// ErrNotFound signals that the requested room or access request does
// not exist. Callers translate this to 404.
var ErrNotFound = errors.New("not found")

func (s *Store) GetRoom(ctx context.Context, roomID string) (*models.Room, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, owner_login, title, github_url, gated, ended, created_at
        FROM rooms WHERE id = ?`, roomID)
	return scanRoom(row)
}

func (s *Store) CreateRoom(ctx context.Context, room models.Room) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO rooms (id, owner_login, title, github_url, gated, ended, created_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)`,
		room.ID,
		nullableString(room.OwnerLogin),
		nullableString(room.Title),
		nullableString(room.GitHubURL),
		boolToInt(room.Gated),
		boolToInt(room.Ended),
		room.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("sqlite create room: %w", err)
	}
	return nil
}

func (s *Store) UpdateRoom(ctx context.Context, room models.Room) error {
	res, err := s.db.ExecContext(ctx, `
        UPDATE rooms
        SET owner_login = ?, title = ?, github_url = ?, gated = ?, ended = ?
        WHERE id = ?`,
		nullableString(room.OwnerLogin),
		nullableString(room.Title),
		nullableString(room.GitHubURL),
		boolToInt(room.Gated),
		boolToInt(room.Ended),
		room.ID,
	)
	if err != nil {
		return fmt.Errorf("sqlite update room: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListRooms(ctx context.Context, limit, offset int) ([]models.Room, error) {
	query := `
        SELECT id, owner_login, title, github_url, gated, ended, created_at
        FROM rooms
        ORDER BY created_at DESC`
	var args []any
	if limit > 0 || offset > 0 {
		// SQLite requires LIMIT before OFFSET; -1 means "no cap".
		if limit <= 0 {
			limit = -1
		}
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite list rooms: %w", err)
	}
	defer rows.Close()

	out := make([]models.Room, 0)
	for rows.Next() {
		room, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite list rooms rows: %w", err)
	}
	return out, nil
}

func (s *Store) RecordUserActivity(ctx context.Context, user models.UserActivity, incrementLogin bool) error {
	login := strings.TrimSpace(user.Login)
	if login == "" {
		return nil
	}
	now := user.LastLoginAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	firstSeen := user.FirstSeenAt
	if firstSeen.IsZero() {
		firstSeen = now
	}
	increment := 0
	if incrementLogin {
		increment = 1
	}
	initialCount := increment
	if initialCount == 0 {
		initialCount = 1
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO user_activity (
            login, name, email, avatar_url, first_seen_at, last_login_at, login_count
        ) VALUES (?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(login) DO UPDATE SET
            name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE user_activity.name END,
            email = CASE WHEN excluded.email <> '' THEN excluded.email ELSE user_activity.email END,
            avatar_url = CASE WHEN excluded.avatar_url <> '' THEN excluded.avatar_url ELSE user_activity.avatar_url END,
            last_login_at = CASE
                WHEN ? THEN excluded.last_login_at
                ELSE user_activity.last_login_at
            END,
            login_count = user_activity.login_count + ?`,
		login,
		strings.TrimSpace(user.Name),
		strings.TrimSpace(user.Email),
		strings.TrimSpace(user.AvatarURL),
		firstSeen.UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano),
		initialCount,
		boolToInt(incrementLogin),
		increment,
	)
	if err != nil {
		return fmt.Errorf("sqlite record user activity: %w", err)
	}
	return nil
}

func (s *Store) ListUserActivities(ctx context.Context) ([]models.UserActivity, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT login, name, email, avatar_url, first_seen_at, last_login_at, login_count
        FROM user_activity
        ORDER BY last_login_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sqlite list user_activity: %w", err)
	}
	defer rows.Close()

	out := make([]models.UserActivity, 0)
	for rows.Next() {
		user, err := scanUserActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite list user_activity rows: %w", err)
	}
	return out, nil
}

func (s *Store) RecordUserRoom(ctx context.Context, login, roomID string, openedAt time.Time) error {
	login = canonicalUserRoomLogin(login)
	roomID = strings.TrimSpace(roomID)
	if login == "" || roomID == "" {
		return nil
	}
	if openedAt.IsZero() {
		openedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO user_rooms (login, room_id, last_opened_at, hidden)
        VALUES (?, ?, ?, 0)
        ON CONFLICT(login, room_id) DO UPDATE SET
            last_opened_at = excluded.last_opened_at,
            hidden = 0`,
		login,
		roomID,
		openedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("sqlite record user room: %w", err)
	}
	return nil
}

func (s *Store) ForgetUserRoom(ctx context.Context, login, roomID string) error {
	login = canonicalUserRoomLogin(login)
	roomID = strings.TrimSpace(roomID)
	if login == "" || roomID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO user_rooms (login, room_id, last_opened_at, hidden)
        SELECT ?, id, ?, 1 FROM rooms WHERE id = ?
        ON CONFLICT(login, room_id) DO UPDATE SET hidden = 1`,
		login,
		time.Now().UTC().Format(time.RFC3339Nano),
		roomID,
	)
	if err != nil {
		return fmt.Errorf("sqlite forget user room: %w", err)
	}
	return nil
}

func (s *Store) ListUserRooms(ctx context.Context, login string, limit, offset int) ([]models.UserRoom, error) {
	login = canonicalUserRoomLogin(login)
	if login == "" {
		return []models.UserRoom{}, nil
	}
	query := `
        SELECT r.id, r.owner_login, r.title, r.github_url, r.gated, r.ended, r.created_at, ur.last_opened_at
        FROM user_rooms ur
        JOIN rooms r ON r.id = ur.room_id
        WHERE ur.login = ? AND ur.hidden = 0
        ORDER BY ur.last_opened_at DESC, r.created_at DESC`
	args := []any{login}
	if limit > 0 || offset > 0 {
		if limit <= 0 {
			limit = -1
		}
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite list user rooms: %w", err)
	}
	defer rows.Close()

	out := make([]models.UserRoom, 0)
	for rows.Next() {
		room, err := scanUserRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite list user rooms rows: %w", err)
	}
	return out, nil
}

func (s *Store) DeleteRoom(ctx context.Context, roomID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rooms WHERE id = ?`, roomID); err != nil {
		return fmt.Errorf("sqlite delete room: %w", err)
	}
	return nil
}

func (s *Store) DeleteUserRoomsByRoom(ctx context.Context, roomID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_rooms WHERE room_id = ?`, roomID); err != nil {
		return fmt.Errorf("sqlite delete user_rooms: %w", err)
	}
	return nil
}

func (s *Store) DeleteMessagesByRoom(ctx context.Context, roomID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE room_id = ?`, roomID); err != nil {
		return fmt.Errorf("sqlite delete messages: %w", err)
	}
	return nil
}

func (s *Store) DeleteAccessRequestsByRoom(ctx context.Context, roomID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM access_requests WHERE room_id = ?`, roomID); err != nil {
		return fmt.Errorf("sqlite delete access_requests: %w", err)
	}
	return nil
}

func (s *Store) CreateAccessRequest(ctx context.Context, req models.AccessRequest) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO access_requests (
            id, room_id, requester_login, requester_anonid, requester_label,
            via, location, status, persistence, created_at, resolved_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID,
		req.RoomID,
		nullableString(req.RequesterLogin),
		nullableNonEmpty(req.RequesterAnonID),
		req.RequesterLabel,
		req.Via,
		nullableString(req.Location),
		string(req.Status),
		nullablePersistence(req.Persistence),
		req.CreatedAt.UTC().Format(time.RFC3339Nano),
		nullableTime(req.ResolvedAt),
	)
	if err != nil {
		return fmt.Errorf("sqlite create access_request: %w", err)
	}
	return nil
}

func (s *Store) GetAccessRequest(ctx context.Context, requestID string) (*models.AccessRequest, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT id, room_id, requester_login, requester_anonid, requester_label,
               via, location, status, persistence, created_at, resolved_at
        FROM access_requests WHERE id = ?`, requestID)
	return scanAccessRequest(row)
}

func (s *Store) UpdateAccessRequest(ctx context.Context, req models.AccessRequest) error {
	res, err := s.db.ExecContext(ctx, `
        UPDATE access_requests
        SET status = ?, persistence = ?, resolved_at = ?
        WHERE id = ?`,
		string(req.Status),
		nullablePersistence(req.Persistence),
		nullableTime(req.ResolvedAt),
		req.ID,
	)
	if err != nil {
		return fmt.Errorf("sqlite update access_request: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListAccessRequests(ctx context.Context, roomID string, limit int) ([]models.AccessRequest, error) {
	query := `
        SELECT id, room_id, requester_login, requester_anonid, requester_label,
               via, location, status, persistence, created_at, resolved_at
        FROM access_requests
        WHERE room_id = ?
        ORDER BY created_at DESC`
	args := []any{roomID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite list access_requests: %w", err)
	}
	defer rows.Close()

	out := make([]models.AccessRequest, 0)
	for rows.Next() {
		req, err := scanAccessRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite list access_requests rows: %w", err)
	}
	return out, nil
}

func (s *Store) FindPendingByRequester(ctx context.Context, roomID, login, anonID string) (*models.AccessRequest, error) {
	return s.findRequester(ctx, roomID, login, anonID, string(models.AccessRequestStatusPending))
}

func (s *Store) FindApprovedByRequester(ctx context.Context, roomID, login, anonID string) (*models.AccessRequest, error) {
	return s.findRequester(ctx, roomID, login, anonID, string(models.AccessRequestStatusApproved))
}

func (s *Store) findRequester(ctx context.Context, roomID, login, anonID, status string) (*models.AccessRequest, error) {
	// Match on either login or anon id, whichever is non-empty. Both
	// being empty means we have no identity to match — return NotFound.
	if login == "" && anonID == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
        SELECT id, room_id, requester_login, requester_anonid, requester_label,
               via, location, status, persistence, created_at, resolved_at
        FROM access_requests
        WHERE room_id = ?
          AND status = ?
          AND (
                (? <> '' AND requester_login = ?)
             OR (? <> '' AND requester_anonid = ?)
          )
        ORDER BY created_at DESC
        LIMIT 1`,
		roomID, status,
		login, login,
		anonID, anonID,
	)
	return scanAccessRequest(row)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRoom(s scanner) (*models.Room, error) {
	var (
		id         string
		owner      sql.NullString
		title      sql.NullString
		githubURL  sql.NullString
		gated      int
		ended      int
		createdRaw string
	)
	if err := s.Scan(&id, &owner, &title, &githubURL, &gated, &ended, &createdRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("sqlite scan room: %w", err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		return nil, fmt.Errorf("sqlite parse room created_at %q: %w", createdRaw, err)
	}
	room := &models.Room{
		ID:        id,
		Gated:     gated != 0,
		Ended:     ended != 0,
		CreatedAt: createdAt,
	}
	if owner.Valid {
		v := owner.String
		room.OwnerLogin = &v
	}
	if title.Valid {
		v := title.String
		room.Title = &v
	}
	if githubURL.Valid {
		v := githubURL.String
		room.GitHubURL = &v
	}
	return room, nil
}

func scanUserRoom(s scanner) (models.UserRoom, error) {
	var (
		id            string
		owner         sql.NullString
		title         sql.NullString
		githubURL     sql.NullString
		gated         int
		ended         int
		createdRaw    string
		lastOpenedRaw string
	)
	if err := s.Scan(&id, &owner, &title, &githubURL, &gated, &ended, &createdRaw, &lastOpenedRaw); err != nil {
		return models.UserRoom{}, fmt.Errorf("sqlite scan user_room: %w", err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		return models.UserRoom{}, fmt.Errorf("sqlite parse user_room created_at %q: %w", createdRaw, err)
	}
	lastOpenedAt, err := time.Parse(time.RFC3339Nano, lastOpenedRaw)
	if err != nil {
		return models.UserRoom{}, fmt.Errorf("sqlite parse user_room last_opened_at %q: %w", lastOpenedRaw, err)
	}
	room := models.UserRoom{
		Room: models.Room{
			ID:        id,
			Gated:     gated != 0,
			Ended:     ended != 0,
			CreatedAt: createdAt,
		},
		LastOpenedAt: lastOpenedAt,
	}
	if owner.Valid {
		v := owner.String
		room.OwnerLogin = &v
	}
	if title.Valid {
		v := title.String
		room.Title = &v
	}
	if githubURL.Valid {
		v := githubURL.String
		room.GitHubURL = &v
	}
	return room, nil
}

func scanAccessRequest(s scanner) (*models.AccessRequest, error) {
	var (
		id         string
		roomID     string
		login      sql.NullString
		anonID     sql.NullString
		label      string
		via        string
		location   sql.NullString
		status     string
		persist    sql.NullString
		createdRaw string
		resolvedAt sql.NullString
	)
	if err := s.Scan(&id, &roomID, &login, &anonID, &label, &via, &location, &status, &persist, &createdRaw, &resolvedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("sqlite scan access_request: %w", err)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		return nil, fmt.Errorf("sqlite parse access_request created_at %q: %w", createdRaw, err)
	}
	req := &models.AccessRequest{
		ID:             id,
		RoomID:         roomID,
		RequesterLabel: label,
		Via:            via,
		Status:         models.AccessRequestStatus(status),
		CreatedAt:      createdAt,
	}
	if login.Valid {
		v := login.String
		req.RequesterLogin = &v
	}
	if anonID.Valid {
		req.RequesterAnonID = anonID.String
	}
	if location.Valid {
		v := location.String
		req.Location = &v
	}
	if persist.Valid {
		p := models.AccessRequestPersistence(persist.String)
		req.Persistence = &p
	}
	if resolvedAt.Valid {
		ts, err := time.Parse(time.RFC3339Nano, resolvedAt.String)
		if err != nil {
			return nil, fmt.Errorf("sqlite parse access_request resolved_at %q: %w", resolvedAt.String, err)
		}
		req.ResolvedAt = &ts
	}
	return req, nil
}

func scanUserActivity(s scanner) (models.UserActivity, error) {
	var (
		user                 models.UserActivity
		firstSeen, lastLogin string
	)
	if err := s.Scan(
		&user.Login,
		&user.Name,
		&user.Email,
		&user.AvatarURL,
		&firstSeen,
		&lastLogin,
		&user.LoginCount,
	); err != nil {
		return models.UserActivity{}, fmt.Errorf("sqlite scan user_activity: %w", err)
	}
	first, err := time.Parse(time.RFC3339Nano, firstSeen)
	if err != nil {
		return models.UserActivity{}, fmt.Errorf("sqlite parse user_activity first_seen_at %q: %w", firstSeen, err)
	}
	last, err := time.Parse(time.RFC3339Nano, lastLogin)
	if err != nil {
		return models.UserActivity{}, fmt.Errorf("sqlite parse user_activity last_login_at %q: %w", lastLogin, err)
	}
	user.FirstSeenAt = first
	user.LastLoginAt = last
	return user, nil
}

func nullableString(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func nullableNonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func nullablePersistence(p *models.AccessRequestPersistence) any {
	if p == nil {
		return nil
	}
	return string(*p)
}

func canonicalUserRoomLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}
