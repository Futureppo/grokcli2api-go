package grok

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
)

func TestBypassProxy(t *testing.T) {
	tests := []struct {
		host     string
		patterns []string
		want     bool
	}{
		{"localhost", []string{"localhost"}, true},
		{"api.internal", []string{"*.internal"}, true},
		{"deep.api.example.com", []string{"example.com"}, true},
		{"10.2.3.4", []string{"10.0.0.0/8"}, true},
		{"cli-chat-proxy.grok.com", []string{"localhost", "*.internal"}, false},
	}
	for _, tt := range tests {
		if got := bypassProxy(tt.host, tt.patterns); got != tt.want {
			t.Errorf("bypassProxy(%q, %v) = %v, want %v", tt.host, tt.patterns, got, tt.want)
		}
	}
}

func TestHTTPClientKeepsWarmConnections(t *testing.T) {
	client, err := NewHTTPClient(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	if transport.IdleConnTimeout != 5*time.Minute || transport.MaxIdleConnsPerHost != 32 {
		t.Fatalf("idle timeout=%s per-host=%d", transport.IdleConnTimeout, transport.MaxIdleConnsPerHost)
	}
}

func TestPermanentAccountDenialDetection(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "top-level error", status: http.StatusForbidden, body: `{"error":"Access to the chat endpoint is denied. Please update permissions."}`, want: true},
		{name: "nested error", status: http.StatusForbidden, body: `{"error":{"code":"permission_denied","message":"ACCESS TO THE CHAT ENDPOINT IS DENIED"}}`, want: true},
		{name: "raw text", status: http.StatusForbidden, body: `Access to the chat endpoint is denied.`, want: true},
		{name: "other forbidden", status: http.StatusForbidden, body: `{"error":"model access denied"}`},
		{name: "quota forbidden", status: http.StatusForbidden, body: `{"code":"personal-team-blocked:spending-limit","error":"quota exhausted"}`},
		{name: "unauthorized with matching text", status: http.StatusUnauthorized, body: `{"error":"Access to the chat endpoint is denied"}`},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":"Access to the chat endpoint is denied"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.status, Header: http.Header{}}
			apiErr := parseAPIError(response, []byte(test.body))
			if got := isPermanentAccountDenial(apiErr); got != test.want {
				t.Fatalf("isPermanentAccountDenial() = %v, want %v; error=%#v", got, test.want, apiErr)
			}
		})
	}
}

func TestRefreshModelsDiscoversEveryAccountAndPersistsCatalogs(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		calls.Add(1)
		models := []map[string]any{{"id": "grok-shared"}}
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			models = append(models, map[string]any{"id": "grok-alpha"})
		case "Bearer token-b":
			models = append(models, map[string]any{"id": "grok-beta"})
		default:
			t.Errorf("unexpected authorization header")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
	}))
	defer upstream.Close()

	dir := t.TempDir()
	writeModelTestCredential(t, dir, "account-a.json", "subject-a", "token-a")
	writeModelTestCredential(t, dir, "account-b.json", "subject-b", "token-b")
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 2, ModelsRefreshInterval: 6 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024, ClientName: "grok-shell",
		ClientVersion: "0.2.93", ClientSurface: "tui", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: cfg.ClientSurface, ReloadInterval: time.Hour, RefreshConcurrency: 2,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.RefreshModels(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model discovery calls = %d, want 2", got)
	}
	if got, want := strings.Join(pool.Models(), ","), "grok-alpha,grok-beta,grok-shared"; got != want {
		t.Fatalf("aggregated models = %q, want %q", got, want)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(payload, &raw); err != nil {
			t.Fatal(err)
		}
		if len(raw["models"].([]any)) != 2 || raw["models_updated_at"] == "" {
			t.Fatalf("credential model catalog was not persisted")
		}
	}
}

func TestRefreshModelsDisablesPermanentlyDeniedAccount(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"Access to the chat endpoint is denied"}`)
		case "Bearer token-b":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "grok-4"}}})
		default:
			t.Errorf("unexpected authorization header")
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	writeModelTestCredential(t, dir, "account-a.json", "subject-a", "token-a")
	writeModelTestCredential(t, dir, "account-b.json", "subject-b", "token-b")
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 2, ModelsRefreshInterval: 6 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024,
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 2,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.RefreshModels(context.Background(), true); err == nil {
		t.Fatal("partial model refresh unexpectedly succeeded")
	}

	available := 0
	for _, id := range pool.AccountIDs() {
		lease, err := pool.AcquireAccount(context.Background(), id)
		if err != nil {
			continue
		}
		available++
		if lease.Session().Token != "token-b" {
			t.Fatalf("denied credential remained available: %q", lease.Session().Token)
		}
		lease.Release()
	}
	if available != 1 {
		t.Fatalf("available accounts=%d, want 1", available)
	}
}

func writeModelTestCredential(t *testing.T, dir, name, subject, token string) {
	t.Helper()
	raw := map[string]any{
		"access_token": token, "refresh_token": "refresh", "client_id": "client", "sub": subject,
		"expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEventStreamPreservesSSEFieldsAndMultilineData(t *testing.T) {
	body := "event: response.output_text.delta\n" +
		"id: evt-1\nretry: 1000\n" +
		"data: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"delta\":\"hello\"}\n\n"
	reader := strings.NewReader(body)
	stream := &EventStream{
		response: &http.Response{Body: io.NopCloser(reader)},
		scanner:  bufio.NewScanner(strings.NewReader(body)),
	}
	event, ok, err := stream.Next()
	if err != nil || !ok {
		t.Fatalf("Next() = %#v, %v, %v", event, ok, err)
	}
	if event.Event != "response.output_text.delta" || event.ID != "evt-1" || event.Retry != "1000" {
		t.Fatalf("event fields lost: %#v", event)
	}
	if string(event.Data) != "{\"type\":\"response.output_text.delta\",\n\"delta\":\"hello\"}" {
		t.Fatalf("data = %q", event.Data)
	}
}
