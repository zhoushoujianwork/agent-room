package models

import (
	"context"
	"time"
)

type MessageStore interface {
	Append(ctx context.Context, message ChatMessage) error
	List(ctx context.Context, roomID string, limit int) ([]ChatMessage, error)
}

// MessageQuery filters a room's message history for search/backfill.
// All fields are optional; the zero value lists the most recent messages
// exactly like List. Stores apply the filters with parameterized queries.
type MessageQuery struct {
	// Q matches a case-insensitive substring of message content.
	Q string
	// Types, when non-empty, restricts results to these message types.
	Types []MessageType
	// SinceSeq returns only messages with a storage sequence greater than
	// this value (0 = no lower bound). Useful for incremental reads.
	SinceSeq int64
	// BeforeID returns only messages stored before the message with this id
	// (exclusive upper bound), the mirror of SinceSeq used to page backwards
	// into older history as the client scrolls up. Empty = no upper bound.
	// An unknown id yields no results (the cursor points at nothing we hold).
	BeforeID string
	// Limit caps the number of returned messages (0 = store default).
	Limit int
}

// MessageSearcher is an optional capability: stores that can filter history
// by content/type/sequence implement it. The relay falls back to plain List
// when a store does not.
type MessageSearcher interface {
	Search(ctx context.Context, roomID string, q MessageQuery) ([]ChatMessage, error)
}

// RoomSummary is a rolling, LLM-generated digest of a room's history.
// CoveredSeq records the largest message sequence already folded into the
// summary so the generator can summarize incrementally.
type RoomSummary struct {
	RoomID     string    `json:"room_id"`
	Summary    string    `json:"summary"`
	CoveredSeq int64     `json:"covered_seq"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// SummaryStore persists one rolling summary per room. Both the SQLite and
// in-memory backends implement it. GetSummary returns a nil-safe zero value
// (not an error) when a room has no summary yet.
type SummaryStore interface {
	GetSummary(ctx context.Context, roomID string) (RoomSummary, error)
	UpsertSummary(ctx context.Context, summary RoomSummary) error
	DeleteSummary(ctx context.Context, roomID string) error
}

// RoomStore persists rooms and access requests. Both the SQLite and
// in-memory backing stores implement this so the relay can run with
// either.
type RoomStore interface {
	GetRoom(ctx context.Context, roomID string) (*Room, error)
	CreateRoom(ctx context.Context, room Room) error
	UpdateRoom(ctx context.Context, room Room) error
	DeleteRoom(ctx context.Context, roomID string) error
	// ListRooms returns rooms newest-first, skipping the first offset rows
	// and capped at limit (0 = no cap). Used by the admin "all rooms" view
	// for pagination; ordinary callers fetch by id.
	ListRooms(ctx context.Context, limit, offset int) ([]Room, error)
	RecordUserActivity(ctx context.Context, user UserActivity, incrementLogin bool) error
	ListUserActivities(ctx context.Context) ([]UserActivity, error)
	RecordUserRoom(ctx context.Context, login, roomID string, openedAt time.Time) error
	ForgetUserRoom(ctx context.Context, login, roomID string) error
	ListUserRooms(ctx context.Context, login string, limit, offset int) ([]UserRoom, error)
	DeleteUserRoomsByRoom(ctx context.Context, roomID string) error

	CreateAccessRequest(ctx context.Context, req AccessRequest) error
	GetAccessRequest(ctx context.Context, requestID string) (*AccessRequest, error)
	UpdateAccessRequest(ctx context.Context, req AccessRequest) error
	ListAccessRequests(ctx context.Context, roomID string, limit int) ([]AccessRequest, error)
	// FindPendingByRequester returns the open request for this requester
	// in this room, if any. Either login or anonID may be empty; both
	// being empty means the caller has no identity and we treat that as
	// "no existing pending".
	FindPendingByRequester(ctx context.Context, roomID, login, anonID string) (*AccessRequest, error)
	// FindApprovedByRequester returns an approved request for this
	// requester in this room, if any.
	FindApprovedByRequester(ctx context.Context, roomID, login, anonID string) (*AccessRequest, error)
	DeleteAccessRequestsByRoom(ctx context.Context, roomID string) error
	DeleteMessagesByRoom(ctx context.Context, roomID string) error
}
