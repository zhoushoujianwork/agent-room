package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"agent-room/internal/models"
)

// UpsertAgent records (or refreshes) the binding between an agent id and its
// owner. On reconnect it updates owner/last_seen and clears revoked, so a
// previously seen agent that comes back with a valid token re-binds. Label and
// provider are only overwritten when the incoming value is non-empty, so a
// presence update that omits them does not wipe what we already have.
func (s *Store) UpsertAgent(ctx context.Context, agent models.Agent) error {
	now := agent.LastSeenAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	created := agent.CreatedAt
	if created.IsZero() {
		created = now
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO agents (agent_id, owner_login, label, provider, created_at, last_seen_at, revoked)
        VALUES (?, ?, ?, ?, ?, ?, 0)
        ON CONFLICT(agent_id) DO UPDATE SET
            owner_login  = excluded.owner_login,
            label        = CASE WHEN excluded.label <> '' THEN excluded.label ELSE agents.label END,
            provider     = CASE WHEN excluded.provider <> '' THEN excluded.provider ELSE agents.provider END,
            last_seen_at = excluded.last_seen_at,
            revoked      = 0`,
		agent.AgentID,
		agent.OwnerLogin,
		agent.Label,
		agent.Provider,
		created.UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("sqlite upsert agent: %w", err)
	}
	return nil
}

func (s *Store) GetAgent(ctx context.Context, agentID string) (*models.Agent, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT agent_id, owner_login, label, provider, created_at, last_seen_at, revoked
        FROM agents WHERE agent_id = ?`, agentID)
	return scanAgent(row)
}

func (s *Store) ListAgentsByOwner(ctx context.Context, owner string) ([]models.Agent, error) {
	return s.listAgents(ctx, `
        SELECT agent_id, owner_login, label, provider, created_at, last_seen_at, revoked
        FROM agents WHERE owner_login = ? AND revoked = 0
        ORDER BY last_seen_at DESC`, owner)
}

func (s *Store) ListAllAgents(ctx context.Context) ([]models.Agent, error) {
	return s.listAgents(ctx, `
        SELECT agent_id, owner_login, label, provider, created_at, last_seen_at, revoked
        FROM agents WHERE revoked = 0
        ORDER BY last_seen_at DESC`)
}

func (s *Store) listAgents(ctx context.Context, query string, args ...any) ([]models.Agent, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite list agents: %w", err)
	}
	defer rows.Close()
	out := make([]models.Agent, 0)
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite list agents rows: %w", err)
	}
	return out, nil
}

// RevokeAgent unbinds an agent (sets revoked=1). The row is kept for audit but
// excluded from owner listings; a later reconnect with a valid token re-binds
// via UpsertAgent (which resets revoked=0).
func (s *Store) RevokeAgent(ctx context.Context, agentID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agents SET revoked = 1 WHERE agent_id = ?`, agentID)
	if err != nil {
		return fmt.Errorf("sqlite revoke agent: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) InsertAgentToken(ctx context.Context, token models.AgentToken) error {
	created := token.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	expiresAt := ""
	if token.ExpiresAt != nil && !token.ExpiresAt.IsZero() {
		expiresAt = token.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	temporary := 0
	if token.Temporary {
		temporary = 1
	}
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO agent_tokens (token_hash, owner_login, note, created_at, last_used_at, expires_at, room_id, temporary, revoked)
        VALUES (?, ?, ?, ?, '', ?, ?, ?, 0)`,
		token.TokenHash,
		token.OwnerLogin,
		token.Note,
		created.UTC().Format(time.RFC3339Nano),
		expiresAt,
		token.RoomID,
		temporary,
	)
	if err != nil {
		return fmt.Errorf("sqlite insert agent token: %w", err)
	}
	return nil
}

// LookupAgentToken returns the token row for a given hash, or ErrNotFound.
func (s *Store) LookupAgentToken(ctx context.Context, hash string) (*models.AgentToken, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT token_hash, owner_login, note, created_at, last_used_at, expires_at, room_id, temporary, revoked
        FROM agent_tokens WHERE token_hash = ?`, hash)
	return scanAgentToken(row)
}

func (s *Store) TouchAgentTokenLastUsed(ctx context.Context, hash string, t time.Time) error {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE agent_tokens SET last_used_at = ? WHERE token_hash = ?`,
		t.UTC().Format(time.RFC3339Nano), hash)
	if err != nil {
		return fmt.Errorf("sqlite touch agent token: %w", err)
	}
	return nil
}

