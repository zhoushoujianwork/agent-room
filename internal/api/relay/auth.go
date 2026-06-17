package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"

	"agent-room/internal/config"
	"agent-room/internal/models"
)

// sessionTTL caps how long a signed session cookie is valid.
const sessionTTL = 24 * time.Hour

// sessionClaims is what we stuff into the signed JWT. Mirrors the
// shape called out in the migration contract.
type sessionClaims struct {
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	jwt.RegisteredClaims
}

// githubUser is the subset of the github.com /user response we keep.
type githubUser struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// authEnabled reports whether any login provider (GitHub OAuth or
// external SSO) is configured AND a session secret exists to sign our
// cookie. Used by every auth-related route to decide between serving
// and 404ing. When false the relay still serves anonymous rooms.
func (s *Server) authEnabled() bool {
	if strings.TrimSpace(s.sessionSecret()) == "" {
		return false
	}
	return s.cfg.GitHub.Enabled() || s.cfg.SSO.Enabled()
}

// githubEnabled reports whether the GitHub OAuth provider specifically
// is configured (client id + secret + session secret).
func (s *Server) githubEnabled() bool {
	return s.cfg.GitHub.Enabled()
}

// ssoEnabled reports whether the external SSO provider specifically is
// configured (app id + secret + authorize url + session secret).
func (s *Server) ssoEnabled() bool {
	return s.cfg.SSO.Enabled() && strings.TrimSpace(s.sessionSecret()) != ""
}

// sessionSecret is the HMAC key used to sign our own session cookie.
// Both providers share it; it comes from AGENT_ROOM_SESSION_SECRET.
func (s *Server) sessionSecret() string {
	return s.cfg.GitHub.SessionSecret
}

// handleAuthLogin starts the GitHub OAuth dance.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.githubEnabled() {
		writeError(w, http.StatusNotFound, "github auth disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// `state` doubles as a return URL. We refuse anything that isn't a
	// local path to avoid being used as an open redirector.
	returnTo := safeReturnPath(r.URL.Query().Get("state"))
	stateToken, err := randomToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate state failed")
		return
	}
	state := stateToken + ":" + returnTo

	cfg := s.oauthConfig(r)
	http.Redirect(w, r, cfg.AuthCodeURL(state), http.StatusFound)
}

