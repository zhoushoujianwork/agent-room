package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"agent-room/internal/config"
	"agent-room/internal/io/memory"
	"agent-room/internal/models"
	"agent-room/internal/service/chat"
)

// helper: build a server with optional GitHub config.
func newTestServer(t *testing.T, gh config.GitHubConfig) *Server {
	t.Helper()
	store := memory.NewStore()
	cfg := config.Config{GitHub: gh}
	return NewServer(cfg, chat.NewService(store), store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// signedCookie returns a session cookie for the given login.
func signedCookie(t *testing.T, secret, login string) *http.Cookie {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, sessionClaims{
		Name: login,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   login,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign cookie: %v", err)
	}
	return &http.Cookie{Name: "agent_room_session", Value: signed, Path: "/"}
}

func TestAuthDisabled404s(t *testing.T) {
	s := newTestServer(t, config.GitHubConfig{}) // empty -> disabled

	for _, path := range []string{
		"/auth/github/login?state=/",
		"/auth/github/callback?code=abc",
		"/v1/me",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 when auth disabled; body=%s", path, rr.Code, rr.Body.String())
		}
	}

	// POST /auth/logout 404.
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("POST /auth/logout = %d, want 404 when auth disabled", rr.Code)
	}

	// Sanity: relay still serves / and /v1/rooms.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / with auth disabled = %d, want 200", rr.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST /v1/rooms with auth disabled = %d, want 201", rr.Code)
	}
}

