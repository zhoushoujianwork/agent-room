package config

import "testing"

func TestLoadBuildHubSSOAliases(t *testing.T) {
	t.Setenv("AGENT_ROOM_SSO_APP_ID", "")
	t.Setenv("AGENT_ROOM_SSO_APP_SECRET", "")
	t.Setenv("AGENT_ROOM_SSO_AUTHORIZE_URL", "")
	t.Setenv("BUILDHUB_APP_ID", "agent-room-zhoushoujian.build.patsnap.info")
	t.Setenv("BUILDHUB_APP_SECRET", "buildhub-secret")
	t.Setenv("BUILDHUB_SSO_AUTHORIZE_URL", "https://build.patsnap.info/api/v1/auth/external/authorize")

	cfg := Load()
	if cfg.SSO.AppID != "agent-room-zhoushoujian.build.patsnap.info" {
		t.Fatalf("SSO.AppID = %q", cfg.SSO.AppID)
	}
	if cfg.SSO.AppSecret != "buildhub-secret" {
		t.Fatalf("SSO.AppSecret = %q", cfg.SSO.AppSecret)
	}
	if cfg.SSO.AuthorizeURL != "https://build.patsnap.info/api/v1/auth/external/authorize" {
		t.Fatalf("SSO.AuthorizeURL = %q", cfg.SSO.AuthorizeURL)
	}
	if !cfg.SSO.Enabled() {
		t.Fatal("SSO.Enabled() = false, want true")
	}
}

func TestLoadAgentRoomSSOTakesPrecedence(t *testing.T) {
	t.Setenv("AGENT_ROOM_SSO_APP_ID", "agent-room-explicit")
	t.Setenv("AGENT_ROOM_SSO_APP_SECRET", "explicit-secret")
	t.Setenv("AGENT_ROOM_SSO_AUTHORIZE_URL", "https://sso.example.com/authorize")
	t.Setenv("BUILDHUB_APP_ID", "agent-room-buildhub")
	t.Setenv("BUILDHUB_APP_SECRET", "buildhub-secret")
	t.Setenv("BUILDHUB_SSO_AUTHORIZE_URL", "https://build.patsnap.info/api/v1/auth/external/authorize")

	cfg := Load()
	if cfg.SSO.AppID != "agent-room-explicit" {
		t.Fatalf("SSO.AppID = %q, want explicit value", cfg.SSO.AppID)
	}
	if cfg.SSO.AppSecret != "explicit-secret" {
		t.Fatalf("SSO.AppSecret = %q, want explicit value", cfg.SSO.AppSecret)
	}
	if cfg.SSO.AuthorizeURL != "https://sso.example.com/authorize" {
		t.Fatalf("SSO.AuthorizeURL = %q, want explicit value", cfg.SSO.AuthorizeURL)
	}
}
