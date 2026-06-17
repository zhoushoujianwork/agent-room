package relay

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"agent-room/internal/config"
	"agent-room/internal/models"
	"agent-room/internal/service/chat"
	"agent-room/internal/service/summary"
	"agent-room/internal/version"
	"agent-room/pkg/id"
)

//go:embed assets/dist
var assets embed.FS

type Server struct {
	cfg         config.Config
	service     *chat.Service
	rooms       models.RoomStore
	agents      AgentStore
	summaries   models.SummaryStore
	summarizer  *summary.Service
	attachments models.AttachmentStore
	logger      *slog.Logger
	upgrader    websocket.Upgrader
	hub         *hub

	agentJoinCodesMu sync.Mutex
	agentJoinCodes   map[string]agentJoinCode
}

// NewServer wires a relay with chat + room persistence + optional
// GitHub OAuth. The rooms store may be nil only for legacy test paths
// — production callers always pass one; if nil, room CRUD that needs
// persistence will 500.
func NewServer(cfg config.Config, service *chat.Service, rooms models.RoomStore, logger *slog.Logger) *Server {
	s := &Server{
		cfg:     cfg,
		service: service,
		rooms:   rooms,
		logger:  logger,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
		agentJoinCodes: make(map[string]agentJoinCode),
	}
	// Agent-ownership persistence is an optional capability: production stores
	// (SQLite, in-memory) implement AgentStore; legacy/test stores may not, in
	// which case the feature is simply off.
	if as, ok := rooms.(AgentStore); ok {
		s.agents = as
	}
	s.hub = newHub(service, logger)
	s.hub.agents = s.agents
	s.hub.secretKey = deriveSecretKey(cfg.SecretKey)
	return s
}

// WithSummary attaches a summary store + background summarizer so the relay
// can serve GET /rooms/:id/summary and refresh digests as messages arrive.
// Optional: when not called (or summarizer is nil), the summary route returns
// an empty payload and Publish skips summary notifications. Returns s so it
// can be chained at the construction site.
func (s *Server) WithSummary(store models.SummaryStore, summarizer *summary.Service) *Server {
	s.summaries = store
	s.summarizer = summarizer
	s.hub.summarizer = summarizer
	return s
}

// WithAttachments attaches an attachment store so the relay can serve room
// image uploads/downloads. Optional: when not called the attachment routes
// 404. Returns s so it can be chained at the construction site.
func (s *Server) WithAttachments(store models.AttachmentStore) *Server {
	s.attachments = store
	return s
}

// NewServerForTest preserves the older two-arg constructor signature
// for tests and any caller that does not need rooms persistence.
// Equivalent to NewServer with a zero config and a fresh in-memory
// room store.
func NewServerForTest(service *chat.Service, rooms models.RoomStore, logger *slog.Logger) *Server {
	return NewServer(config.Config{}, service, rooms, logger)
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/downloads/install.sh", s.handleInstallScript)
	mux.HandleFunc("/downloads/agent-room", s.handleBinaryDownload)
	mux.HandleFunc("/downloads/windows", s.handleWindowsDownload)
	mux.HandleFunc("/favicon.svg", s.handleFavicon)
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/auth/github/login", s.handleAuthLogin)
	mux.HandleFunc("/auth/github/callback", s.handleAuthCallback)
	mux.HandleFunc("/auth/sso/login", s.handleSSOLogin)
	mux.HandleFunc("/auth/sso/callback", s.handleSSOCallback)
	mux.HandleFunc("/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("/v1/me/rooms", s.handleMyRooms)
	mux.HandleFunc("/v1/me/rooms/", s.handleMyRoom)
	mux.HandleFunc("/v1/me", s.handleMe)
	mux.HandleFunc("/v1/admin/rooms", s.handleAdminRooms)
	mux.HandleFunc("/v1/admin/users", s.handleAdminUsers)
	mux.HandleFunc("/v1/agents", s.handleAgentsList)
	mux.HandleFunc("/v1/agents/tokens", s.handleAgentTokens)
	mux.HandleFunc("/v1/agents/tokens/", s.handleAgentTokenItem)
	mux.HandleFunc("/v1/agents/ws", s.handleAgentCtrlWS)
	mux.HandleFunc("/v1/agents/", s.handleAgentItem)
	mux.HandleFunc("/v1/rooms", s.handleCreateRoom)
	mux.HandleFunc("/v1/rooms/", s.handleRoom)
	return mux
}

func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	baseURL := s.publicBaseURL(r)
	script := unixInstallScript(baseURL)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(script))
}

func (s *Server) handleBinaryDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	osName := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("os")))
	arch := normalizeDownloadArch(r.URL.Query().Get("arch"))
	if osName != "darwin" && osName != "linux" {
		writeError(w, http.StatusBadRequest, "unsupported os")
		return
	}
	if arch == "" {
		writeError(w, http.StatusBadRequest, "unsupported arch")
		return
	}

	filename := fmt.Sprintf("agent-room-%s-%s", osName, arch)
	path := filepath.FromSlash("internal/api/relay/assets/downloads/" + filename)
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "binary is not available")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.ServeFile(w, r, path)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" &&
		r.URL.Path != "/agent-room-intro.svg" &&
		r.URL.Path != "/rooms" &&
		r.URL.Path != "/agents" &&
		r.URL.Path != "/admin" &&
		!strings.HasPrefix(r.URL.Path, "/rooms/") &&
		!strings.HasPrefix(r.URL.Path, "/assets/") &&
		!strings.HasPrefix(r.URL.Path, "/banners/") {
		writeError(w, http.StatusNotFound, "unknown route")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	dist, err := fs.Sub(assets, "assets/dist")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ui asset filesystem not found")
		return
	}

	if strings.HasPrefix(r.URL.Path, "/assets/") ||
		strings.HasPrefix(r.URL.Path, "/banners/") ||
		r.URL.Path == "/agent-room-intro.svg" {
		http.FileServer(http.FS(dist)).ServeHTTP(w, r)
		return
	}

	data, err := assets.ReadFile("assets/dist/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ui asset not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleFavicon serves the embedded SVG favicon. Registered as its own
// route because handleRoot only serves /assets/* and the SPA index;
// favicon.svg lives at the dist root, not under /assets/.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	data, err := assets.ReadFile("assets/dist/favicon.svg")
	if err != nil {
		writeError(w, http.StatusNotFound, "favicon not found")
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version.String()})
}

