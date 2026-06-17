package memory

import (
	"context"
	"strings"
	"time"

	"agent-room/internal/models"
)

// Search filters a room's messages by content substring, type, and sequence
// lower bound, returning oldest-first like List. Mirrors the SQLite store so
// the relay behaves identically on either backend.
func (s *Store) Search(_ context.Context, roomID string, q models.MessageQuery) ([]models.ChatMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	typeSet := make(map[models.MessageType]bool, len(q.Types))
	for _, t := range q.Types {
		typeSet[t] = true
	}
	needle := strings.ToLower(strings.TrimSpace(q.Q))

	// Resolve the BeforeID cursor to its seq once. An empty/unknown id leaves
	// beforeSeq at 0, which disables the upper bound (mirrors SQLite, where an
	// unknown id yields no rows — see below: 0 means "no cursor", and a real
	// cursor always has seq >= 1).
	var beforeSeq int64
	cursorMissing := false
	if id := strings.TrimSpace(q.BeforeID); id != "" {
		if seq, ok := s.msgSeq[id]; ok {
			beforeSeq = seq
		} else {
			cursorMissing = true
		}
	}

	matched := make([]models.ChatMessage, 0)
	if cursorMissing {
		return matched, nil
	}
	for _, msg := range s.messages[roomID] {
		if q.SinceSeq > 0 && s.msgSeq[msg.ID] <= q.SinceSeq {
			continue
		}
		if beforeSeq > 0 && s.msgSeq[msg.ID] >= beforeSeq {
			continue
		}
		if len(typeSet) > 0 && !typeSet[msg.Type] {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(msg.Content), needle) {
			continue
		}
		matched = append(matched, msg)
	}

	// Apply Limit to the most recent matches (tail), preserving order.
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[len(matched)-q.Limit:]
	}
	out := make([]models.ChatMessage, len(matched))
	copy(out, matched)
	return out, nil
}

// GetSummary returns the rolling summary for a room, or a zero value (with
// RoomID set) when none exists yet.
func (s *Store) GetSummary(_ context.Context, roomID string) (models.RoomSummary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sum, ok := s.summaries[roomID]; ok {
		return sum, nil
	}
	return models.RoomSummary{RoomID: roomID}, nil
}

// UpsertSummary writes or replaces the rolling summary for a room.
func (s *Store) UpsertSummary(_ context.Context, summary models.RoomSummary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if summary.UpdatedAt.IsZero() {
		summary.UpdatedAt = time.Now().UTC()
	}
	s.summaries[summary.RoomID] = summary
	return nil
}

func (s *Store) DeleteSummary(_ context.Context, roomID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.summaries, roomID)
	return nil
}
