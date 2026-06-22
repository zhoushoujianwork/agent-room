package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"agent-room/internal/models"
)

// migrate creates or upgrades all tables this app owns. It is split
// out from Open so tests can call it twice in a row to verify
// idempotency (the design contract calls this out explicitly). Every
// statement here uses IF NOT EXISTS so re-running is safe.
func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS messages (
            seq             INTEGER PRIMARY KEY AUTOINCREMENT,
            id              TEXT    NOT NULL UNIQUE,
            room_id         TEXT    NOT NULL,
            type            TEXT    NOT NULL DEFAULT '',
            sender_id       TEXT    NOT NULL DEFAULT '',
            sender_kind     TEXT    NOT NULL DEFAULT '',
            target_id       TEXT    NOT NULL DEFAULT '',
            content         TEXT    NOT NULL DEFAULT '',
            reply_requested INTEGER NOT NULL DEFAULT 0,
            turn_budget     INTEGER NOT NULL DEFAULT 0,
            created_at      TEXT    NOT NULL,
            metadata_json   TEXT    NOT NULL DEFAULT '{}'
        );`,
		`CREATE INDEX IF NOT EXISTS idx_messages_room_seq ON messages(room_id, seq);`,
		`CREATE TABLE IF NOT EXISTS rooms (
            id           TEXT PRIMARY KEY,
            owner_login  TEXT NULL,
            title        TEXT NULL,
            github_url   TEXT NULL,
            gated        INTEGER NOT NULL DEFAULT 0,
            ended        INTEGER NOT NULL DEFAULT 0,
            created_at   TEXT NOT NULL
        );`,
		`CREATE TABLE IF NOT EXISTS access_requests (
            id               TEXT PRIMARY KEY,
            room_id          TEXT NOT NULL,
            requester_login  TEXT NULL,
            requester_anonid TEXT NULL,
            requester_label  TEXT NOT NULL,
            via              TEXT NOT NULL,
            location         TEXT NULL,
            status           TEXT NOT NULL,
            persistence      TEXT NULL,
            created_at       TEXT NOT NULL,
            resolved_at      TEXT NULL,
            FOREIGN KEY (room_id) REFERENCES rooms(id)
        );`,
		`CREATE INDEX IF NOT EXISTS idx_access_requests_room ON access_requests(room_id, status);`,
		`CREATE TABLE IF NOT EXISTS attachments (
            id         TEXT PRIMARY KEY,
            room_id    TEXT NOT NULL,
            mime       TEXT NOT NULL,
            size       INTEGER NOT NULL DEFAULT 0,
            bytes      BLOB NOT NULL,
            created_at TEXT NOT NULL
        );`,
		`CREATE INDEX IF NOT EXISTS idx_attachments_room ON attachments(room_id);`,
		`CREATE TABLE IF NOT EXISTS room_summaries (
            room_id     TEXT PRIMARY KEY,
            summary     TEXT NOT NULL DEFAULT '',
            covered_seq INTEGER NOT NULL DEFAULT 0,
            updated_at  TEXT NOT NULL
        );`,
		`CREATE TABLE IF NOT EXISTS user_activity (
            login         TEXT PRIMARY KEY,
            name          TEXT NOT NULL DEFAULT '',
            email         TEXT NOT NULL DEFAULT '',
            avatar_url    TEXT NOT NULL DEFAULT '',
            first_seen_at TEXT NOT NULL,
            last_login_at TEXT NOT NULL,
            login_count   INTEGER NOT NULL DEFAULT 0
        );`,
		`CREATE INDEX IF NOT EXISTS idx_user_activity_last_login ON user_activity(last_login_at);`,
		`CREATE TABLE IF NOT EXISTS user_rooms (
            login          TEXT NOT NULL,
            room_id        TEXT NOT NULL,
            last_opened_at TEXT NOT NULL,
            hidden         INTEGER NOT NULL DEFAULT 0,
            PRIMARY KEY (login, room_id),
            FOREIGN KEY (room_id) REFERENCES rooms(id) ON DELETE CASCADE
        );`,
		`CREATE INDEX IF NOT EXISTS idx_user_rooms_login_opened ON user_rooms(login, hidden, last_opened_at);`,
		`INSERT OR IGNORE INTO user_rooms (login, room_id, last_opened_at, hidden)
            SELECT lower(owner_login), id, created_at, 0
            FROM rooms
            WHERE owner_login IS NOT NULL AND owner_login <> '';`,
		`CREATE TABLE IF NOT EXISTS agents (
            agent_id     TEXT PRIMARY KEY,
            owner_login  TEXT NOT NULL,
            label        TEXT NOT NULL DEFAULT '',
            provider     TEXT NOT NULL DEFAULT '',
            created_at   TEXT NOT NULL,
            last_seen_at TEXT NOT NULL,
            revoked      INTEGER NOT NULL DEFAULT 0
        );`,
		`CREATE INDEX IF NOT EXISTS idx_agents_owner ON agents(owner_login);`,
		`CREATE TABLE IF NOT EXISTS agent_tokens (
            token_hash   TEXT PRIMARY KEY,
            owner_login  TEXT NOT NULL,
            note         TEXT NOT NULL DEFAULT '',
            created_at   TEXT NOT NULL,
            last_used_at TEXT NOT NULL DEFAULT '',
            expires_at   TEXT NOT NULL DEFAULT '',
            room_id      TEXT NOT NULL DEFAULT '',
            temporary    INTEGER NOT NULL DEFAULT 0,
            revoked      INTEGER NOT NULL DEFAULT 0
        );`,
		`CREATE INDEX IF NOT EXISTS idx_agent_tokens_owner ON agent_tokens(owner_login);`,
		`CREATE TABLE IF NOT EXISTS agent_configs (
            agent_id       TEXT PRIMARY KEY REFERENCES agents(agent_id),
            model          TEXT NOT NULL DEFAULT '',
            api_base_url   TEXT NOT NULL DEFAULT '',
            api_key_cipher TEXT NOT NULL DEFAULT '',
            updated_at     TEXT NOT NULL,
            updated_by     TEXT NOT NULL DEFAULT ''
        );`,
		`CREATE TABLE IF NOT EXISTS agent_rooms (
            agent_id TEXT NOT NULL,
            room_id  TEXT NOT NULL,
            added_by TEXT NOT NULL,
            added_at TEXT NOT NULL,
            PRIMARY KEY (agent_id, room_id)
        );`,
		`CREATE INDEX IF NOT EXISTS idx_agent_rooms_room ON agent_rooms(room_id);`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("sqlite migrate: %w", err)
		}
	}
	if err := ensureColumn(db, "rooms", "github_url", `github_url TEXT NULL`); err != nil {
		return err
	}
	for _, col := range []struct {
		name string
		ddl  string
	}{
		{name: "expires_at", ddl: `expires_at TEXT NOT NULL DEFAULT ''`},
		{name: "room_id", ddl: `room_id TEXT NOT NULL DEFAULT ''`},
		{name: "temporary", ddl: `temporary INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := ensureColumn(db, "agent_tokens", col.name, col.ddl); err != nil {
			return err
		}
	}
	return nil
}

