package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/config"
)

func TestAPIKeyGateAndChatProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer upstream-token" {
			t.Errorf("upstream auth = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-grok-client-name") == "" {
			t.Error("missing grok identity header")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		writeJSON(w, 200, map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hello"}}}})
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream.URL, []string{"local-key"})
	body := `{"model":"grok-4","messages":[{"role":"user","content":"hi"}]}`
	rejected := httptest.NewRecorder()
	h.ServeHTTP(rejected, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("without key status = %d", rejected.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer local-key")
	accepted := httptest.NewRecorder()
	h.ServeHTTP(accepted, req)
	if accepted.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(accepted.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["model"] != "grok-4" || response["object"] != "chat.completion" {
		t.Fatalf("response=%#v", response)
	}
}

func TestQuotaErrorSwitchesAccount(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		tokens = append(tokens, token)
		call := len(tokens)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":"personal-team-blocked:spending-limit","error":"quota exhausted"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-ok","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokens(t, upstream.URL, nil, []string{"token-a", "token-b"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"user":"session-a"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("expected two different accounts, got %v", tokens)
	}
}

func TestServiceUnavailableRetriesDifferentAccount(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		call := len(tokens)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporarily unavailable"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-ok","choices":[]}`)
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokens(t, upstream.URL, nil, []string{"token-a", "token-b"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("expected retry on a different account, got %v", tokens)
	}
}

func TestSessionAffinityDoesNotUseLocalAPIKey(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-ok","choices":[]}`)
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokens(t, upstream.URL, []string{"shared-key"}, []string{"token-a", "token-b"})
	request := func(session string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer shared-key")
		req.Header.Set("X-Grok-Session-ID", session)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("session %s status=%d body=%s", session, rec.Code, rec.Body.String())
		}
	}
	request("one")
	request("one")
	request("two")
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 3 || tokens[0] != tokens[1] || tokens[2] == tokens[0] {
		t.Fatalf("unexpected affinity assignments: %v", tokens)
	}
}

func TestStreamingSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	text := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(text, `"role":"assistant"`) || !strings.Contains(text, `"content":"hi"`) || !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("invalid SSE response (%d): %s", rec.Code, text)
	}
}

func TestStreamingFailureAfterHeadersIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer does not support hijacking")
			return
		}
		conn, writer, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = writer.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 512\r\n\r\ndata: {\"choices\":[]}\n\n")
		_ = writer.Flush()
		_ = conn.Close()
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokens(t, upstream.URL, nil, []string{"token-a", "token-b"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status=%d upstream_calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
}

func TestResponsesDefaultsToOpenAIFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["input"] != "hello" || body["model"] != "grok-4" || body["store"] != false {
			t.Fatalf("wire=%#v", body)
		}
		writeJSON(w, 200, map[string]any{"id": "resp_1", "object": "response", "status": "completed", "output": []any{}})
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4","input":"hello"}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	if response["object"] != "response" || response["model"] != "grok-4" {
		t.Fatalf("response=%#v", response)
	}
	if _, exists := response["choices"]; exists {
		t.Fatal("response was incorrectly normalized as chat completion")
	}
}

func TestResponsesGrokBuildClientUsesNativePassThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grok_extension"] != "native" || body["model"] != "grok-build" {
			t.Fatalf("wire=%#v", body)
		}
		writeJSON(w, 200, map[string]any{"native": true, "grok_field": "kept"})
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-build","input":"hello","grok_extension":"native"}`))
	req.Header.Set("x-grok-client-name", "grok-shell")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var response map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	if response["native"] != true || response["grok_field"] != "kept" {
		t.Fatalf("response=%#v", response)
	}
}

func TestResponsesStreamPreservesEventsWithoutDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4","input":"hello","stream":true}`)))
	text := rec.Body.String()
	if !strings.Contains(text, "event: response.output_text.delta") || !strings.Contains(text, "event: response.completed") {
		t.Fatalf("events missing: %s", text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("OpenAI Responses stream must not append DONE: %s", text)
	}
}

func TestGrokBuildNativeStreamPreservesRawSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: grok.custom\nid: native-1\nretry: 500\ndata: {\"type\":\"grok.custom\",\"value\":1}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-build","input":"hello","stream":true}`))
	req.Header.Set("User-Agent", "grok-shell/0.2.93")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	text := rec.Body.String()
	for _, expected := range []string{"event: grok.custom", "id: native-1", "retry: 500", "data: [DONE]"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("%q missing from native stream: %s", expected, text)
		}
	}
}

func TestAnthropicMessagesAndXAPIKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["max_output_tokens"] != float64(128) || body["input"] == nil {
			t.Fatalf("wire=%#v", body)
		}
		writeJSON(w, 200, map[string]any{
			"id": "resp_1", "output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hello"}}}},
			"usage": map[string]any{"input_tokens": 2, "output_tokens": 1},
		})
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, []string{"anthropic-key"})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "anthropic-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	if response["type"] != "message" || response["role"] != "assistant" || response["stop_reason"] != "end_turn" {
		t.Fatalf("response=%#v", response)
	}
}

func TestAnthropicMessagesStreamingSequence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_1\",\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"hello\"}\n\n")
		_, _ = io.WriteString(w, "event: response.content_part.done\ndata: {\"type\":\"response.content_part.done\",\"item_id\":\"msg_1\",\"content_index\":0}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	text := rec.Body.String()
	ordered := []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: content_block_stop", "event: message_delta", "event: message_stop"}
	position := 0
	for _, expected := range ordered {
		index := strings.Index(text[position:], expected)
		if index < 0 {
			t.Fatalf("%q missing or out of order: %s", expected, text)
		}
		position += index + len(expected)
	}
}

func TestAnthropicAuthErrorEnvelope(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", []string{"key"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"type":"error"`) || !strings.Contains(rec.Body.String(), `"authentication_error"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestStreamingUpstreamErrorKeepsHTTPStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"code":"rate_limit","error":"slow down"}`)
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4","input":"hello","stream":true}`)))
	if rec.Code != http.StatusTooManyRequests || strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d type=%s body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestPublicRoutesBypassGate(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", []string{"key"})
	for _, path := range []string{"/v1/models", "/v1/auth/api-key"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 {
			t.Errorf("%s status = %d", path, rec.Code)
		}
	}
}