func TestCreateRoomRequiresSignInWhenAuthEnabled(t *testing.T) {
	s := newTestServer(t, config.GitHubConfig{
		ClientID: "id", ClientSecret: "sec", SessionSecret: "secret-32-bytes-or-so-here-12345",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCreateRoomSignedInTagsOwner(t *testing.T) {
	secret := "secret-32-bytes-or-so-here-12345"
	s := newTestServer(t, config.GitHubConfig{
		ClientID: "id", ClientSecret: "sec", SessionSecret: secret, CookieName: "agent_room_session",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var got models.Room
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OwnerLogin == nil || *got.OwnerLogin != "alice" {
		t.Fatalf("owner = %v, want alice", got.OwnerLogin)
	}
	if !got.Gated {
		t.Fatalf("gated = false, want true for signed-in room")
	}
}

func TestMyRoomsEndpointPersistsCreatedAndJoinedRooms(t *testing.T) {
	secret := "secret-32-bytes-or-so-here-12345"
	s := newTestServer(t, config.GitHubConfig{
		ClientID: "id", ClientSecret: "sec", SessionSecret: secret, CookieName: "agent_room_session",
	})

	create := func(login string) models.Room {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
		req.AddCookie(signedCookie(t, secret, login))
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create room as %s = %d, want 201; body=%s", login, rr.Code, rr.Body.String())
		}
		var room models.Room
		if err := json.NewDecoder(rr.Body).Decode(&room); err != nil {
			t.Fatalf("decode room: %v", err)
		}
		return room
	}
	listMine := func(login string) []models.UserRoom {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/me/rooms", nil)
		req.AddCookie(signedCookie(t, secret, login))
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("list rooms as %s = %d, want 200; body=%s", login, rr.Code, rr.Body.String())
		}
		var rooms []models.UserRoom
		if err := json.NewDecoder(rr.Body).Decode(&rooms); err != nil {
			t.Fatalf("decode rooms: %v", err)
		}
		return rooms
	}

	aliceRoom := create("alice")
	bobRoom := create("bob")

	rooms := listMine("alice")
	if len(rooms) != 1 || rooms[0].ID != aliceRoom.ID {
		t.Fatalf("alice rooms after create = %+v, want only %s", rooms, aliceRoom.ID)
	}

	req := httptest.NewRequest(http.MethodPut, "/v1/me/rooms/"+bobRoom.ID, nil)
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("remember bob room as alice = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	rooms = listMine("alice")
	if len(rooms) != 2 {
		t.Fatalf("alice rooms after joining bob room len = %d, want 2: %+v", len(rooms), rooms)
	}
	seen := map[string]bool{}
	for _, room := range rooms {
		seen[room.ID] = true
		if room.LastOpenedAt.IsZero() {
			t.Fatalf("room %s has zero last_opened_at", room.ID)
		}
	}
	if !seen[aliceRoom.ID] || !seen[bobRoom.ID] {
		t.Fatalf("alice rooms = %+v, want created %s and joined %s", rooms, aliceRoom.ID, bobRoom.ID)
	}

	req = httptest.NewRequest(http.MethodDelete, "/v1/me/rooms/"+bobRoom.ID, nil)
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("forget bob room as alice = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	rooms = listMine("alice")
	if len(rooms) != 1 || rooms[0].ID != aliceRoom.ID {
		t.Fatalf("alice rooms after forget = %+v, want only %s", rooms, aliceRoom.ID)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/"+bobRoom.ID, nil)
	req.AddCookie(signedCookie(t, secret, "bob"))
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bob room was deleted by alice forget: get = %d; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/me/rooms", nil)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("anonymous my rooms = %d, want 403", rr.Code)
	}
}

func TestGetRoomRequiresSignInWhenAuthEnabled(t *testing.T) {
	secret := "secret-32-bytes-or-so-here-12345"
	s := newTestServer(t, config.GitHubConfig{
		ClientID: "id", ClientSecret: "sec", SessionSecret: secret, CookieName: "agent_room_session",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var room models.Room
	if err := json.NewDecoder(rr.Body).Decode(&room); err != nil {
		t.Fatalf("decode room: %v", err)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/"+room.ID, nil)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("anonymous GET room = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/"+room.ID, nil)
	req.AddCookie(signedCookie(t, secret, "bob"))
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("signed-in GET room = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestPatchRoomOwnerOnly(t *testing.T) {
	secret := "secret-32-bytes-or-so-here-12345"
	s := newTestServer(t, config.GitHubConfig{
		ClientID: "id", ClientSecret: "sec", SessionSecret: secret, CookieName: "agent_room_session",
	})

	// Owner creates a room.
	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status = %d", rr.Code)
	}
	var room models.Room
	_ = json.NewDecoder(rr.Body).Decode(&room)

	// Non-owner PATCH → 403.
	payload, _ := json.Marshal(map[string]any{
		"title":      "new title",
		"github_url": "zhoushoujianwork/agent_room",
	})
	req = httptest.NewRequest(http.MethodPatch, "/v1/rooms/"+room.ID, bytes.NewReader(payload))
	req.AddCookie(signedCookie(t, secret, "bob"))
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-owner PATCH = %d, want 403", rr.Code)
	}

	// Owner PATCH succeeds.
	req = httptest.NewRequest(http.MethodPatch, "/v1/rooms/"+room.ID, bytes.NewReader(payload))
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner PATCH = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var updated models.Room
	_ = json.NewDecoder(rr.Body).Decode(&updated)
	if updated.Title == nil || *updated.Title != "new title" {
		t.Fatalf("title = %v, want new title", updated.Title)
	}
	if updated.GitHubURL == nil || *updated.GitHubURL != "https://github.com/zhoushoujianwork/agent_room" {
		t.Fatalf("github_url = %v", updated.GitHubURL)
	}

	payload, _ = json.Marshal(map[string]any{"github_url": "https://example.com/not-github"})
	req = httptest.NewRequest(http.MethodPatch, "/v1/rooms/"+room.ID, bytes.NewReader(payload))
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid github_url PATCH = %d, want 400", rr.Code)
	}
}

func TestAccessRequestLifecycle(t *testing.T) {
	secret := "secret-32-bytes-or-so-here-12345"
	s := newTestServer(t, config.GitHubConfig{
		ClientID: "id", ClientSecret: "sec", SessionSecret: secret, CookieName: "agent_room_session",
	})

	// Owner creates a gated room.
	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	var room models.Room
	_ = json.NewDecoder(rr.Body).Decode(&room)
	gateBody, _ := json.Marshal(map[string]any{"gated": true})
	req = httptest.NewRequest(http.MethodPatch, "/v1/rooms/"+room.ID, bytes.NewReader(gateBody))
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)

	// Anonymous joiner registers a fake ws client so we can observe
	// the access_decision envelope.
	joinerAnonID := "anon-fixed-123"
	joiner := newFakeClient("anon-principal", models.SenderKindUser)
	joiner.participant.Metadata = map[string]string{"anon_id": joinerAnonID}
	s.hub.rooms[room.ID] = map[*client]struct{}{joiner: {}}

	// Anonymous joiner posts a request.
	createBody, _ := json.Marshal(map[string]any{"label": "Guest One"})
	req = httptest.NewRequest(http.MethodPost, "/v1/rooms/"+room.ID+"/access-requests", bytes.NewReader(createBody))
	req.Header.Set("X-Anon-ID", joinerAnonID)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create request = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var created models.AccessRequest
	_ = json.NewDecoder(rr.Body).Decode(&created)
	if created.Status != models.AccessRequestStatusPending {
		t.Fatalf("status = %s, want pending", created.Status)
	}

	// Owner GETs the list.
	req = httptest.NewRequest(http.MethodGet, "/v1/rooms/"+room.ID+"/access-requests", nil)
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d", rr.Code)
	}
	var list []models.AccessRequest
	_ = json.NewDecoder(rr.Body).Decode(&list)
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}

	// Owner approves + persists.
	patchBody, _ := json.Marshal(map[string]any{"decision": "approve", "persistence": "persist"})
	req = httptest.NewRequest(http.MethodPatch, "/v1/rooms/"+room.ID+"/access-requests/"+created.ID, bytes.NewReader(patchBody))
	req.AddCookie(signedCookie(t, secret, "alice"))
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var patched models.AccessRequest
	_ = json.NewDecoder(rr.Body).Decode(&patched)
	if patched.Status != models.AccessRequestStatusApproved {
		t.Fatalf("status after approve = %s", patched.Status)
	}
	if patched.Persistence == nil || *patched.Persistence != models.AccessRequestPersistencePersist {
		t.Fatalf("persistence = %v, want persist", patched.Persistence)
	}

	// Joiner should have received an access_decision envelope.
	select {
	case msg := <-joiner.send:
		env, ok := msg.(accessDecisionEnvelope)
		if !ok {
			t.Fatalf("expected accessDecisionEnvelope, got %T", msg)
		}
		if env.Type != "access_decision" || env.Status != "approved" || env.Persistence != "persist" {
			t.Fatalf("envelope = %+v", env)
		}
		if env.RequestID != created.ID {
			t.Fatalf("envelope request_id = %s, want %s", env.RequestID, created.ID)
		}
	case <-time.After(time.Second):
		t.Fatalf("did not receive access_decision envelope")
	}
}

func TestAccessRequestDuplicatePending(t *testing.T) {
	secret := "secret-32-bytes-or-so-here-12345"
	s := newTestServer(t, config.GitHubConfig{
		ClientID: "id", ClientSecret: "sec", SessionSecret: secret, CookieName: "agent_room_session",
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	req.AddCookie(signedCookie(t, secret, "owner"))
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create room = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var room models.Room
	_ = json.NewDecoder(rr.Body).Decode(&room)

	anonID := "anon-dup-1"
	// First request: 201.
	req = httptest.NewRequest(http.MethodPost, "/v1/rooms/"+room.ID+"/access-requests", nil)
	req.Header.Set("X-Anon-ID", anonID)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first request = %d", rr.Code)
	}

	// Second request: 409.
	req = httptest.NewRequest(http.MethodPost, "/v1/rooms/"+room.ID+"/access-requests", nil)
	req.Header.Set("X-Anon-ID", anonID)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("dup request = %d, want 409", rr.Code)
	}
}

// -- SSO ----------------------------------------------------------------

// newSSOServer builds a server with only external SSO configured (no
// GitHub). The session secret is shared via GitHubConfig.SessionSecret,
// which is how config.Load wires AGENT_ROOM_SESSION_SECRET.
func newSSOServer(t *testing.T, sessionSecret string, sso config.ExternalSSOConfig) *Server {
	t.Helper()
	store := memory.NewStore()
	cfg := config.Config{
		GitHub: config.GitHubConfig{SessionSecret: sessionSecret, CookieName: "agent_room_session"},
		SSO:    sso,
	}
	return NewServer(cfg, chat.NewService(store), store, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// ssoAppToken signs an external SSO app_token (HS256, aud=appID).
func ssoAppToken(t *testing.T, appSecret, appID, login string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, ssoClaims{
		Email: login + "@example.com",
		Name:  login,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   login,
			Audience:  jwt.ClaimStrings{appID},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	signed, err := tok.SignedString([]byte(appSecret))
	if err != nil {
		t.Fatalf("sign app_token: %v", err)
	}
	return signed
}

func TestSSOMeAdvertisesLoginURL(t *testing.T) {
	s := newSSOServer(t, "secret-32-bytes-or-so-here-12345", config.ExternalSSOConfig{
		AppID:     "agent-room.example.com",
		AppSecret: "app-secret-xyz", AuthorizeURL: "https://sso.example.com/api/v1/auth/external/authorize",
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("/v1/me = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&got)
	if got["auth_enabled"] != true {
		t.Fatalf("auth_enabled = %v, want true", got["auth_enabled"])
	}
	if got["auth_provider"] != "sso" {
		t.Fatalf("auth_provider = %v, want sso", got["auth_provider"])
	}
	if lu, _ := got["login_url"].(string); lu != "/auth/sso/login?state=/" {
		t.Fatalf("login_url = %q, want /auth/sso/login?state=/", lu)
	}
}

func TestSSOLoginRedirectsToAuthorize(t *testing.T) {
	s := newSSOServer(t, "secret-32-bytes-or-so-here-12345", config.ExternalSSOConfig{
		AppID: "myapp", AppSecret: "app-secret-xyz",
		AuthorizeURL: "https://sso.example.com/api/v1/auth/external/authorize",
	})
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/login?state=/", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("login = %d, want 302", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "app_id=myapp") || !strings.Contains(loc, "redirect_uri=") {
		t.Fatalf("Location = %q, missing app_id/redirect_uri", loc)
	}
}

func TestSSOCallbackMintsSessionCookie(t *testing.T) {
	secret := "secret-32-bytes-or-so-here-12345"
	appSecret := "app-secret-xyz"
	appID := "myapp"
	s := newSSOServer(t, secret, config.ExternalSSOConfig{
		AppID: appID, AppSecret: appSecret,
		AuthorizeURL: "https://sso.example.com/api/v1/auth/external/authorize",
	})

	token := ssoAppToken(t, appSecret, appID, "alice")
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/callback?app_token="+token+"&state=/", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("callback = %d, want 302; body=%s", rr.Code, rr.Body.String())
	}
	// Must set a session cookie that the relay can verify.
	var sess *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "agent_room_session" {
			sess = c
		}
	}
	if sess == nil || sess.Value == "" {
		t.Fatalf("no session cookie set")
	}
	claims, err := parseSession(secret, sess.Value)
	if err != nil {
		t.Fatalf("parse minted session: %v", err)
	}
	if claims.Subject != "alice" {
		t.Fatalf("subject = %q, want alice", claims.Subject)
	}

	// The minted cookie should now tag a created room with that owner.
	req = httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	req.AddCookie(sess)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	var room models.Room
	_ = json.NewDecoder(rr.Body).Decode(&room)
	if room.OwnerLogin == nil || *room.OwnerLogin != "alice" {
		t.Fatalf("owner = %v, want alice", room.OwnerLogin)
	}
}

func TestSSOCallbackRejectsBadSignature(t *testing.T) {
	s := newSSOServer(t, "secret-32-bytes-or-so-here-12345", config.ExternalSSOConfig{
		AppID: "myapp", AppSecret: "real-secret",
		AuthorizeURL: "https://sso.example.com/api/v1/auth/external/authorize",
	})
	// Token signed with the WRONG secret.
	bad := ssoAppToken(t, "wrong-secret", "myapp", "attacker")
	req := httptest.NewRequest(http.MethodGet, "/auth/sso/callback?app_token="+bad+"&state=/", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code == http.StatusFound {
		t.Fatalf("callback accepted a bad-signature token (status %d)", rr.Code)
	}
}

func TestSSOOnlyRequiresSignInAndKeepsGitHubDisabled(t *testing.T) {
	// SSO enabled, GitHub not. Anonymous create is rejected, and the GitHub
	// login route stays disabled.
	s := newSSOServer(t, "secret-32-bytes-or-so-here-12345", config.ExternalSSOConfig{
		AppID: "myapp", AppSecret: "app-secret-xyz",
		AuthorizeURL: "https://sso.example.com/api/v1/auth/external/authorize",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/rooms", nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("anon create = %d, want 403", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/auth/github/login?state=/", nil)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("github login with SSO-only = %d, want 404", rr.Code)
	}
}