func ensureColumn(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("sqlite inspect %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name      string
			typeName  string
			notNull   int
			defaultTo sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &defaultTo, &pk); err != nil {
			return fmt.Errorf("sqlite scan %s column: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite inspect %s columns: %w", table, err)
	}
	if _, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + ddl); err != nil {
		return fmt.Errorf("sqlite add %s.%s: %w", table, column, err)
	}
	return nil
}

type Store struct {
	db *sql.DB
}

// Open returns a SQLite-backed MessageStore at the given file path.
// The schema is created on first use. WAL is enabled so concurrent
// readers (history GETs) do not block the broadcast write path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("sqlite open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying *sql.DB for tests that want to exercise
// migrate() directly. Production callers should use the public methods.
func (s *Store) DB() *sql.DB { return s.db }

// Migrate re-runs the schema migration. Useful in tests to confirm
// idempotency; production callers do not need to invoke this — Open()
// already migrates on first use.
func (s *Store) Migrate() error { return migrate(s.db) }

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Append(ctx context.Context, msg models.ChatMessage) error {
	metadata, err := json.Marshal(orEmptyMap(msg.Metadata))
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO messages (
            id, room_id, type, sender_id, sender_kind, target_id,
            content, reply_requested, turn_budget, created_at, metadata_json
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `,
		msg.ID,
		msg.RoomID,
		string(msg.Type),
		msg.SenderID,
		string(msg.SenderKind),
		msg.TargetID,
		msg.Content,
		boolToInt(msg.ReplyRequested),
		msg.TurnBudget,
		msg.CreatedAt.UTC().Format(time.RFC3339Nano),
		string(metadata),
	); err != nil {
		return fmt.Errorf("sqlite append: %w", err)
	}
	return nil
}

func (s *Store) List(ctx context.Context, roomID string, limit int) ([]models.ChatMessage, error) {
	query := `
        SELECT id, room_id, type, sender_id, sender_kind, target_id,
               content, reply_requested, turn_budget, created_at, metadata_json
        FROM messages
        WHERE room_id = ?
        ORDER BY seq DESC
    `
	args := []any{roomID}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite list: %w", err)
	}
	defer rows.Close()

	out := make([]models.ChatMessage, 0)
	for rows.Next() {
		msg, err := scanMessageRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite rows: %w", err)
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
