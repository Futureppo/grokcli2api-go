package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrNoAuth = errors.New("no auth configured; provide GROK_SESSION_TOKEN, GROK_AUTH_FILE, or GROK_OAUTH_CLIENT_ID")

type Session struct {
	Token      string   `json:"-"`
	Surface    string   `json:"surface"`
	UserID     string   `json:"user_id,omitempty"`
	ObtainedAt float64  `json:"obtained_at"`
	ExpiresAt  *float64 `json:"expires_at"`
}

func (s Session) Expired() bool {
	return s.ExpiresAt != nil && float64(time.Now().Unix()) >= *s.ExpiresAt-60
}

type Provider interface {
	Acquire(context.Context) (Session, error)
}

type FixedProvider struct{ Token, Surface string }

func (p FixedProvider) Acquire(context.Context) (Session, error) {
	if p.Token == "" {
		return Session{}, ErrNoAuth
	}
	return Session{Token: p.Token, Surface: defaultSurface(p.Surface), ObtainedAt: nowFloat()}, nil
}

type FileProvider struct{ Path, Surface string }

func (p FileProvider) Acquire(context.Context) (Session, error) {
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return Session{}, fmt.Errorf("read auth file: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return Session{}, fmt.Errorf("auth file is not valid JSON: %w", err)
	}
	token := extractToken(raw)
	if token == "" {
		return Session{}, errors.New("auth file did not contain a usable token")
	}
	expires := extractExpires(raw)
	return Session{Token: token, Surface: defaultSurface(p.Surface), UserID: extractUserID(raw), ObtainedAt: nowFloat(), ExpiresAt: &expires}, nil
}

type OAuthProvider struct{}

func (OAuthProvider) Acquire(context.Context) (Session, error) {
	return Session{}, errors.New("OAuthProvider requires manual implementation; use GROK_SESSION_TOKEN or GROK_AUTH_FILE")
}

type NoopProvider struct{}

func (NoopProvider) Acquire(context.Context) (Session, error) { return Session{}, ErrNoAuth }

type Store struct {
	mu       sync.Mutex
	provider Provider
	session  *Session
}

func NewStore(provider Provider) *Store { return &Store{provider: provider} }

func (s *Store) Current(ctx context.Context) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil || s.session.Expired() {
		fresh, err := s.provider.Acquire(ctx)
		if err != nil {
			return Session{}, err
		}
		s.session = &fresh
	}
	return *s.session, nil
}

func (s *Store) ForceRefresh(ctx context.Context) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh, err := s.provider.Acquire(ctx)
	if err != nil {
		return Session{}, err
	}
	s.session = &fresh
	return fresh, nil
}

var tokenNames = []string{"key", "access_token", "AccessToken", "session_token", "SessionToken", "bearer", "Bearer", "id_token", "IDToken", "accessToken"}

func oidcEntry(raw map[string]any) map[string]any {
	var candidates []map[string]any
	for _, value := range raw {
		if node, ok := value.(map[string]any); ok {
			candidates = append(candidates, node)
		}
	}
	for _, node := range candidates {
		mode, _ := node["auth_mode"].(string)
		if mode = strings.ToLower(mode); mode == "oidc" || mode == "oauth" {
			return node
		}
	}
	for _, node := range candidates {
		if key, _ := node["key"].(string); strings.HasPrefix(key, "eyJ") {
			return node
		}
	}
	return nil
}

func firstString(node map[string]any, names []string) string {
	for _, name := range names {
		if value, ok := node[name].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func extractToken(raw map[string]any) string {
	if node := oidcEntry(raw); node != nil {
		if v := firstString(node, tokenNames); v != "" {
			return v
		}
	}
	if node, ok := raw["tokens"].(map[string]any); ok {
		if v := firstString(node, tokenNames); v != "" {
			return v
		}
	}
	return firstString(raw, tokenNames)
}

func extractUserID(raw map[string]any) string {
	if node := oidcEntry(raw); node != nil {
		if v := firstString(node, []string{"user_id", "userId", "UserId", "sub"}); v != "" {
			return v
		}
	}
	return firstString(raw, []string{"user_id", "userId", "UserId", "subject", "sub"})
}

func extractExpires(raw map[string]any) float64 {
	var values []any
	if node := oidcEntry(raw); node != nil {
		for _, name := range []string{"expires_at", "ExpiresAt", "exp", "expiry", "expiration"} {
			values = append(values, node[name])
		}
	}
	if node, ok := raw["tokens"].(map[string]any); ok {
		values = append(values, node["expires_at"], node["ExpiresAt"])
	}
	values = append(values, raw["expires_at"], raw["ExpiresAt"])
	for _, value := range values {
		switch v := value.(type) {
		case float64:
			return v
		case string:
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				return float64(t.UnixNano()) / 1e9
			}
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return float64(time.Now().Add(30 * 24 * time.Hour).Unix())
}

func defaultSurface(v string) string {
	if v == "" {
		return "cli"
	}
	return v
}
func nowFloat() float64 { return float64(time.Now().UnixNano()) / 1e9 }
