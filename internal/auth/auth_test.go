package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileProviderOIDCSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	data := `{"https://auth.x.ai::client":{"key":"eyJabc.token","auth_mode":"oidc","user_id":"u-1","expires_at":"2026-07-11T10:33:39.565925300Z"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := (FileProvider{Path: path}).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if session.Token != "eyJabc.token" || session.UserID != "u-1" {
		t.Fatalf("unexpected session: %#v", session)
	}
	if session.ExpiresAt == nil || *session.ExpiresAt < 1_700_000_000 {
		t.Fatalf("expiry not parsed: %#v", session.ExpiresAt)
	}
}

func TestFileProviderLegacySchema(t *testing.T) {
	raw := map[string]any{"tokens": map[string]any{"access_token": "legacy", "expires_at": float64(1735689600)}, "user_id": "u-2"}
	if got := extractToken(raw); got != "legacy" {
		t.Fatalf("token = %q", got)
	}
	if got := extractUserID(raw); got != "u-2" {
		t.Fatalf("user id = %q", got)
	}
	if got := extractExpires(raw); got != 1735689600 {
		t.Fatalf("expires = %f", got)
	}
}

func TestStoreCachesSession(t *testing.T) {
	p := &countingProvider{}
	store := NewStore(p)
	if _, err := store.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("acquire calls = %d", p.calls)
	}
}

type countingProvider struct{ calls int }

func (p *countingProvider) Acquire(context.Context) (Session, error) {
	p.calls++
	return Session{Token: "token", ObtainedAt: float64(time.Now().Unix())}, nil
}
