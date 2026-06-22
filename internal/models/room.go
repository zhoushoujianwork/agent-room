package models

import "time"

// Room is the persistent record for a chat room. Rooms can be created
// anonymously (OwnerLogin == nil) — that's the existing flow — or by a
// signed-in GitHub user, in which case OwnerLogin contains their login
// and they can manage gating / title / lifecycle.
type Room struct {
	ID         string    `json:"room_id"`
	OwnerLogin *string   `json:"owner"`
	Title      *string   `json:"title"`
	GitHubURL  *string   `json:"github_url"`
	Gated      bool      `json:"gated"`
	Ended      bool      `json:"ended"`
	CreatedAt  time.Time `json:"created_at"`
}

// UserRoom is the signed-in user's durable "my rooms" record. It embeds the
// room metadata and adds the user's own last-opened timestamp so clients can
// sort recently created/joined rooms consistently across browsers.
type UserRoom struct {
	Room
	LastOpenedAt time.Time `json:"last_opened_at"`
}

// AccessRequestStatus enumerates the lifecycle of a join request.
type AccessRequestStatus string

const (
	AccessRequestStatusPending  AccessRequestStatus = "pending"
	AccessRequestStatusApproved AccessRequestStatus = "approved"
	AccessRequestStatusDenied   AccessRequestStatus = "denied"
)

// AccessRequestPersistence captures how long an approval is valid.
// "once" = only for the connection that issued the request. "persist"
// = the requester is a permanent member of the room.
type AccessRequestPersistence string

const (
	AccessRequestPersistenceOnce    AccessRequestPersistence = "once"
	AccessRequestPersistencePersist AccessRequestPersistence = "persist"
)

// AccessRequest is a single join request against a gated room.
type AccessRequest struct {
	ID              string                    `json:"id"`
	RoomID          string                    `json:"room_id"`
	RequesterLogin  *string                   `json:"requester_login"`
	RequesterAnonID string                    `json:"-"` // server-side de-dupe key for anonymous requesters
	RequesterLabel  string                    `json:"requester_label"`
	Via             string                    `json:"via"`
	Location        *string                   `json:"location"`
	Status          AccessRequestStatus       `json:"status"`
	Persistence     *AccessRequestPersistence `json:"persistence,omitempty"`
	CreatedAt       time.Time                 `json:"created_at"`
	ResolvedAt      *time.Time                `json:"resolved_at,omitempty"`
}

// UserActivity is the durable admin-facing profile/activity row for a
// signed-in user. Online fields are populated at request time from the relay
// hub and are not persisted.
type UserActivity struct {
	Login           string     `json:"login"`
	Name            string     `json:"name,omitempty"`
	Email           string     `json:"email,omitempty"`
	AvatarURL       string     `json:"avatar_url,omitempty"`
	FirstSeenAt     time.Time  `json:"first_seen_at"`
	LastLoginAt     time.Time  `json:"last_login_at"`
	LoginCount      int        `json:"login_count"`
	RoomsCreated    int        `json:"rooms_created"`
	Online          bool       `json:"online"`
	ConnectionCount int        `json:"connection_count"`
	OnlineRoomIDs   []string   `json:"online_room_ids,omitempty"`
	LastSeenAt      *time.Time `json:"last_seen_at,omitempty"`
}

// UserPresence is the transient online state for a signed-in user aggregated
// across all rooms.
type UserPresence struct {
	Login           string     `json:"login"`
	ConnectionCount int        `json:"connection_count"`
	RoomIDs         []string   `json:"room_ids,omitempty"`
	ConnectedAt     time.Time  `json:"connected_at"`
	LastSeenAt      *time.Time `json:"last_seen_at,omitempty"`
}

type UserTrendPoint struct {
	Date     string `json:"date"`
	Logins   int    `json:"logins"`
	NewUsers int    `json:"new_users"`
}

type AdminUsersReport struct {
	Users       []UserActivity   `json:"users"`
	TotalUsers  int              `json:"total_users"`
	OnlineUsers int              `json:"online_users"`
	Logins24h   int              `json:"logins_24h"`
	Logins7d    int              `json:"logins_7d"`
	Trend       []UserTrendPoint `json:"trend"`
}
