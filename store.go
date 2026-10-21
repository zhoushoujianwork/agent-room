package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type server struct {
	db      *sql.DB
	baseURL string
}

type room struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Visibility     string   `json:"visibility"`
	CreatedAt      string   `json:"created_at"`
	LatestSeq      int64    `json:"latest_seq"`
	URL            string   `json:"url"`
	APIURL         string   `json:"api_url"`
	LastActivityAt string   `json:"last_activity_at"`
	Summary        *message `json:"summary"`
	ordinal        int64
}

type post struct {
	Sender          string `json:"sender"`
	Content         string `json:"content"`
	To              string `json:"to,omitempty"`
	ReplyTo         string `json:"reply_to,omitempty"`
	ClientMessageID string `json:"client_message_id"`
	Kind            string `json:"kind"`
	Covers          *int64 `json:"covers_through_seq,omitempty"`
}

type message struct {
	ID        string `json:"id"`
	RoomID    string `json:"room_id"`
	Seq       int64  `json:"seq"`
	CreatedAt string `json:"created_at"`
	post
}

type problem struct {
	status int
	text   string
}

func (e *problem) Error() string { return e.text }

func newServer(path, base string) (*server, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS board_rooms (
 ordinal INTEGER PRIMARY KEY AUTOINCREMENT,
 id TEXT NOT NULL UNIQUE, title TEXT NOT NULL, visibility TEXT NOT NULL,
 created_at TEXT NOT NULL, latest_seq INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS board_messages (
 id TEXT PRIMARY KEY, room_id TEXT NOT NULL REFERENCES board_rooms(id),
 seq INTEGER NOT NULL, created_at TEXT NOT NULL,
 sender TEXT NOT NULL, content TEXT NOT NULL, target TEXT NOT NULL,
 reply_to TEXT NOT NULL, client_message_id TEXT NOT NULL, kind TEXT NOT NULL,
 covers_through_seq INTEGER, request_hash TEXT NOT NULL,
 UNIQUE(room_id, seq), UNIQUE(room_id, client_message_id)
);
CREATE INDEX IF NOT EXISTS board_summary ON board_messages(room_id, kind, covers_through_seq DESC, seq DESC);
CREATE INDEX IF NOT EXISTS board_public ON board_rooms(visibility, ordinal);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &server{db: db, baseURL: base}, nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

type scanner interface{ Scan(...any) error }
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const messageColumns = `id, room_id, seq, created_at, sender, content, target, reply_to, client_message_id, kind, covers_through_seq`

func scanMessage(row scanner) (message, error) {
	var m message
	err := row.Scan(&m.ID, &m.RoomID, &m.Seq, &m.CreatedAt, &m.Sender, &m.Content, &m.To, &m.ReplyTo, &m.ClientMessageID, &m.Kind, &m.Covers)
	return m, err
}

func latestSummary(ctx context.Context, q queryer, id string) (*message, error) {
	m, err := scanMessage(q.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM board_messages WHERE room_id=? AND kind='summary' ORDER BY covers_through_seq DESC, seq DESC LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *server) getRoom(ctx context.Context, q queryer, id string) (room, error) {
	var r room
	err := q.QueryRowContext(ctx, `SELECT ordinal,id,title,visibility,created_at,latest_seq FROM board_rooms WHERE id=?`, id).Scan(&r.ordinal, &r.ID, &r.Title, &r.Visibility, &r.CreatedAt, &r.LatestSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return r, &problem{404, "room not found"}
	}
	if err != nil {
		return r, err
	}
	r.URL = s.baseURL + "/rooms/" + r.ID
	r.APIURL = s.baseURL + "/api/rooms/" + r.ID
	r.LastActivityAt = r.CreatedAt
	if r.LatestSeq > 0 {
		err = q.QueryRowContext(ctx, `SELECT created_at FROM board_messages WHERE room_id=? AND seq=?`, id, r.LatestSeq).Scan(&r.LastActivityAt)
		if err != nil {
			return r, err
		}
	}
	r.Summary, err = latestSummary(ctx, q, id)
	return r, err
}

func (s *server) appendMessage(ctx context.Context, id string, p post) (message, bool, error) {
	var empty message
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback()
	var seq int64
	err = tx.QueryRowContext(ctx, `SELECT latest_seq FROM board_rooms WHERE id=?`, id).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, false, &problem{404, "room not found"}
	}
	if err != nil {
		return empty, false, err
	}
	b, _ := json.Marshal(p)
	digest := fmt.Sprintf("%x", sha256.Sum256(b))
	var oldHash string
	err = tx.QueryRowContext(ctx, `SELECT request_hash FROM board_messages WHERE room_id=? AND client_message_id=?`, id, p.ClientMessageID).Scan(&oldHash)
	if err == nil {
		if oldHash != digest {
			return empty, false, &problem{409, "client_message_id already used for a different message"}
		}
		m, err := scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageColumns+` FROM board_messages WHERE room_id=? AND client_message_id=?`, id, p.ClientMessageID))
		return m, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return empty, false, err
	}
	if p.ReplyTo != "" {
		var exists int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM board_messages WHERE room_id=? AND id=?`, id, p.ReplyTo).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return empty, false, &problem{404, "reply target not found in this room"}
		}
		if err != nil {
			return empty, false, err
		}
	}
	if p.Covers != nil && (*p.Covers < 0 || *p.Covers > seq) {
		return empty, false, &problem{400, "summary coverage must refer to existing history or zero"}
	}
	mid, err := randomID()
	if err != nil {
		return empty, false, err
	}
	m := message{ID: mid, RoomID: id, Seq: seq + 1, CreatedAt: now(), post: p}
	_, err = tx.ExecContext(ctx, `INSERT INTO board_messages (`+messageColumns+`,request_hash) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, id, m.Seq, m.CreatedAt, p.Sender, p.Content, p.To, p.ReplyTo, p.ClientMessageID, p.Kind, p.Covers, digest)
	if err != nil {
		return empty, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE board_rooms SET latest_seq=? WHERE id=?`, m.Seq, id)
	if err != nil {
		return empty, false, err
	}
	if err = tx.Commit(); err != nil {
		return empty, false, err
	}
	return m, true, nil
}