func TestModelsEndpointAggregatesCredentialCatalogs(t *testing.T) {
	dir := t.TempDir()
	writeCredentialFileModels(t, dir, "subject-a", "token-a", []string{"grok-alpha", "grok-shared"})
	writeCredentialFileModels(t, dir, "subject-b", "token-b", []string{"grok-beta", "grok-shared"})
	cfg := config.Config{
		ChatProxyBaseURL: "http://127.0.0.1:1", ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, ModelsRefreshInterval: 6 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024, RetryMaxAttempts: 3,
		RetryBaseDelay: time.Millisecond, RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour,
		ClientName: "grok-shell", ClientVersion: "0.2.93", ClientSurface: "tui",
		ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		ids = append(ids, model.ID)
	}
	if got, want := strings.Join(ids, ","), "grok-alpha,grok-beta,grok-shared"; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}
}

func TestRemovedRoutesAre404(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", nil)
	for _, path := range []string{"/docs", "/openapi.json", "/v1/health", "/v1/auth/status"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("/v1/auth/refresh status = %d", rec.Code)
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAffinityInputPrecedenceAndOpaqueConversationID(t *testing.T) {
	body := map[string]any{
		"prompt_cache_key": "cache", "previous_response_id": "resp", "user": "user",
		"metadata": map[string]any{"user_id": "metadata-user"},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Grok-Session-ID", "secret-session")
	if got := requestAffinity(req, body); got != "session:secret-session" {
		t.Fatalf("header affinity = %q", got)
	}
	conv := conversationID(requestAffinity(req, body))
	if conv == "" || strings.Contains(conv, "secret-session") || conv != conversationID("session:secret-session") {
		t.Fatalf("conversation id is not stable and opaque: %q", conv)
	}
	req.Header.Del("X-Grok-Session-ID")
	if got := requestAffinity(req, body); got != "cache:cache" {
		t.Fatalf("prompt cache affinity = %q", got)
	}
	delete(body, "prompt_cache_key")
	if got := requestAffinity(req, body); got != "previous:resp" {
		t.Fatalf("previous response affinity = %q", got)
	}
}

func BenchmarkModelsEndpoint(b *testing.B) {
	h := newBenchmarkHandler(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

func newTestHandler(t *testing.T, upstream string, keys []string) http.Handler {
	return newTestHandlerWithTokens(t, upstream, keys, []string{"upstream-token"})
}

func newTestHandlerWithTokens(t *testing.T, upstream string, keys, tokens []string) http.Handler {
	t.Helper()
	dir := t.TempDir()
	for i, token := range tokens {
		writeCredentialFile(t, dir, fmt.Sprintf("test-%d", i), token)
	}
	cfg := config.Config{ChatProxyBaseURL: upstream, ChatProxyVersion: "v1", AuthsDir: dir, AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, AffinityTTL: time.Hour, AffinityMaxEntries: 1024, RetryMaxAttempts: 3, RetryBaseDelay: time.Millisecond, RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour, ClientName: "grok-shell", ClientVersion: "0.2.93", ClientSurface: "tui", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", APIKeys: keys}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s.Handler()
}

func newBenchmarkHandler(b *testing.B) http.Handler {
	b.Helper()
	dir := b.TempDir()
	writeBenchmarkCredential(b, dir, "token")
	cfg := config.Config{ChatProxyBaseURL: "http://127.0.0.1:1", ChatProxyVersion: "v1", AuthsDir: dir, AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, AffinityTTL: time.Hour, AffinityMaxEntries: 1024, RetryMaxAttempts: 3, RetryBaseDelay: time.Millisecond, RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour, ClientName: "grok-shell", ClientVersion: "0.2.93", ClientSurface: "tui", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli"}
	s, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(s.Close)
	return s.Handler()
}

func writeBenchmarkCredential(b *testing.B, dir, token string) {
	b.Helper()
	writeCredentialFile(b, dir, "test", token)
}

type fatalHelper interface {
	Helper()
	Fatal(...any)
}

func writeCredentialFile(tb fatalHelper, dir, subject, token string) {
	tb.Helper()
	writeCredentialFileModels(tb, dir, subject, token, []string{"grok-4", "grok-build"})
}

func writeCredentialFileModels(tb fatalHelper, dir, subject, token string, models []string) {
	tb.Helper()
	raw := map[string]any{"access_token": token, "refresh_token": "refresh", "client_id": "client", "sub": subject, "expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "models": models, "models_updated_at": time.Now().UTC().Format(time.RFC3339Nano)}
	b, err := json.Marshal(raw)
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, subject+".json"), b, 0o600); err != nil {
		tb.Fatal(err)
	}
}