func (s *Server) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	login := s.loginFromRequest(r)
	if s.authEnabled() && login == "" {
		writeError(w, http.StatusForbidden, "sign in required")
		return
	}

	var body struct {
		Title string `json:"title"`
	}
	// Empty body is fine; only decode if there is something to decode.
	if r.ContentLength != 0 {
		// Best-effort: ignore decode errors so callers can post nothing
		// (the existing flow). A malformed body should not 400 here.
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	room := models.Room{
		ID:        newRoomID(),
		Gated:     login != "",
		Ended:     false,
		CreatedAt: time.Now().UTC(),
	}
	if login != "" {
		l := login
		room.OwnerLogin = &l
	}
	if title := strings.TrimSpace(body.Title); title != "" {
		room.Title = &title
	}

	if s.rooms != nil {
		if err := s.rooms.CreateRoom(r.Context(), room); err != nil {
			writeError(w, http.StatusInternalServerError, "create room failed: "+err.Error())
			return
		}
		if login != "" {
			if err := s.rooms.RecordUserRoom(r.Context(), login, room.ID, room.CreatedAt); err != nil {
				writeError(w, http.StatusInternalServerError, "record user room failed: "+err.Error())
				return
			}
		}
	}
	writeJSON(w, http.StatusCreated, room)
}

// Pagination bounds for the admin "all rooms" endpoint. The frontend pages
// with ?limit=&offset=; the max cap keeps a single response bounded even if
// a client asks for more.
const (
	adminRoomsDefaultLimit = 50
	adminRoomsMaxLimit     = 200
)

// handleAdminRooms returns rooms newest-first, paginated via ?limit= and
// ?offset= (defaults: limit 50, offset 0). Admin-only: the caller must hold a
// valid session whose login is in AGENT_ROOM_ADMINS. 404 when auth is
// disabled (mirrors the other auth-gated routes), 403 for a signed-in
// non-admin or anonymous caller.
func (s *Server) handleAdminRooms(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeError(w, http.StatusNotFound, "auth disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.isAdminRequest(r) {
		writeError(w, http.StatusForbidden, "admin only")
		return
	}
	if s.rooms == nil {
		writeError(w, http.StatusInternalServerError, "no room storage configured")
		return
	}
	limit := adminRoomsDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(n, adminRoomsMaxLimit)
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
	rooms, err := s.rooms.ListRooms(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rooms)
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeError(w, http.StatusNotFound, "auth disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.isAdminRequest(r) {
		writeError(w, http.StatusForbidden, "admin only")
		return
	}
	if s.rooms == nil {
		writeError(w, http.StatusInternalServerError, "no room storage configured")
		return
	}
	users, err := s.rooms.ListUserActivities(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rooms, err := s.rooms.ListRooms(r.Context(), 0, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	report := buildAdminUsersReport(users, rooms, s.hub.UserPresence(), time.Now().UTC())
	writeJSON(w, http.StatusOK, report)
}

func buildAdminUsersReport(users []models.UserActivity, rooms []models.Room, presence map[string]models.UserPresence, now time.Time) models.AdminUsersReport {
	roomCounts := make(map[string]int)
	for _, room := range rooms {
		if room.OwnerLogin == nil {
			continue
		}
		if login := strings.TrimSpace(*room.OwnerLogin); login != "" {
			roomCounts[strings.ToLower(login)]++
		}
	}

	byLogin := make(map[string]models.UserActivity, len(users)+len(presence))
	for _, user := range users {
		login := strings.TrimSpace(user.Login)
		if login == "" {
			continue
		}
		user.Login = login
		byLogin[strings.ToLower(login)] = user
	}
	for login, p := range presence {
		key := strings.ToLower(strings.TrimSpace(login))
		if key == "" {
			continue
		}
		user := byLogin[key]
		if user.Login == "" {
			user.Login = login
			user.FirstSeenAt = now
			user.LastLoginAt = now
			user.LoginCount = 1
		}
		user.Online = true
		user.ConnectionCount = p.ConnectionCount
		user.OnlineRoomIDs = append([]string(nil), p.RoomIDs...)
		user.LastSeenAt = p.LastSeenAt
		byLogin[key] = user
	}

	out := make([]models.UserActivity, 0, len(byLogin))
	since24h := now.Add(-24 * time.Hour)
	since7d := now.AddDate(0, 0, -7)
	report := models.AdminUsersReport{}
	newByDay := make(map[string]int)
	loginByDay := make(map[string]int)
	for key, user := range byLogin {
		user.RoomsCreated = roomCounts[key]
		if user.Online {
			report.OnlineUsers++
		}
		if !user.LastLoginAt.IsZero() {
			if !user.LastLoginAt.Before(since24h) {
				report.Logins24h++
			}
			if !user.LastLoginAt.Before(since7d) {
				report.Logins7d++
			}
			loginByDay[user.LastLoginAt.Format("2006-01-02")]++
		}
		if !user.FirstSeenAt.IsZero() {
			newByDay[user.FirstSeenAt.Format("2006-01-02")]++
		}
		out = append(out, user)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Online != out[j].Online {
			return out[i].Online
		}
		return out[i].LastLoginAt.After(out[j].LastLoginAt)
	})
	report.Users = out
	report.TotalUsers = len(out)
	for i := 6; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		report.Trend = append(report.Trend, models.UserTrendPoint{
			Date:     day,
			Logins:   loginByDay[day],
			NewUsers: newByDay[day],
		})
	}
	return report
}