func (s *Store) ListAgentTokensByOwner(ctx context.Context, owner string) ([]models.AgentToken, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT token_hash, owner_login, note, created_at, last_used_at, expires_at, room_id, temporary, revoked
        FROM agent_tokens WHERE owner_login = ? AND revoked = 0
        ORDER BY created_at DESC`, owner)
	if err != nil {
		return nil, fmt.Errorf("sqlite list agent tokens: %w", err)
	}
	defer rows.Close()
	out := make([]models.AgentToken, 0)
	for rows.Next() {
		t, err := scanAgentToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite list agent tokens rows: %w", err)
	}
	return out, nil
}

// RevokeAgentTokenByPrefix revokes the single active token whose hash starts
// with prefix for the given owner. If the prefix matches zero tokens, (0, nil)
// is returned so the caller can 404. If the prefix matches more than one token
// the operation is aborted and (n, nil) is returned with n>1 so the caller can
// 409 — no tokens are revoked in this case. Atomicity is ensured by running
// the count and update inside the same transaction with a LIKE predicate.
func (s *Store) RevokeAgentTokenByPrefix(ctx context.Context, owner, prefix string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite revoke agent token begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM agent_tokens
		WHERE owner_login = ? AND revoked = 0 AND token_hash LIKE ? || '%'`,
		owner, prefix).Scan(&count); err != nil {
		return 0, fmt.Errorf("sqlite revoke agent token count: %w", err)
	}
	if count != 1 {
		// 0 → not found; >1 → ambiguous prefix — do not revoke anything.
		return count, nil
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_tokens SET revoked = 1
		WHERE owner_login = ? AND revoked = 0 AND token_hash LIKE ? || '%'`,
		owner, prefix); err != nil {
		return 0, fmt.Errorf("sqlite revoke agent token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite revoke agent token commit: %w", err)
	}
	return 1, nil
}

// AddAgentRoom records that agentID should be in roomID (desired state).
// Idempotent: inserting a duplicate row is silently ignored.
func (s *Store) AddAgentRoom(ctx context.Context, agentID, roomID, addedBy string) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO agent_rooms (agent_id, room_id, added_by, added_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(agent_id, room_id) DO NOTHING`,
		agentID, roomID, addedBy, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("sqlite add agent room: %w", err)
	}
	return nil
}

// RemoveAgentRoom removes the desired-state entry for agentID/roomID.
// No error if the row does not exist.
func (s *Store) RemoveAgentRoom(ctx context.Context, agentID, roomID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM agent_rooms WHERE agent_id = ? AND room_id = ?`, agentID, roomID)
	if err != nil {
		return fmt.Errorf("sqlite remove agent room: %w", err)
	}
	return nil
}

// ListAgentRooms returns the desired room_id list for an agent.
func (s *Store) ListAgentRooms(ctx context.Context, agentID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT room_id FROM agent_rooms WHERE agent_id = ? ORDER BY added_at`, agentID)
	if err != nil {
		return nil, fmt.Errorf("sqlite list agent rooms: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("sqlite scan agent room: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite list agent rooms rows: %w", err)
	}
	return out, nil
}

func scanAgent(sc scanner) (*models.Agent, error) {
	var (
		a                    models.Agent
		createdRaw, lastSeen string
		revoked              int
	)
	if err := sc.Scan(&a.AgentID, &a.OwnerLogin, &a.Label, &a.Provider, &createdRaw, &lastSeen, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("sqlite scan agent: %w", err)
	}
	created, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		return nil, fmt.Errorf("sqlite parse agent created_at %q: %w", createdRaw, err)
	}
	seen, err := time.Parse(time.RFC3339Nano, lastSeen)
	if err != nil {
		return nil, fmt.Errorf("sqlite parse agent last_seen_at %q: %w", lastSeen, err)
	}
	a.CreatedAt = created
	a.LastSeenAt = seen
	a.Revoked = revoked != 0
	return &a, nil
}

func scanAgentToken(sc scanner) (*models.AgentToken, error) {
	var (
		t          models.AgentToken
		createdRaw string
		lastUsed   string
		expiresRaw string
		temporary  int
		revoked    int
	)
	if err := sc.Scan(&t.TokenHash, &t.OwnerLogin, &t.Note, &createdRaw, &lastUsed, &expiresRaw, &t.RoomID, &temporary, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("sqlite scan agent token: %w", err)
	}
	created, err := time.Parse(time.RFC3339Nano, createdRaw)
	if err != nil {
		return nil, fmt.Errorf("sqlite parse agent token created_at %q: %w", createdRaw, err)
	}
	t.CreatedAt = created
	t.Revoked = revoked != 0
	t.HashPrefix = tokenHashPrefix(t.TokenHash)
	if lastUsed != "" {
		ts, err := time.Parse(time.RFC3339Nano, lastUsed)
		if err != nil {
			return nil, fmt.Errorf("sqlite parse agent token last_used_at %q: %w", lastUsed, err)
		}
		t.LastUsedAt = &ts
	}
	if expiresRaw != "" {
		ts, err := time.Parse(time.RFC3339Nano, expiresRaw)
		if err != nil {
			return nil, fmt.Errorf("sqlite parse agent token expires_at %q: %w", expiresRaw, err)
		}
		t.ExpiresAt = &ts
	}
	t.Temporary = temporary != 0
	return &t, nil
}

// tokenHashPrefix returns the public short handle for a token hash: the first
// 12 hex chars. Long enough that accidental collisions are vanishingly rare,
// short enough to be a convenient handle in the UI and revoke API.
func tokenHashPrefix(hash string) string {
	const n = 12
	if len(hash) <= n {
		return hash
	}
	return hash[:n]
}