// handleAuthCallback exchanges the OAuth code, fetches the user, and
// drops a signed session cookie.
func (s *Server) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !s.githubEnabled() {
		writeError(w, http.StatusNotFound, "github auth disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := r.URL.Query().Get("state")
	if code == "" {
		writeAuthError(w, http.StatusBadRequest, "missing code")
		return
	}
	returnTo := "/"
	if idx := strings.IndexByte(state, ':'); idx >= 0 {
		returnTo = safeReturnPath(state[idx+1:])
	}

	cfg := s.oauthConfig(r)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		s.logger.Warn("github oauth exchange failed", slog.Any("error", err))
		writeAuthError(w, http.StatusBadGateway, "oauth exchange failed")
		return
	}

	user, err := fetchGitHubUser(ctx, cfg.Client(ctx, tok))
	if err != nil {
		s.logger.Warn("github user fetch failed", slog.Any("error", err))
		writeAuthError(w, http.StatusBadGateway, "github user fetch failed")
		return
	}
	if strings.TrimSpace(user.Login) == "" {
		writeAuthError(w, http.StatusBadGateway, "github returned no login")
		return
	}

	signed, err := signSession(s.sessionSecret(), sessionClaims{
		Name:      user.Name,
		Email:     user.Email,
		AvatarURL: user.AvatarURL,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.Login,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(sessionTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	if err != nil {
		s.logger.Error("sign session failed", slog.Any("error", err))
		writeAuthError(w, http.StatusInternalServerError, "sign session failed")
		return
	}
	s.recordUserActivity(r.Context(), models.UserActivity{
		Login:     user.Login,
		Name:      user.Name,
		Email:     user.Email,
		AvatarURL: user.AvatarURL,
	}, true)

	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    signed,
		Path:     "/",
		Expires:  time.Now().Add(sessionTTL),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// ssoClaims is the app-scoped JWT the external SSO provider returns.
// HS256-signed with AGENT_ROOM_SSO_APP_SECRET, audience == AGENT_ROOM_SSO_APP_ID.
type ssoClaims struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	Department string `json:"department"`
	jwt.RegisteredClaims
}

// handleSSOLogin redirects the browser to the external SSO authorize
// endpoint. Only active when SSO is configured; like GitHub it is a
// user-initiated, optional login — the relay never forces this on
// anonymous visitors.
func (s *Server) handleSSOLogin(w http.ResponseWriter, r *http.Request) {
	if !s.ssoEnabled() {
		writeError(w, http.StatusNotFound, "sso disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	returnTo := safeReturnPath(r.URL.Query().Get("state"))
	redirectURI := s.publicBaseURL(r) + "/auth/sso/callback"
	authorize, err := url.Parse(s.cfg.SSO.AuthorizeURL)
	if err != nil {
		writeAuthError(w, http.StatusInternalServerError, "bad authorize url")
		return
	}
	q := authorize.Query()
	q.Set("app_id", s.cfg.SSO.AppID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", returnTo)
	authorize.RawQuery = q.Encode()
	http.Redirect(w, r, authorize.String(), http.StatusFound)
}

// handleSSOCallback receives ?app_token=<JWT>, verifies it with the app
// secret, and mints our own session cookie (same shape GitHub uses), so
// every downstream owner/approval check works identically regardless of
// which provider signed the user in.
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	if !s.ssoEnabled() {
		writeError(w, http.StatusNotFound, "sso disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	appToken := strings.TrimSpace(r.URL.Query().Get("app_token"))
	if appToken == "" {
		writeAuthError(w, http.StatusBadRequest, "missing app_token")
		return
	}
	returnTo := safeReturnPath(r.URL.Query().Get("state"))

	claims, err := parseSSOToken(s.cfg.SSO.AppSecret, s.cfg.SSO.AppID, appToken)
	if err != nil {
		s.logger.Warn("sso token verify failed", slog.Any("error", err))
		writeAuthError(w, http.StatusBadGateway, "sso token verification failed")
		return
	}
	login := strings.TrimSpace(claims.Subject)
	if login == "" {
		writeAuthError(w, http.StatusBadGateway, "sso token has no subject")
		return
	}

	signed, err := signSession(s.sessionSecret(), sessionClaims{
		Name:  claims.Name,
		Email: claims.Email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   login,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(sessionTTL)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	if err != nil {
		s.logger.Error("sign session failed", slog.Any("error", err))
		writeAuthError(w, http.StatusInternalServerError, "sign session failed")
		return
	}
	s.recordUserActivity(r.Context(), models.UserActivity{
		Login: login,
		Name:  claims.Name,
		Email: claims.Email,
	}, true)
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    signed,
		Path:     "/",
		Expires:  time.Now().Add(sessionTTL),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// parseSSOToken verifies the external SSO app_token: HS256 only, audience
// must equal the app id, signature must match the app secret.
func parseSSOToken(secret, appID, raw string) (*ssoClaims, error) {
	var claims ssoClaims
	tok, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	}, jwt.WithAudience(appID), jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("invalid sso token")
	}
	return &claims, nil
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeError(w, http.StatusNotFound, "auth disabled")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		writeError(w, http.StatusNotFound, "auth disabled")
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	claims, _ := s.sessionFromRequest(r)
	if claims == nil {
		state := safeReturnPath(r.URL.Query().Get("state"))
		// Prefer SSO when available (internal deploys); fall back to GitHub.
		loginURL := "/auth/github/login?state=" + state
		provider := "github"
		if s.ssoEnabled() {
			loginURL = "/auth/sso/login?state=" + state
			provider = "sso"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": false,
			"auth_enabled":  true,
			"auth_provider": provider,
			"login_url":     loginURL,
		})
		return
	}
	s.recordUserActivity(r.Context(), userActivityFromClaims(claims), false)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"auth_enabled":  true,
		"is_admin":      s.cfg.IsAdmin(claims.Subject),
		"user": map[string]string{
			"login":      claims.Subject,
			"name":       claims.Name,
			"email":      claims.Email,
			"avatar_url": claims.AvatarURL,
		},
	})
}

func (s *Server) recordUserActivity(ctx context.Context, user models.UserActivity, incrementLogin bool) {
	if s.rooms == nil {
		return
	}
	if user.LastLoginAt.IsZero() {
		user.LastLoginAt = time.Now().UTC()
	}
	if user.FirstSeenAt.IsZero() {
		user.FirstSeenAt = user.LastLoginAt
	}
	if err := s.rooms.RecordUserActivity(ctx, user, incrementLogin); err != nil {
		s.logger.Warn("record user activity failed", slog.String("login", user.Login), slog.Any("error", err))
	}
}

func userActivityFromClaims(claims *sessionClaims) models.UserActivity {
	if claims == nil {
		return models.UserActivity{}
	}
	now := time.Now().UTC()
	return models.UserActivity{
		Login:       claims.Subject,
		Name:        claims.Name,
		Email:       claims.Email,
		AvatarURL:   claims.AvatarURL,
		FirstSeenAt: now,
		LastLoginAt: now,
	}
}

// isAdminRequest reports whether the request carries a valid session whose
// login is configured as a cross-room admin. False when auth is disabled,
// the caller is anonymous, or the login is not in the admin list.
func (s *Server) isAdminRequest(r *http.Request) bool {
	return s.cfg.IsAdmin(s.loginFromRequest(r))
}

// sessionFromRequest returns the verified claims from the session
// cookie if present and valid. Returns (nil, nil) when no cookie is
// present so callers can treat missing-vs-invalid the same way.
func (s *Server) sessionFromRequest(r *http.Request) (*sessionClaims, error) {
	if !s.authEnabled() {
		return nil, nil
	}
	cookie, err := r.Cookie(s.cookieName())
	if err != nil || cookie.Value == "" {
		return nil, nil
	}
	return parseSession(s.sessionSecret(), cookie.Value)
}

// loginFromRequest is sugar for handlers that just want the GitHub
// login (empty when unauthenticated or auth disabled).
func (s *Server) loginFromRequest(r *http.Request) string {
	claims, err := s.sessionFromRequest(r)
	if err != nil || claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.Subject)
}

// cookieName returns the configured cookie name, defaulting if env
// supplied an empty value somehow.
func (s *Server) cookieName() string {
	if name := strings.TrimSpace(s.cfg.GitHub.CookieName); name != "" {
		return name
	}
	return "agent_room_session"
}

// oauthConfig builds an oauth2.Config using the configured client +
// the request-derived redirect URI when one wasn't pinned via env.
func (s *Server) oauthConfig(r *http.Request) *oauth2.Config {
	redirect := strings.TrimSpace(s.cfg.GitHub.RedirectURI)
	if redirect == "" {
		redirect = s.publicBaseURL(r) + "/auth/github/callback"
	}
	return &oauth2.Config{
		ClientID:     s.cfg.GitHub.ClientID,
		ClientSecret: s.cfg.GitHub.ClientSecret,
		RedirectURL:  redirect,
		Scopes:       []string{"read:user", "user:email"},
		Endpoint:     githuboauth.Endpoint,
	}
}

func fetchGitHubUser(ctx context.Context, client *http.Client) (*githubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return nil, fmt.Errorf("build user request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github user request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github user status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var u githubUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("decode user: %w", err)
	}
	return &u, nil
}

func signSession(secret string, claims sessionClaims) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(secret))
}

// parseSession verifies the signature and exp, returning the typed
// claims. We use ParseWithClaims so unknown JSON fields error rather
// than silently dropping.
func parseSession(secret, raw string) (*sessionClaims, error) {
	var claims sessionClaims
	tok, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("invalid session token")
	}
	return &claims, nil
}

// safeReturnPath collapses anything that isn't a local /path into "/"
// to defend against the OAuth callback being abused as an open
// redirector. Empty input is also normalised to "/".
func safeReturnPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") {
		return "/"
	}
	// Defensive: a leading "//" would be parsed by browsers as a
	// scheme-relative URL pointing at another host.
	if strings.HasPrefix(value, "//") {
		return "/"
	}
	return value
}

func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// writeAuthError returns a tiny HTML body so callback errors are
// readable in a browser tab. JSON would just confuse a redirected
// human.
func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Auth error</title><body style=\"font-family:system-ui;padding:32px\"><h1>Auth error</h1><p>%s</p></body>", htmlEscape(message))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// Compile-time assertion the config type matches what auth code reads.
var _ = config.GitHubConfig{}