// handleWindowsDownload serves the bare Windows binary, mirroring the
// darwin/linux flow: the frontend shows a copyable PowerShell snippet that
// downloads agent-room.exe and runs `bridge -relay <origin> -room <id>`.
// Nothing room- or relay-specific is baked into the artifact, so a stale
// download can never carry an outdated relay URL (the old zip+BAT starter did
// exactly that and shipped ws:// origins that fail behind an HTTPS-only
// ingress). Legacy ?room=/?relay= query params are accepted and ignored.
func (s *Server) handleWindowsDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	path := filepath.FromSlash("internal/api/relay/assets/downloads/agent-room-windows-amd64.exe")
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "windows binary is not available")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="agent-room.exe"`)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.ServeFile(w, r, path)
}

func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request) {
	// /v1/rooms/:id with no further path: room CRUD.
	if roomID, ok := parseRoomOnlyPath(r.URL.Path); ok {
		s.handleRoomCRUD(w, r, roomID)
		return
	}

	// /v1/rooms/:id/access-requests[/:rid]
	if roomID, requestID, ok := parseAccessRequestPath(r.URL.Path); ok {
		s.handleAccessRequests(w, r, roomID, requestID)
		return
	}

	// /v1/rooms/:id/attachments[/:aid]
	if roomID, attachmentID, ok := parseAttachmentPath(r.URL.Path); ok {
		s.handleAttachments(w, r, roomID, attachmentID)
		return
	}

	// /v1/rooms/:id/agent-join-codes[/redeem]
	if roomID, action, ok := parseAgentJoinCodePath(r.URL.Path); ok {
		s.handleAgentJoinCodes(w, r, roomID, action)
		return
	}

	// /v1/rooms/:id/agents[/:agentID]
	if roomID, agentID, ok := parseRoomAgentsPath(r.URL.Path); ok {
		s.handleRoomAgents(w, r, roomID, agentID)
		return
	}

	roomID, action, ok := parseRoomPath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown route")
		return
	}

	switch {
	case action == "messages" && r.Method == http.MethodGet:
		s.handleListMessages(w, r, roomID)
	case action == "messages" && r.Method == http.MethodPost:
		s.handlePostMessage(w, r, roomID)
	case action == "messages" && r.Method == http.MethodDelete:
		s.handleClearMessages(w, r, roomID)
	case action == "participants" && r.Method == http.MethodGet:
		s.handleListParticipants(w, r, roomID)
	case action == "summary" && r.Method == http.MethodGet:
		s.handleGetSummary(w, r, roomID)
	case action == "summary" && r.Method == http.MethodDelete:
		s.handleClearSummary(w, r, roomID)
	case action == "ws" && r.Method == http.MethodGet:
		s.handleWS(w, r, roomID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// parseRoomOnlyPath matches /v1/rooms/:id with no trailing path.
func parseRoomOnlyPath(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/v1/rooms/")
	if rest == path {
		return "", false
	}
	rest = strings.TrimRight(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// parseRoomAgentsPath matches /v1/rooms/:id/agents and /v1/rooms/:id/agents/:agentID.
// Returns (roomID, agentID, true); agentID is "" for the collection endpoint.
func parseRoomAgentsPath(path string) (string, string, bool) {
	rest := strings.TrimPrefix(path, "/v1/rooms/")
	if rest == path {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || parts[1] != "agents" || parts[0] == "" {
		return "", "", false
	}
	if len(parts) == 2 {
		return parts[0], "", true
	}
	if len(parts) == 3 && parts[2] != "" {
		return parts[0], parts[2], true
	}
	return "", "", false
}

// parseAgentJoinCodePath matches /v1/rooms/:id/agent-join-codes and
// /v1/rooms/:id/agent-join-codes/redeem.
func parseAgentJoinCodePath(path string) (string, string, bool) {
	rest := strings.TrimPrefix(path, "/v1/rooms/")
	if rest == path {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || parts[1] != "agent-join-codes" || parts[0] == "" {
		return "", "", false
	}
	if len(parts) == 2 {
		return parts[0], "", true
	}
	if len(parts) == 3 && parts[2] == "redeem" {
		return parts[0], "redeem", true
	}
	return "", "", false
}

// parseAccessRequestPath matches /v1/rooms/:id/access-requests
// and /v1/rooms/:id/access-requests/:rid.
func parseAccessRequestPath(path string) (string, string, bool) {
	rest := strings.TrimPrefix(path, "/v1/rooms/")
	if rest == path {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || parts[1] != "access-requests" || parts[0] == "" {
		return "", "", false
	}
	if len(parts) == 2 {
		return parts[0], "", true
	}
	if len(parts) == 3 && parts[2] != "" {
		return parts[0], parts[2], true
	}
	return "", "", false
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request, roomID string) {
	q := r.URL.Query()
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}

	query := models.MessageQuery{Limit: limit, Q: strings.TrimSpace(q.Get("q"))}
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			query.SinceSeq = n
		}
	}
	query.BeforeID = strings.TrimSpace(q.Get("before"))
	for _, t := range q["type"] {
		if t = strings.TrimSpace(t); t != "" {
			query.Types = append(query.Types, models.MessageType(t))
		}
	}

	var (
		messages []models.ChatMessage
		err      error
	)
	// Plain listing stays on the List path (cheap, no allocation of a query
	// struct semantics); only reach for Search when a filter is present.
	if query.Q == "" && query.SinceSeq == 0 && query.BeforeID == "" && len(query.Types) == 0 {
		messages, err = s.service.List(r.Context(), roomID, limit)
	} else {
		messages, err = s.service.Search(r.Context(), roomID, query)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

func (s *Server) handleClearMessages(w http.ResponseWriter, r *http.Request, roomID string) {
	if _, ok := s.requireOwner(w, r, roomID); !ok {
		return
	}
	if err := s.rooms.DeleteMessagesByRoom(r.Context(), roomID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.deleteSummary(r.Context(), roomID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleGetSummary returns the room's rolling LLM summary. When the summary
// feature is off or no summary exists yet, it returns an empty payload (200)
// so clients can treat "no summary" uniformly without special-casing 404.
func (s *Server) handleGetSummary(w http.ResponseWriter, r *http.Request, roomID string) {
	if s.summaries == nil {
		writeJSON(w, http.StatusOK, models.RoomSummary{RoomID: roomID})
		return
	}
	sum, err := s.summaries.GetSummary(r.Context(), roomID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (s *Server) handleClearSummary(w http.ResponseWriter, r *http.Request, roomID string) {
	if _, ok := s.requireOwner(w, r, roomID); !ok {
		return
	}
	if err := s.deleteSummary(r.Context(), roomID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteSummary(ctx context.Context, roomID string) error {
	if s.summaries == nil {
		return nil
	}
	return s.summaries.DeleteSummary(ctx, roomID)
}

func (s *Server) handleListParticipants(w http.ResponseWriter, _ *http.Request, roomID string) {
	writeJSON(w, http.StatusOK, s.hub.Participants(roomID))
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request, roomID string) {
	var msg models.ChatMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	msg, err := s.hub.Publish(r.Context(), roomID, msg)
	if err != nil {
		if errors.Is(err, ErrCommandMissingTarget) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, msg)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request, roomID string) {
	// Resolve agent ownership before upgrading so an invalid/revoked token
	// fails with a clean HTTP error instead of a mid-stream close. Carrying no
	// agent_token stays anonymous (backward compatible); carrying an invalid
	// one is rejected. When the store doesn't support agent ownership the token
	// is ignored (feature off), preserving legacy behavior.
	ownerLogin := ""
	tokenRoomID := ""
	tokenTemporary := false
	if token := strings.TrimSpace(r.URL.Query().Get("agent_token")); token != "" && s.agents != nil {
		resolved, ok := s.resolveAgentToken(r.Context(), token)
		if !ok {
			s.logger.Warn("rejected agent: invalid token", slog.String("room_id", roomID))
			writeError(w, http.StatusUnauthorized, "invalid or revoked agent token")
			return
		}
		if resolved.RoomID != "" && resolved.RoomID != roomID {
			s.logger.Warn("rejected agent: token room mismatch",
				slog.String("room_id", roomID),
				slog.String("token_room_id", resolved.RoomID))
			writeError(w, http.StatusForbidden, "agent token is not authorized for this room")
			return
		}
		ownerLogin = resolved.OwnerLogin
		tokenRoomID = resolved.RoomID
		tokenTemporary = resolved.Temporary
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Warn("websocket upgrade failed", slog.String("room_id", roomID), slog.Any("error", err))
		return
	}

	audit := r.URL.Query().Get("audit") == "1" && s.isAdminRequest(r)
	client := newClient(s.hub, roomID, conn, r.RemoteAddr, r.URL.Query(), audit)
	if ownerLogin != "" {
		client.owner = ownerLogin
		client.tokenRoomID = tokenRoomID
		client.tokenTemporary = tokenTemporary
		if client.participant.Metadata == nil {
			client.participant.Metadata = map[string]string{}
		}
		client.participant.Metadata["owner_login"] = ownerLogin
		s.logger.Info("agent bound to owner",
			slog.String("room_id", roomID), slog.String("owner_login", ownerLogin))
	}
	s.hub.register(client)

	go client.writeLoop()
	client.readLoop(r.Context())
}

func parseRoomPath(path string) (string, string, bool) {
	rest := strings.TrimPrefix(path, "/v1/rooms/")
	if rest == path {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

type hub struct {
	service *chat.Service
	logger  *slog.Logger
	// agents, when set, persists agent->owner bindings as agents come online.
	// Nil when the backing store doesn't support agent ownership.
	agents AgentStore
	// secretKey is the AES-256 key (derived from AGENT_ROOM_SECRET_KEY) used to
	// decrypt a stored agent api key before pushing it on the targeted
	// config_update channel. Nil when no secret is configured (api keys are
	// then never stored, so nothing to decrypt).
	secretKey []byte
	mu        sync.RWMutex
	rooms     map[string]map[*client]struct{}
	// agentCtrl holds agent-level control connections: agentID -> set of clients.
	// A control connection is not bound to any room (roomID="") and is not in h.rooms.
	agentCtrl map[string]map[*client]struct{}
	// agentRuntime holds the latest non-secret runtime config reported by a
	// connected bridge. It is an online view, not durable desired config.
	agentRuntime map[string]agentRuntimeConfig
	// execTokens maps roomID -> executor agent id -> exec_token. The relay is
	// the sole keeper of these tokens: an executor reports its token in its
	// presence message, the relay records it here (never storing or
	// broadcasting it), and stamps it onto targeted command messages just
	// before delivery. Agents therefore never see or send the token.
	execTokens map[string]map[string]string
	// summarizer, when set, is notified after each message is persisted so it
	// can refresh the room's rolling summary in the background. Nil when the
	// LLM summary feature is off; all calls on a nil *summary.Service no-op.
	summarizer *summary.Service
}

func newHub(service *chat.Service, logger *slog.Logger) *hub {
	return &hub{
		service:      service,
		logger:       logger,
		rooms:        make(map[string]map[*client]struct{}),
		agentCtrl:    make(map[string]map[*client]struct{}),
		agentRuntime: make(map[string]agentRuntimeConfig),
		execTokens:   make(map[string]map[string]string),
	}
}

func (h *hub) register(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.ctrl {
		agentID := c.participant.ID
		if h.agentCtrl[agentID] == nil {
			h.agentCtrl[agentID] = make(map[*client]struct{})
		}
		h.agentCtrl[agentID][c] = struct{}{}
		h.logger.Info("agent ctrl connection registered", slog.String("agent_id", agentID))
		return
	}
	if h.rooms[c.roomID] == nil {
		h.rooms[c.roomID] = make(map[*client]struct{})
	}
	h.rooms[c.roomID][c] = struct{}{}
	h.logger.Info("client joined room", slog.String("room_id", c.roomID))
}

func (h *hub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.ctrl {
		agentID := c.participant.ID
		if set := h.agentCtrl[agentID]; set != nil {
			delete(set, c)
			close(c.send)
			if len(set) == 0 {
				delete(h.agentCtrl, agentID)
			}
		}
		h.logger.Info("agent ctrl connection unregistered", slog.String("agent_id", agentID))
		return
	}
	if clients := h.rooms[c.roomID]; clients != nil {
		delete(clients, c)
		close(c.send)
		if len(clients) == 0 {
			delete(h.rooms, c.roomID)
			delete(h.execTokens, c.roomID)
		}
	}
	// Drop a departing executor's token unless another live connection still
	// claims the same agent id in this room.
	if tokens := h.execTokens[c.roomID]; tokens != nil {
		if _, ok := tokens[c.participant.ID]; ok {
			stillPresent := false
			for other := range h.rooms[c.roomID] {
				if other.participant.ID == c.participant.ID {
					stillPresent = true
					break
				}
			}
			if !stillPresent {
				delete(tokens, c.participant.ID)
				if len(tokens) == 0 {
					delete(h.execTokens, c.roomID)
				}
			}
		}
	}
	h.logger.Info("client left room", slog.String("room_id", c.roomID))
}

func (h *hub) Participants(roomID string) []models.Participant {
	h.mu.RLock()
	defer h.mu.RUnlock()

	clients := h.rooms[roomID]
	groups := make(map[string]*models.Participant, len(clients))
	for c := range clients {
		if c.audit {
			continue
		}
		p := c.participant
		key := string(p.Kind) + ":" + p.ID
		group := groups[key]
		if group == nil {
			next := p
			next.ConnectionCount = 0
			next.Connections = nil
			next.Metadata = copyStringMap(p.Metadata)
			groups[key] = &next
			group = &next
		}
		group.ConnectionCount++
		if p.ConnectedAt.Before(group.ConnectedAt) {
			group.ConnectedAt = p.ConnectedAt
		}
		if p.LastSeenAt.After(group.LastSeenAt) {
			group.LastSeenAt = p.LastSeenAt
		}
		if group.Label == "" || group.Label == group.ID {
			group.Label = p.Label
		}
		if group.Metadata == nil {
			group.Metadata = map[string]string{}
		}
		for key, value := range p.Metadata {
			if strings.TrimSpace(value) != "" {
				group.Metadata[key] = value
			}
		}
		group.Connections = append(group.Connections, models.ParticipantConnection{
			ID:          p.ConnectionID,
			Label:       p.Label,
			RemoteAddr:  p.RemoteAddr,
			ConnectedAt: p.ConnectedAt,
			LastSeenAt:  p.LastSeenAt,
			Metadata:    copyStringMap(p.Metadata),
		})
	}
	participants := make([]models.Participant, 0, len(groups))
	for _, participant := range groups {
		sort.Slice(participant.Connections, func(i, j int) bool {
			return participant.Connections[i].ConnectedAt.Before(participant.Connections[j].ConnectedAt)
		})
		participants = append(participants, *participant)
	}
	sort.Slice(participants, func(i, j int) bool {
		if participants[i].Kind != participants[j].Kind {
			return participants[i].Kind == models.SenderKindAgent
		}
		return strings.ToLower(participants[i].Label) < strings.ToLower(participants[j].Label)
	})
	return participants
}

func (h *hub) UserPresence() map[string]models.UserPresence {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := make(map[string]models.UserPresence)
	roomSets := make(map[string]map[string]struct{})
	for roomID, clients := range h.rooms {
		for c := range clients {
			if c.audit {
				continue
			}
			p := c.participant
			if p.Kind != models.SenderKindUser {
				continue
			}
			login := strings.TrimSpace(p.ID)
			if login == "" {
				continue
			}
			key := strings.ToLower(login)
			state := out[key]
			if state.Login == "" {
				state.Login = login
				state.ConnectedAt = p.ConnectedAt
			}
			state.ConnectionCount++
			if state.ConnectedAt.IsZero() || p.ConnectedAt.Before(state.ConnectedAt) {
				state.ConnectedAt = p.ConnectedAt
			}
			if state.LastSeenAt == nil || p.LastSeenAt.After(*state.LastSeenAt) {
				seen := p.LastSeenAt
				state.LastSeenAt = &seen
			}
			if roomSets[key] == nil {
				roomSets[key] = make(map[string]struct{})
			}
			roomSets[key][roomID] = struct{}{}
			out[key] = state
		}
	}
	for key, rooms := range roomSets {
		state := out[key]
		for roomID := range rooms {
			state.RoomIDs = append(state.RoomIDs, roomID)
		}
		sort.Strings(state.RoomIDs)
		out[key] = state
	}
	return out
}

func (h *hub) updateParticipant(c *client, msg models.ChatMessage) {
	if msg.SenderID == "" {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	c.participant.ID = msg.SenderID
	if msg.SenderKind != "" {
		c.participant.Kind = msg.SenderKind
	}
	c.participant.Label = msg.SenderID
	if label := strings.TrimSpace(msg.Metadata["label"]); label != "" {
		c.participant.Label = label
	} else if label := strings.TrimSpace(msg.Metadata["principal_name"]); label != "" {
		c.participant.Label = label
	} else if label := strings.TrimSpace(msg.Metadata["viewer_name"]); label != "" {
		c.participant.Label = label
	}
	c.participant.LastSeenAt = time.Now().UTC()
	// Capture any exec_token reported in presence into the relay-private token
	// store, then keep sensitive keys out of the participant metadata so they
	// never reach the (unauthenticated) /participants endpoint or any broadcast.
	if token := strings.TrimSpace(msg.Metadata["exec_token"]); token != "" {
		if h.execTokens[c.roomID] == nil {
			h.execTokens[c.roomID] = make(map[string]string)
		}
		h.execTokens[c.roomID][msg.SenderID] = token
	}
	if len(msg.Metadata) > 0 {
		if c.participant.Metadata == nil {
			c.participant.Metadata = make(map[string]string, len(msg.Metadata))
		}
		for key, value := range msg.Metadata {
			if strings.TrimSpace(value) == "" || isSensitiveMetadataKey(key) {
				continue
			}
			// owner_login is server-derived from the agent token; never let a
			// client self-report it. It is (re)stamped from c.owner below.
			if key == "owner_login" {
				continue
			}
			c.participant.Metadata[key] = value
		}
	}
	// Keep the authoritative owner_login on the participant so presence
	// broadcasts carry the (display-safe) owner; anonymous connections have
	// none and any client-supplied value was dropped above.
	if c.owner != "" {
		if c.participant.Metadata == nil {
			c.participant.Metadata = map[string]string{}
		}
		c.participant.Metadata["owner_login"] = c.owner
	} else {
		delete(c.participant.Metadata, "owner_login")
	}
}

// bindAgentOwner upserts the durable agents row for a connection that arrived
// with a valid agent token, once the presence message reveals the real agent
// id (SenderID). An agent previously unbound by its owner (revoked) is kept
// anonymous: we drop the injected owner rather than re-binding it.
func (h *hub) bindAgentOwner(ctx context.Context, c *client, msg models.ChatMessage) {
	if h.agents == nil || c.owner == "" {
		return
	}
	agentID := strings.TrimSpace(msg.SenderID)
	if agentID == "" {
		return
	}
	kind := msg.SenderKind
	if kind == "" {
		kind = c.participant.Kind
	}
	if kind != models.SenderKindAgent {
		return
	}
	if existing, err := h.agents.GetAgent(ctx, agentID); err == nil && existing != nil && existing.Revoked {
		h.mu.Lock()
		c.owner = ""
		delete(c.participant.Metadata, "owner_login")
		h.mu.Unlock()
		h.logger.Info("agent left anonymous: binding revoked by owner",
			slog.String("agent_id", agentID))
		return
	}
	label := strings.TrimSpace(msg.Metadata["label"])
	if label == "" {
		label = c.participant.Label
	}
	provider := strings.TrimSpace(msg.Metadata["provider"])
	if err := h.agents.UpsertAgent(ctx, models.Agent{
		AgentID:    agentID,
		OwnerLogin: c.owner,
		Label:      label,
		Provider:   provider,
		LastSeenAt: time.Now().UTC(),
	}); err != nil {
		h.logger.Warn("upsert agent binding failed",
			slog.String("agent_id", agentID), slog.Any("error", err))
		return
	}
	// Now that the agent is bound and online, push any server-managed config so
	// it takes effect on the next reply. Delivered only on this connection —
	// never broadcast, never persisted (see sendConfigUpdateToClient).
	if !c.tokenTemporary {
		h.pushAgentConfig(ctx, c, agentID)
	}
}

// ErrCommandMissingTarget is returned when a command message lacks the
// target_id required to route an exec token to a single recipient.
var ErrCommandMissingTarget = errors.New("command messages must specify target_id")

// sensitiveMetadataKeys lists metadata fields that must never enter
// storage or non-targeted broadcasts. They are delivered only to the
// message's target client over WebSocket.
var sensitiveMetadataKeys = []string{"exec_token"}

func (h *hub) Publish(ctx context.Context, roomID string, msg models.ChatMessage) (models.ChatMessage, error) {
	// A command must address a single executor; targetless commands can never
	// be delivered to one.
	if msg.Type == models.MessageTypeCommand && strings.TrimSpace(msg.TargetID) == "" {
		return models.ChatMessage{}, ErrCommandMissingTarget
	}
	// Clients may never supply sensitive metadata (e.g. exec_token); the relay
	// is the sole source. Strip anything they sent before it can be stored or
	// broadcast.
	msg = redactSensitive(msg)
	msg = h.suppressAgentPeerReplyUnlessMentioned(roomID, msg)
	msg = h.retargetAgentPeerMention(roomID, msg)

	stored, err := h.service.Add(ctx, roomID, msg)
	if err != nil {
		return models.ChatMessage{}, err
	}

	// Let the rolling-summary service know a message landed; it decides
	// whether to refresh the room digest in the background. No-op when the
	// LLM summary feature is off.
	h.summarizer.Notify(roomID, stored.Type)

	// Relay-mediated credential injection: a targeted command gets the
	// recipient executor's token (recorded from its presence) stamped onto the
	// private copy delivered only to that executor. Everyone else, and storage,
	// sees the redacted message.
	if stored.Type == models.MessageTypeCommand {
		if token := h.execToken(roomID, stored.TargetID); token != "" {
			private := stored
			private.Metadata = mergeSensitive(stored.Metadata, map[string]string{"exec_token": token})
			h.broadcastTargeted(roomID, stored, private, stored.TargetID)
			return stored, nil
		}
	}

	h.broadcastTargeted(roomID, stored, stored, "")
	return stored, nil
}

func (h *hub) suppressAgentPeerReplyUnlessMentioned(roomID string, msg models.ChatMessage) models.ChatMessage {
	if msg.Type != "" && msg.Type != models.MessageTypeChat {
		return msg
	}
	if msg.SenderKind != models.SenderKindAgent || !msg.ReplyRequested || msg.TurnBudget <= 0 {
		return msg
	}
	if !h.isLiveChatAgent(roomID, msg.TargetID) {
		return msg
	}
	if explicitlyMentionsAgent(msg.Content, msg.TargetID) {
		return msg
	}
	h.logger.Info("suppressing agent peer auto reply",
		slog.String("room_id", roomID),
		slog.String("from", msg.SenderID),
		slog.String("target", msg.TargetID))
	msg.ReplyRequested = false
	return msg
}

func (h *hub) retargetAgentPeerMention(roomID string, msg models.ChatMessage) models.ChatMessage {
	if msg.Type != "" && msg.Type != models.MessageTypeChat {
		return msg
	}
	if msg.SenderKind != models.SenderKindAgent || !msg.ReplyRequested || msg.TurnBudget <= 0 {
		return msg
	}
	if h.isLiveChatAgent(roomID, msg.TargetID) {
		return msg
	}
	if target := h.mentionedLiveChatAgent(roomID, msg.SenderID, msg.Content); target != "" {
		h.logger.Info("retargeting agent peer mention",
			slog.String("room_id", roomID),
			slog.String("from", msg.SenderID),
			slog.String("old_target", msg.TargetID),
			slog.String("new_target", target))
		msg.TargetID = target
	}
	return msg
}

func explicitlyMentionsAgent(content, agentID string) bool {
	content = strings.TrimSpace(content)
	agentID = strings.TrimSpace(agentID)
	if content == "" || agentID == "" {
		return false
	}
	return strings.Contains(content, "@"+agentID)
}

func (h *hub) isLiveChatAgent(roomID, agentID string) bool {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[roomID] {
		if c.participant.Kind != models.SenderKindAgent || c.participant.ID != agentID {
			continue
		}
		return !strings.EqualFold(strings.TrimSpace(c.participant.Metadata["mode"]), "executor")
	}
	return false
}

func (h *hub) mentionedLiveChatAgent(roomID, senderID, content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	type candidate struct {
		id  string
		pos int
	}
	candidates := make([]candidate, 0)
	h.mu.RLock()
	for c := range h.rooms[roomID] {
		if c.participant.Kind != models.SenderKindAgent {
			continue
		}
		id := strings.TrimSpace(c.participant.ID)
		if id == "" || id == strings.TrimSpace(senderID) {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(c.participant.Metadata["mode"]), "executor") {
			continue
		}
		if pos := strings.Index(content, id); pos >= 0 {
			candidates = append(candidates, candidate{id: id, pos: pos})
		}
	}
	h.mu.RUnlock()
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].pos != candidates[j].pos {
			return candidates[i].pos < candidates[j].pos
		}
		return candidates[i].id < candidates[j].id
	})
	return candidates[0].id
}

// execToken returns the exec_token an executor reported via presence for the
// given room and agent id, or "" if none is known.
func (h *hub) execToken(roomID, agentID string) string {
	if strings.TrimSpace(agentID) == "" {
		return ""
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if tokens := h.execTokens[roomID]; tokens != nil {
		return tokens[agentID]
	}
	return ""
}

func (h *hub) broadcastTargeted(roomID string, public models.ChatMessage, private models.ChatMessage, targetID string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[roomID] {
		var out outgoing = public
		if targetID != "" && c.participant.Kind == models.SenderKindAgent && c.participant.ID == targetID {
			out = private
		}
		select {
		case c.send <- out:
		default:
			h.logger.Warn("dropping slow websocket client", slog.String("room_id", roomID))
		}
	}
}

// accessDecisionEnvelope is the WebSocket payload sent to a requester
// when an owner approves or denies their pending join request.
type accessDecisionEnvelope struct {
	Type        string `json:"type"`
	RequestID   string `json:"request_id"`
	Status      string `json:"status"`
	Persistence string `json:"persistence,omitempty"`
}

// broadcastAccessDecision delivers the access_decision envelope to any
// WebSocket client whose participant identity matches the requester.
// We match on participant.ID == login when the requester logged in,
// and otherwise on the connection-level metadata anon id when present.
func (h *hub) broadcastAccessDecision(roomID string, req models.AccessRequest) {
	if h == nil {
		return
	}
	envelope := accessDecisionEnvelope{
		Type:      "access_decision",
		RequestID: req.ID,
		Status:    string(req.Status),
	}
	if req.Persistence != nil {
		envelope.Persistence = string(*req.Persistence)
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.rooms[roomID] {
		if !accessDecisionMatchesClient(req, c) {
			continue
		}
		select {
		case c.send <- envelope:
		default:
			h.logger.Warn("dropping slow websocket client during access decision",
				slog.String("room_id", roomID))
		}
	}
}

func accessDecisionMatchesClient(req models.AccessRequest, c *client) bool {
	if req.RequesterLogin != nil && *req.RequesterLogin != "" {
		// Browser viewers carry their stable principal id in participant.ID;
		// when GitHub login was set as principal_id by the frontend this
		// will match. We also match against metadata principal_login when
		// the frontend separates login from principal_id.
		if c.participant.ID == *req.RequesterLogin {
			return true
		}
		if c.participant.Metadata["principal_login"] == *req.RequesterLogin {
			return true
		}
	}
	if req.RequesterAnonID != "" {
		if c.participant.Metadata["anon_id"] == req.RequesterAnonID {
			return true
		}
	}
	return false
}

func isSensitiveMetadataKey(key string) bool {
	return slices.Contains(sensitiveMetadataKeys, key)
}

func hasSensitiveMetadata(metadata map[string]string) bool {
	for _, key := range sensitiveMetadataKeys {
		if strings.TrimSpace(metadata[key]) != "" {
			return true
		}
	}
	return false
}

func redactSensitive(msg models.ChatMessage) models.ChatMessage {
	if !hasSensitiveMetadata(msg.Metadata) {
		return msg
	}
	msg.Metadata = copyStringMap(msg.Metadata)
	for _, key := range sensitiveMetadataKeys {
		delete(msg.Metadata, key)
	}
	return msg
}

func mergeSensitive(base map[string]string, source map[string]string) map[string]string {
	out := copyStringMap(base)
	if out == nil {
		out = make(map[string]string, len(sensitiveMetadataKeys))
	}
	for _, key := range sensitiveMetadataKeys {
		if value := strings.TrimSpace(source[key]); value != "" {
			out[key] = source[key]
		}
	}
	return out
}

// outgoing carries any JSON-encodable payload to a client over its
// WebSocket. Today this is either a models.ChatMessage or an
// accessDecisionEnvelope; the writeLoop just JSON-encodes whatever
// lands in the channel.
type outgoing = any

type client struct {
	hub         *hub
	roomID      string
	conn        *websocket.Conn
	send        chan outgoing
	participant models.Participant
	audit       bool
	// ctrl marks this as an agent-level control connection (not bound to a room).
	// When true, roomID is always "".
	ctrl bool
	// owner is the GitHub login this connection's agent token resolved to at
	// handshake; empty for anonymous connections. It is server-derived and
	// authoritative — clients can never set their own owner_login via metadata.
	owner string
	// tokenRoomID scopes temporary skill tokens to a single room. Empty means
	// this connection used a normal long-lived token or no token.
	tokenRoomID string
	// tokenTemporary marks room-scoped skill tokens so privileged per-agent
	// config is not pushed to an untrusted temporary join.
	tokenTemporary bool
}

func newClient(hub *hub, roomID string, conn *websocket.Conn, remoteAddr string, query map[string][]string, audit bool) *client {
	now := time.Now().UTC()
	connectionID := firstQuery(query, "client_id")
	if connectionID == "" {
		connectionID = id.New("conn")
	}
	kind := models.SenderKind(firstQuery(query, "client_kind"))
	if kind == "" {
		kind = models.SenderKindUser
	}
	label := firstQuery(query, "client_label")
	if label == "" {
		label = connectionID
	}
	participantID := connectionID
	if kind == models.SenderKindUser {
		participantID = firstQuery(query, "principal_id")
		if participantID == "" {
			participantID = label
		}
		if principalLabel := firstQuery(query, "principal_label"); principalLabel != "" {
			label = principalLabel
		}
		if label == "" {
			label = participantID
		}
	}
	metadata := map[string]string{}
	if provider := firstQuery(query, "provider"); provider != "" {
		metadata["provider"] = provider
	}
	if device := firstQuery(query, "device"); device != "" {
		metadata["device"] = device
	}
	if principalEmail := firstQuery(query, "principal_email"); principalEmail != "" {
		metadata["principal_email"] = principalEmail
	}
	if principalName := firstQuery(query, "principal_name"); principalName != "" {
		metadata["principal_name"] = principalName
	}
	metadata["connection_id"] = connectionID

	return &client{
		hub:    hub,
		roomID: roomID,
		conn:   conn,
		send:   make(chan outgoing, 64),
		audit:  audit,
		participant: models.Participant{
			ID:              participantID,
			RoomID:          roomID,
			Kind:            kind,
			Label:           label,
			ConnectionID:    connectionID,
			ConnectionCount: 1,
			RemoteAddr:      remoteAddr,
			ConnectedAt:     now,
			LastSeenAt:      now,
			Metadata:        metadata,
		},
	}
}

func copyStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

// 心跳参数：writeLoop 每 pingInterval 发一次 ping，读侧要求 pongWait 内有
// 任何帧（pong/数据）到达，否则判定客户端假死并踢出。此前只发 ping 不验
// pong，一个冻结的客户端（如 Windows 控制台 QuickEdit 卡死的 executor）
// 会以"在线"状态永久滞留 participants。
const (
	clientPingInterval = 30 * time.Second
	clientPongWait     = 75 * time.Second
	clientWriteWait    = 10 * time.Second
)

func (c *client) readLoop(ctx context.Context) {
	defer func() {
		c.hub.unregister(c)
		_ = c.conn.Close()
	}()

	_ = c.conn.SetReadDeadline(time.Now().Add(clientPongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(clientPongWait))
	})

	for {
		var msg models.ChatMessage
		if err := c.conn.ReadJSON(&msg); err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				c.hub.logger.Warn("websocket read failed", slog.String("room_id", c.roomID), slog.Any("error", err))
			}
			return
		}
		// 数据帧本身就是活性证明（兼容不回 pong 的极简客户端）。
		_ = c.conn.SetReadDeadline(time.Now().Add(clientPongWait))
		c.hub.updateParticipant(c, msg)
		c.hub.bindAgentOwner(ctx, c, msg)
		if _, err := c.hub.Publish(ctx, c.roomID, msg); err != nil {
			c.hub.logger.Error("publish message failed", slog.String("room_id", c.roomID), slog.Any("error", err))
			return
		}
	}
}

func (c *client) writeLoop() {
	ticker := time.NewTicker(clientPingInterval)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()

	for {
		select {
		case msg, ok := <-c.send:
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			// 写超时：对端接收窗口塞死时不让 writeLoop 永久阻塞。
			_ = c.conn.SetWriteDeadline(time.Now().Add(clientWriteWait))
			if err := c.conn.WriteJSON(msg); err != nil {
				c.hub.logger.Warn("websocket write failed", slog.String("room_id", c.roomID), slog.Any("error", err))
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(clientWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				if !errors.Is(err, websocket.ErrCloseSent) {
					c.hub.logger.Warn("websocket ping failed", slog.String("room_id", c.roomID), slog.Any("error", err))
				}
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func firstQuery(values map[string][]string, key string) string {
	items := values[key]
	if len(items) == 0 {
		return ""
	}
	return strings.TrimSpace(items[0])
}

// newRoomID returns a 96-bit (24-hex) random identifier with no prefix —
// this whole app is rooms, so a `room-` namespace would just be noise.
// CSPRNG failure is fatal because privacy depends entirely on these ids
// being unguessable.
func newRoomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("relay: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b[:])
}

func deriveHTTPBaseURL(r *http.Request) string {
	proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
	scheme := "http"
	if r.TLS != nil || proto == "https" {
		scheme = "https"
	}
	host := r.Host
	if forwardedHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwardedHost != "" {
		host = forwardedHost
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

func (s *Server) publicBaseURL(r *http.Request) string {
	if base := strings.TrimRight(strings.TrimSpace(s.cfg.PublicBaseURL), "/"); base != "" {
		return base
	}
	return deriveHTTPBaseURL(r)
}

func normalizeDownloadArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return ""
	}
}

func unixInstallScript(baseURL string) string {
	return fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "agent-room: unsupported architecture: $arch" >&2; exit 1 ;;
esac

case "$os" in
  darwin|linux) ;;
  *) echo "agent-room: this installer supports macOS and Linux. On Windows, download agent-room.exe from /downloads/windows instead." >&2; exit 1 ;;
esac

install_dir="${AGENT_ROOM_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$install_dir"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

url="${AGENT_ROOM_DOWNLOAD_BASE:-%[1]s}/downloads/agent-room?os=${os}&arch=${arch}"
echo "Downloading agent-room ${os}/${arch}..."
curl -fsSL "$url" -o "$tmp"
chmod +x "$tmp"
mv "$tmp" "$install_dir/agent-room"

echo "agent-room installed to $install_dir/agent-room"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "Add this to your shell profile if needed: export PATH=\"$install_dir:$PATH\"" ;;
esac
`, strings.TrimRight(baseURL, "/"))
}
