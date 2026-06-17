package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-room/internal/models"
)

// GetSummary returns the rolling summary for a room. A room with no summary
// yet yields a zero RoomSummary (RoomID set, empty Summary) and no error, so
// callers can treat "no summary" and "empty summary" uniformly.
func (s *Store) GetSummary(ctx context.Context, roomID string) (models.RoomSummary, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT room_id, summary, covered_seq, updated_at
        FROM room_summaries WHERE room_id = ?`, roomID)
	var (
		out       models.RoomSummary
		updatedAt string
	)
	err := row.Scan(&out.RoomID, &out.Summary, &out.CoveredSeq, &updatedAt)
	if err == sql.ErrNoRows {
		return models.RoomSummary{RoomID: roomID}, nil
	}
	if err != nil {
		return models.RoomSummary{}, fmt.Errorf("sqlite get summary: %w", err)
	}
	ts, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return models.RoomSummary{}, fmt.Errorf("sqlite parse summary updated_at %q: %w", updatedAt, err)
	}
	out.UpdatedAt = ts
	return out, nil
}

// UpsertSummary writes (or replaces) the rolling summary for a room.
func (s *Store) UpsertSummary(ctx context.Context, summary models.RoomSummary) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO room_summaries (room_id, summary, covered_seq, updated_at)
        VALUES (?, ?, ?, ?)
        ON CONFLICT(room_id) DO UPDATE SET
            summary = excluded.summary,
            covered_seq = excluded.covered_seq,
            updated_at = excluded.updated_at`,
		summary.RoomID,
		summary.Summary,
		summary.CoveredSeq,
		summary.UpdatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("sqlite upsert summary: %w", err)
	}
	return nil
}

func (s *Store) DeleteSummary(ctx context.Context, roomID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM room_summaries WHERE room_id = ?`, roomID); err != nil {
		return fmt.Errorf("sqlite delete summary: %w", err)
	}
	return nil
}

// Search filters a room's messages by content substring, type, and sequence
// lower bound. Results are returned oldest-first, like List.
func (s *Store) Search(ctx context.Context, roomID string, q models.MessageQuery) ([]models.ChatMessage, error) {
	var sb strings.Builder
	sb.WriteString(`
        SELECT id, room_id, type, sender_id, sender_kind, target_id,
               content, reply_requested, turn_budget, created_at, metadata_json
        FROM messages
        WHERE room_id = ?`)
	args := []any{roomID}

	if trimmed := strings.TrimSpace(q.Q); trimmed != "" {
		sb.WriteString(" AND content LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(trimmed)+"%")
	}
	if q.SinceSeq > 0 {
		sb.WriteString(" AND seq > ?")
		args = append(args, q.SinceSeq)
	}
	if id := strings.TrimSpace(q.BeforeID); id != "" {
		// Page backwards: only rows older than the cursor message. A scalar
		// subquery resolves the cursor's seq; an unknown id makes the subquery
		// NULL and the comparison false, yielding no rows (correct: the cursor
		// points at history we don't hold).
		sb.WriteString(" AND seq < (SELECT seq FROM messages WHERE id = ?)")
		args = append(args, id)
	}
	if len(q.Types) > 0 {
		sb.WriteString(" AND type IN (")
		for i, t := range q.Types {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("?")
			args = append(args, string(t))
		}
		sb.WriteString(")")
	}
	// Newest-first with LIMIT so we return the most recent matches, then
	// reverse to oldest-first below for chronological reading.
	sb.WriteString(" ORDER BY seq DESC")
	if q.Limit > 0 {
		sb.WriteString(" LIMIT ?")
		args = append(args, q.Limit)
	}

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite search: %w", err)
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
		return nil, fmt.Errorf("sqlite search rows: %w", err)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// escapeLike escapes LIKE wildcards so a user-supplied query is matched
// literally (backslash is the ESCAPE char declared in the query above).
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// scanMessageRow decodes one messages row into a ChatMessage. Shared by
// List and Search to keep column handling in one place.
func scanMessageRow(rows *sql.Rows) (models.ChatMessage, error) {
	var (
		msg            models.ChatMessage
		msgType        string
		senderKind     string
		replyRequested int
		createdAt      string
		metadataJSON   string
	)
	if err := rows.Scan(
		&msg.ID, &msg.RoomID, &msgType, &msg.SenderID, &senderKind,
		&msg.TargetID, &msg.Content, &replyRequested, &msg.TurnBudget,
		&createdAt, &metadataJSON,
	); err != nil {
		return models.ChatMessage{}, fmt.Errorf("sqlite scan: %w", err)
	}
	msg.Type = models.MessageType(msgType)
	msg.SenderKind = models.SenderKind(senderKind)
	msg.ReplyRequested = replyRequested != 0
	ts, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return models.ChatMessage{}, fmt.Errorf("sqlite parse created_at %q: %w", createdAt, err)
	}
	msg.CreatedAt = ts
	if metadataJSON != "" && metadataJSON != "null" {
		if err := json.Unmarshal([]byte(metadataJSON), &msg.Metadata); err != nil {
			return models.ChatMessage{}, fmt.Errorf("sqlite parse metadata: %w", err)
		}
	}
	return msg, nil
}
