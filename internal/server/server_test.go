package server

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
	"github.com/Futureppo/grokcli2api-go/internal/openai"
)

func TestRootServiceInfo(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).root(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name":    "grokcli2api-go",
		"version": config.Version,
		"project": "https://github.com/Futureppo/grokcli2api-go",
	}
	if !reflect.DeepEqual(response, want) {
		t.Fatalf("response = %#v, want %#v", response, want)
	}
}

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
		if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("non-stream Accept-Encoding = %q, want gzip", got)
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

func TestChatProxySanitizesStreamingAndNonStreamingBodies(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		received = append(received, body)
		mu.Unlock()
		if streaming, _ := body["stream"].(bool); streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}}}})
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)

	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{
			"model":"grok-4",
			"messages":[{"role":"developer","content":"rules","cache_control":true},{"role":"user","content":"hi"}],
			"stream":%t,
			"max_completion_tokens":128,
			"parallel_tool_calls":true,
			"stream_options":{"include_usage":true},
			"unknown_extension":"drop-me"
		}`, stream)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
			t.Fatalf("stream=%t status=%d body=%s", stream, rec.Code, rec.Body.String())
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(received))
	}
	for index, body := range received {
		if body["max_tokens"] != float64(128) {
			t.Fatalf("request[%d] max_tokens = %#v", index, body["max_tokens"])
		}
		for _, key := range []string{"max_completion_tokens", "parallel_tool_calls", "unknown_extension"} {
			if _, exists := body[key]; exists {
				t.Fatalf("request[%d] leaked %s: %#v", index, key, body)
			}
		}
		streaming, _ := body["stream"].(bool)
		streamOptions, hasStreamOptions := body["stream_options"].(map[string]any)
		if streaming && (!hasStreamOptions || streamOptions["include_usage"] != true) {
			t.Fatalf("request[%d] missing streaming usage request: %#v", index, body)
		}
		if !streaming && hasStreamOptions {
			t.Fatalf("request[%d] leaked non-stream stream_options: %#v", index, body)
		}
		messages := body["messages"].([]any)
		first := messages[0].(map[string]any)
		if first["role"] != "system" || first["cache_control"] != nil {
			t.Fatalf("request[%d] messages = %#v", index, messages)
		}
	}
}

func TestChatFinishReasonAtEOFCommitsExplicitSessionContinuity(t *testing.T) {
	var sessions, turns []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessions = append(sessions, r.Header.Get("x-grok-session-id"))
		turns = append(turns, r.Header.Get("x-grok-turn-idx"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-upstream\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-upstream\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	body := `{"model":"grok-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	for request := 0; request < 2; request++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("X-Grok-Session-ID", "downstream-session")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "data: [DONE]") {
			t.Fatalf("request %d status=%d body=%s", request, rec.Code, rec.Body.String())
		}
	}
	if len(sessions) != 2 || sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("upstream sessions=%#v", sessions)
	}
	if len(turns) != 2 || turns[0] != "0" || turns[1] != "1" {
		t.Fatalf("upstream turns=%#v", turns)
	}
}

func TestChatProxySilentlyDropsMalformedToolHistoryBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{})
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)

	rec := httptest.NewRecorder()
	body := `{"model":"grok-4","messages":[{"role":"tool","tool_call_id":"missing","content":"result"}]}`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no representable input after cleaning") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("malformed request reached upstream %d times", calls.Load())
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

func TestExactChatDenialDeletesAccountAndRetries(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	deniedToken := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		tokens = append(tokens, token)
		if deniedToken == "" {
			deniedToken = token
		}
		denied := token == deniedToken
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if denied {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"status_code":403,"error":"Access to the chat endpoint is denied. Please update the permissions."}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-ok","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokens(t, upstream.URL, nil, []string{"token-a", "token-b"})

	request := func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	request()
	request()

	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 3 || tokens[0] == tokens[1] || tokens[2] != tokens[1] {
		t.Fatalf("disabled account was scheduled again: %v", tokens)
	}
}

func TestExactChatDenialDeletesAccountAndRetriesStream(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	deniedToken := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		tokens = append(tokens, token)
		if deniedToken == "" {
			deniedToken = token
		}
		denied := token == deniedToken
		mu.Unlock()
		if denied {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"Access to the chat endpoint is denied. Please update the permissions."}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokens(t, upstream.URL, nil, []string{"token-a", "token-b"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("expected stream retry on a different account, got %v", tokens)
	}
}

func TestExactChatDenialDeletesOnlyAccount(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"status_code":403,"error":"Access to the chat endpoint is denied. Please update the permissions."}`)
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokens(t, upstream.URL, nil, []string{"token-a"})
	request := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}]}`))
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if status := request(); status != http.StatusForbidden {
		t.Fatalf("first status=%d, want 403", status)
	}
	if status := request(); status != http.StatusServiceUnavailable {
		t.Fatalf("second status=%d, want 503", status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("deleted account called upstream %d times, want 1", got)
	}
}

func TestFreeModelQuotaRetriesAndOnlyCoolsAffectedModel(t *testing.T) {
	var mu sync.Mutex
	deniedToken := ""
	var grok4Tokens, buildTokens []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		model, _ := body["model"].(string)
		mu.Lock()
		if model == "grok-4" {
			grok4Tokens = append(grok4Tokens, token)
			if deniedToken == "" {
				deniedToken = token
			}
		} else if model == "grok-build" {
			buildTokens = append(buildTokens, token)
		}
		denied := token == deniedToken
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if model == "grok-4" && denied {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"status_code":429,"error":"You've used all the included free usage for model grok-4 for now. Usage resets over a rolling 24-hour window."}`)
			return
		}
		if model == "grok-build" && !denied {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"try another account"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-ok","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokens(t, upstream.URL, nil, []string{"token-a", "token-b"})

	request := func(model string) {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("model=%s status=%d body=%s", model, rec.Code, rec.Body.String())
		}
	}
	request("grok-4")
	request("grok-4")
	request("grok-build")

	mu.Lock()
	defer mu.Unlock()
	deniedGrok4Calls := 0
	for _, token := range grok4Tokens {
		if token == deniedToken {
			deniedGrok4Calls++
		}
	}
	if deniedGrok4Calls != 1 {
		t.Fatalf("quota-exhausted account received %d grok-4 calls, tokens=%v", deniedGrok4Calls, grok4Tokens)
	}
	if !slices.Contains(buildTokens, deniedToken) {
		t.Fatalf("quota-exhausted account was not available for another model: %v", buildTokens)
	}
}

func TestFreeModelQuotaRetriesStream(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	deniedToken := ""
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		tokens = append(tokens, token)
		if deniedToken == "" {
			deniedToken = token
		}
		denied := token == deniedToken
		mu.Unlock()
		if denied {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"You've used all the included free usage for model grok-4 for now."}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokens(t, upstream.URL, nil, []string{"token-a", "token-b"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("expected stream retry on a different account, got %v", tokens)
	}
}

func TestConcurrent401RefreshesCredentialOnce(t *testing.T) {
	var refreshCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			writeJSON(w, http.StatusOK, map[string]any{"access_token": "token-new", "refresh_token": "refresh-new", "expires_in": 3600})
		case "/v1/chat/completions":
			if r.Header.Get("Authorization") == "Bearer token-old" {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "expired token"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": "chatcmpl-ok", "choices": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	credential := map[string]any{
		"access_token": "token-old", "refresh_token": "refresh-old", "client_id": "client", "sub": "subject",
		"expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), "token_endpoint": upstream.URL + "/token",
		"models": []string{"grok-4"}, "models_updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "account.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 4, AccountMaxInflight: 16,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024, RetryMaxAttempts: 2,
		RetryBaseDelay: time.Millisecond, RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour,
		ClientName: "grok-shell", ClientVersion: "0.2.93", ClientSurface: "tui",
		ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	handler := app.Handler()
	start := make(chan struct{})
	errorsCh := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}]}`))
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				errorsCh <- fmt.Errorf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("OAuth refresh calls = %d, want 1", got)
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
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("stream Accept-Encoding = %q, want identity", got)
		}
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

func TestStreamingGzipCompatibilityFallback(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("stream Accept-Encoding = %q, want gzip", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		_ = gz.Close()
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokensAndCompression(t, upstream.URL, nil, []string{"upstream-token"}, "gzip")
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	h.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"content":"hi"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestStreamingHeadersAndTextFlushBeforeCompletion(t *testing.T) {
	tests := []struct {
		name, route, body, upstreamPath, textEvent, doneEvent, want string
	}{
		{
			name: "chat", route: "/v1/chat/completions", upstreamPath: "/v1/chat/completions", want: "hello",
			body:      `{"model":"grok-4","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			textEvent: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n",
			doneEvent: "data: [DONE]\n\n",
		},
		{
			name: "responses", route: "/v1/responses", upstreamPath: "/v1/chat/completions", want: "hello",
			body:      `{"model":"grok-4","input":"hi","stream":true}`,
			textEvent: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n",
			doneEvent: "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		},
		{
			name: "anthropic", route: "/v1/messages", upstreamPath: "/v1/chat/completions", want: "hello",
			body:      `{"model":"grok-4","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			textEvent: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n",
			doneEvent: "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headersSent := make(chan struct{})
			sendText := make(chan struct{})
			finish := make(chan struct{})
			var sendTextOnce, finishOnce sync.Once
			defer sendTextOnce.Do(func() { close(sendText) })
			defer finishOnce.Do(func() { close(finish) })
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.upstreamPath {
					t.Errorf("upstream path = %q, want %q", r.URL.Path, test.upstreamPath)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				close(headersSent)
				<-sendText
				_, _ = io.WriteString(w, test.textEvent)
				w.(http.Flusher).Flush()
				<-finish
				_, _ = io.WriteString(w, test.doneEvent)
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			downstream := httptest.NewServer(newTestHandler(t, upstream.URL, nil))
			defer downstream.Close()
			type responseResult struct {
				response *http.Response
				err      error
			}
			responses := make(chan responseResult, 1)
			go func() {
				req, err := http.NewRequest(http.MethodPost, downstream.URL+test.route, strings.NewReader(test.body))
				if err != nil {
					responses <- responseResult{err: err}
					return
				}
				req.Header.Set("Content-Type", "application/json")
				response, err := http.DefaultClient.Do(req)
				responses <- responseResult{response: response, err: err}
			}()
			select {
			case <-headersSent:
			case <-time.After(time.Second):
				t.Fatal("upstream headers were not sent")
			}
			var result responseResult
			select {
			case result = <-responses:
				if result.err != nil {
					t.Fatal(result.err)
				}
			case <-time.After(time.Second):
				t.Fatal("downstream headers were not flushed before the first event")
			}
			defer result.response.Body.Close()
			if result.response.StatusCode != http.StatusOK || !strings.Contains(result.response.Header.Get("Cache-Control"), "no-transform") {
				t.Fatalf("status=%d cache-control=%q", result.response.StatusCode, result.response.Header.Get("Cache-Control"))
			}
			sendTextOnce.Do(func() { close(sendText) })
			textSeen := make(chan error, 1)
			go func() {
				scanner := bufio.NewScanner(result.response.Body)
				for scanner.Scan() {
					if strings.Contains(scanner.Text(), test.want) {
						textSeen <- nil
						return
					}
				}
				textSeen <- scanner.Err()
			}()
			select {
			case err := <-textSeen:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("text was buffered until stream completion")
			}
			finishOnce.Do(func() { close(finish) })
		})
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
		if body["input"] != "hello" || body["model"] != "grok-4" || body["store"] != true || body["grok_extension"] != nil {
			t.Fatalf("wire=%#v", body)
		}
		writeJSON(w, 200, map[string]any{
			"id": "resp_1", "object": "response", "status": "completed", "model": "grok-4-build-free", "output": []any{},
			"billing":    map[string]any{"cost_in_usd_ticks": 9},
			"usage":      map[string]any{"input_tokens": 2, "output_tokens": 1, "total_tokens": 3, "cost_in_usd_ticks": 9},
			"grok_field": "hidden",
		})
	}))
	defer upstream.Close()
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
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
	if _, exists := response["grok_field"]; exists {
		t.Fatalf("Grok-native field leaked into default response: %#v", response)
	}
	if _, exists := response["billing"]; exists {
		t.Fatalf("billing leaked into default response: %#v", response)
	}
	if usage := response["usage"].(map[string]any); len(usage) != 3 || usage["cost_in_usd_ticks"] != nil {
		t.Fatalf("usage=%#v", usage)
	}
}

func TestResponsesForwardsPreviousResponseID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["previous_response_id"] != "resp_parent" || body["store"] != true {
			t.Fatalf("wire=%#v", body)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "resp_child", "previous_response_id": "resp_parent", "output": []any{}})
	}))
	defer upstream.Close()
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4","input":"continue","previous_response_id":"resp_parent"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResponsesStoreAwareToolContinuationWireBodies(t *testing.T) {
	tests := []struct {
		name          string
		responseID    string
		store         bool
		wantPrevious  bool
		wantInputSize int
	}{
		{name: "stored", responseID: "resp_wire_stored", store: true, wantPrevious: true, wantInputSize: 1},
		{name: "stateless", responseID: "resp_wire_stateless", store: false, wantInputSize: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			callID := "call_" + test.name
			openai.RememberCompletedResponseWithStore(openai.DefaultToolReplay, "grok-4", map[string]any{
				"id": test.responseID, "output": []any{map[string]any{
					"id": "fc_" + test.name, "type": "function_call", "call_id": callID, "name": "lookup", "arguments": `{}`,
				}},
			}, "", test.store)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				_, hasPrevious := body["previous_response_id"]
				if hasPrevious != test.wantPrevious {
					t.Fatalf("wire previous=%t body=%#v", hasPrevious, body)
				}
				input := body["input"].([]any)
				if len(input) != test.wantInputSize {
					t.Fatalf("wire input=%#v", input)
				}
				if !test.store && openai.String(input[0].(map[string]any), "type", "") != "function_call" {
					t.Fatalf("stateless call was not replayed: %#v", input)
				}
				writeJSON(w, http.StatusOK, map[string]any{"id": "resp_next_" + test.name, "output": []any{}})
			}))
			defer upstream.Close()
			h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
			rec := httptest.NewRecorder()
			requestBody := fmt.Sprintf(`{"model":"grok-4","previous_response_id":%q,"input":[{"type":"function_call_output","call_id":%q,"output":"ok"}]}`, test.responseID, callID)
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !test.store {
				var response map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if _, exists := response["previous_response_id"]; exists {
					t.Fatalf("stateless continuation synthesized previous id: %#v", response)
				}
			}
		})
	}
}

func TestTenantStoreFalseReplayMissStartsFreshConversation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["previous_response_id"] != nil {
			t.Fatalf("lost state handle reached upstream: %#v", body)
		}
		input, _ := body["input"].([]any)
		if len(input) != 1 || openai.String(input[0].(map[string]any), "type", "") != "message" {
			t.Fatalf("orphan output was not silently removed: %#v", input)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "resp_fresh", "status": "completed", "output": []any{}})
	}))
	defer upstream.Close()
	h := newTestHandlerWithTokensForBackend(t, upstream.URL, []string{"local-key"}, []string{"token"}, modelcatalog.BackendResponses)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"grok-4","store":false,"previous_response_id":"resp_lost",
		"input":[
			{"type":"function_call_output","call_id":"lost","output":"opaque"},
			{"type":"message","role":"user","content":"continue"}
		]
	}`))
	req.Header.Set("Authorization", "Bearer local-key")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResponsesConvertsNamespaceToolsAndRestoresCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tools=%#v", tools)
		}
		tool := tools[0].(map[string]any)
		if tool["type"] != "function" || tool["name"] != "mcp__github__fetch" {
			t.Fatalf("converted tool=%#v", tool)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "resp_1", "status": "completed",
			"output": []any{map[string]any{"type": "function_call", "name": "mcp__github__fetch", "call_id": "call_1", "arguments": `{}`}},
		})
	}))
	defer upstream.Close()
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
	body := `{"model":"grok-4","input":"fetch","tools":[{"type":"namespace","name":"mcp__github__","tools":[{"type":"function","name":"fetch","parameters":{"type":"object","properties":{}}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	call := response["output"].([]any)[0].(map[string]any)
	if call["name"] != "fetch" || call["namespace"] != "mcp__github__" {
		t.Fatalf("restored call=%#v", call)
	}
}

func TestResponsesRejectsUnknownInputBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Fatal("invalid request reached upstream")
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4","input":[{"type":"future_item"}]}`)))
	if rec.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	inner := payload["error"].(map[string]any)
	if inner["param"] != "input" || inner["type"] != "invalid_request_error" {
		t.Fatalf("error=%#v", inner)
	}
}

func TestResponsesStreamRestoresNamespace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"item_1\",\"type\":\"function_call\",\"name\":\"ns__lookup\",\"call_id\":\"call_1\",\"arguments\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"item_1\",\"type\":\"function_call\",\"name\":\"ns__lookup\",\"call_id\":\"call_1\",\"arguments\":\"{}\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"name\":\"ns__lookup\",\"call_id\":\"call_1\",\"arguments\":\"{}\"}]}}\n\n")
	}))
	defer upstream.Close()
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
	body := `{"model":"grok-4","input":"lookup","stream":true,"tools":[{"type":"namespace","name":"ns__","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}]}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	text := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(text, `"name":"ns__lookup"`) || !strings.Contains(text, `"name":"lookup"`) || !strings.Contains(text, `"namespace":"ns__"`) {
		t.Fatalf("status=%d body=%s", rec.Code, text)
	}
}

func TestResponsesGrokBuildClientUsesNativePassThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grok_extension"] != nil || body["model"] != "grok-build" || body["store"] != false {
			t.Fatalf("wire=%#v", body)
		}
		writeJSON(w, 200, map[string]any{"native": true, "grok_field": "kept"})
	}))
	defer upstream.Close()
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
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

func TestGrokCLIClientDetectionRequiresRecognizedIdentity(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{name: "ordinary OpenAI client", headers: map[string]string{"User-Agent": "OpenAI/Python 2.0"}},
		{name: "unrecognized Grok header value", headers: map[string]string{"x-grok-client-name": "third-party-dashboard"}},
		{name: "surface alone", headers: map[string]string{"x-grok-client-surface": "tui"}},
		{name: "token auth", headers: map[string]string{"X-XAI-Token-Auth": "xai-grok-cli"}, want: true},
		{name: "CLI version", headers: map[string]string{"x-grok-client-version": "0.2.99"}, want: true},
		{name: "CLI name", headers: map[string]string{"x-grok-client-name": "grok-shell"}, want: true},
		{name: "CLI identifier", headers: map[string]string{"x-grok-client-identifier": "grok-cli"}, want: true},
		{name: "current CLI user agent", headers: map[string]string{"User-Agent": "grok-cli/0.2.99 (windows; amd64)"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			for key, value := range test.headers {
				req.Header.Set(key, value)
			}
			if got := isGrokCLIClient(req); got != test.want {
				t.Fatalf("isGrokCLIClient = %t, want %t", got, test.want)
			}
		})
	}
}

func TestResponsesStreamPreservesEventsWithoutDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: grok.custom\ndata: {\"type\":\"grok.custom\",\"value\":1}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4","input":"hello","stream":true}`)))
	text := rec.Body.String()
	if !strings.Contains(text, "event: response.output_text.delta") || !strings.Contains(text, "event: response.completed") {
		t.Fatalf("events missing: %s", text)
	}
	if strings.Contains(text, "[DONE]") {
		t.Fatalf("OpenAI Responses stream must not append DONE: %s", text)
	}
	if strings.Contains(text, "grok.custom") {
		t.Fatalf("Grok-native event leaked into default stream: %s", text)
	}
}

func TestResponsesStreamEmitsErrorOnPrematureEOF(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cut\",\"model\":\"grok-4-build-free\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	}))
	defer upstream.Close()
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4","input":"hello","stream":true}`)))
	text := rec.Body.String()
	if !strings.Contains(text, "event: error") || !strings.Contains(text, `"type":"error"`) || !strings.Contains(text, "stream ended before a terminal event") {
		t.Fatalf("missing premature EOF error: %s", text)
	}
}

func TestResponsesNativeHTTPImageReachesUpstream(t *testing.T) {
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": "resp_image", "status": "completed", "output": []any{}})
	}))
	defer upstream.Close()
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"http://example.com/a.png"}]}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	encoded, _ := json.Marshal(received)
	if !strings.Contains(string(encoded), "http://example.com/a.png") {
		t.Fatalf("image was not represented on native Responses wire: %#v", received)
	}
}

func TestResponsesOversizedBodyUses413(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("oversized request reached upstream")
	}))
	defer upstream.Close()
	h := newTestHandler(t, upstream.URL, nil)
	body := `{"model":"grok-4","input":"` + strings.Repeat("x", (16<<20)+1) + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGrokBuildNativeResponsesRequiresTerminalEvent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: grok.custom\nid: native-1\nretry: 500\ndata: {\"type\":\"grok.custom\",\"value\":1}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-build","input":"hello","stream":true}`))
	req.Header.Set("User-Agent", "grok-shell/0.2.93")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	text := rec.Body.String()
	for _, expected := range []string{"event: grok.custom", "id: native-1", "retry: 500", "event: error", "upstream_stream_incomplete"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("%q missing from native stream: %s", expected, text)
		}
	}
	if strings.Contains(text, "data: [DONE]") {
		t.Fatalf("Responses [DONE] must not be treated as a terminal event: %s", text)
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
	h := newTestHandlerForBackend(t, upstream.URL, []string{"anthropic-key"}, modelcatalog.BackendResponses)
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

func TestAnthropicMessagesAlwaysSendsToolTypes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		stream := stream
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				tools, ok := body["tools"].([]any)
				if !ok || len(tools) != 1 {
					t.Errorf("tools=%#v", body["tools"])
				} else {
					tool, _ := tools[0].(map[string]any)
					if tool["type"] != "function" || tool["name"] != "Read" {
						t.Errorf("tool=%#v", tool)
					}
					parameters, _ := tool["parameters"].(map[string]any)
					if parameters["type"] != "object" {
						t.Errorf("parameters=%#v", parameters)
					}
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":2}}}\n\n")
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"id": "resp_1", "output": []any{}, "usage": map[string]any{"input_tokens": 2, "output_tokens": 0},
				})
			}))
			defer upstream.Close()

			requestBody := map[string]any{
				"model": "grok-4", "max_tokens": 128, "stream": stream,
				"messages": []any{map[string]any{"role": "user", "content": "read a file"}},
				"tools": []any{map[string]any{
					"name": "Read", "description": "Read a file",
					"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
				}},
			}
			encoded, err := json.Marshal(requestBody)
			if err != nil {
				t.Fatal(err)
			}
			h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(encoded))))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
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
	h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
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

func TestAnthropicMessagesAppliesResponseOptions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		stream := stream
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
					_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"reasoning_1\",\"type\":\"reasoning\"}}\n\n")
					_, _ = io.WriteString(w, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"reasoning_1\",\"delta\":\"hidden\"}\n\n")
					_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"reasoning_1\",\"type\":\"reasoning\"}}\n\n")
					_, _ = io.WriteString(w, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"item_id\":\"msg_1\",\"content_index\":0,\"part\":{\"type\":\"output_text\"}}\n\n")
					_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"ABCST\"}\n\n")
					_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"content_index\":0,\"delta\":\"OPXYZ\"}\n\n")
					_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"id": "resp_1", "output": []any{
						map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "hidden"}}},
						map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "ABCSTOPXYZ"}}},
					},
				})
			}))
			defer upstream.Close()
			h := newTestHandlerForBackend(t, upstream.URL, nil, modelcatalog.BackendResponses)
			body := fmt.Sprintf(`{"model":"grok-4","max_tokens":128,"stream":%t,"stop_sequences":["STOP"],"messages":[{"role":"user","content":"hi"}]}`, stream)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
			text := rec.Body.String()
			if rec.Code != http.StatusOK || strings.Contains(text, "hidden") || strings.Contains(text, "XYZ") || !strings.Contains(text, "ABC") || !strings.Contains(text, "stop_sequence") || !strings.Contains(text, "STOP") {
				t.Fatalf("status=%d body=%s", rec.Code, text)
			}
			if !strings.Contains(text, "msg_resp_1") {
				t.Fatalf("message id was not normalized: %s", text)
			}
		})
	}
}

func TestAnthropicMessagesRejectsOrphanToolResult(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"grok-4","max_tokens":64,
		"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_missing","content":"x"}]}]
	}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"type":"error"`) || !strings.Contains(rec.Body.String(), "no representable input after cleaning") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
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
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/auth/api-key", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAdminCredentialLifecycle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream path = %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "grok-4"}}})
	}))
	defer upstream.Close()
	s := newAdminTestServer(t, upstream.URL)
	defer s.Close()
	h := s.Handler()

	payload := adminCredentialPayload(t, "remote-subject", "token-secret")
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/credentials", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Key", "admin-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	var created struct {
		Credential auth.CredentialInfo `json:"credential"`
		Created    bool                `json:"created"`
		Discovery  string              `json:"model_discovery"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.Discovery != "succeeded" || created.Credential.Status != "ready" || created.Credential.ID == "" {
		t.Fatalf("create response = %#v", created)
	}

	unauthorized := httptest.NewRequest(http.MethodGet, "/v1/admin/credentials", nil)
	unauthorized.Header.Set("Authorization", "Bearer client-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, unauthorized)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("ordinary key status = %d", rec.Code)
	}
	adminOnClientAPI := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	adminOnClientAPI.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, adminOnClientAPI)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("administrator key on client API status = %d", rec.Code)
	}

	list := httptest.NewRequest(http.MethodGet, "/v1/admin/credentials", nil)
	list.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, list)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{"remote-subject", "token-secret", "refresh-secret", "client-secret"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("list leaked %q: %s", secret, rec.Body.String())
		}
	}

	var multipartBody bytes.Buffer
	writer := multipart.NewWriter(&multipartBody)
	part, err := writer.CreateFormFile("file", "../../ignored.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(adminCredentialPayload(t, "remote-subject", "token-replaced")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/admin/credentials", &multipartBody)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"created":false`) {
		t.Fatalf("replace status=%d body=%s", rec.Code, rec.Body.String())
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/v1/admin/credentials/"+created.Credential.ID, nil)
	deleteReq.Header.Set("X-Admin-Key", "admin-secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, deleteReq)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", rec.Code, rec.Body.String())
	}

	inference := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4","input":"hello"}`))
	inference.Header.Set("Authorization", "Bearer client-key")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, inference)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty-pool inference status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminCredentialUploadValidation(t *testing.T) {
	s := newAdminTestServer(t, "http://127.0.0.1:1")
	defer s.Close()
	h := s.Handler()
	request := func(contentType string, body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/credentials", body)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-Admin-Key", "admin-secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := request("text/plain", strings.NewReader("{}")); rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("media type status = %d", rec.Code)
	}
	if rec := request("application/json", strings.NewReader(`{"access_token":`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request("application/json", strings.NewReader(`{"access_token":"token"}`)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid credential status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := request("application/json", bytes.NewReader(make([]byte, maxCredentialSize+1))); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminUploadKeepsCredentialWhenDiscoveryFails(t *testing.T) {
	s := newAdminTestServer(t, "http://127.0.0.1:1")
	defer s.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/credentials", bytes.NewReader(adminCredentialPayload(t, "offline-subject", "offline-token")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Key", "admin-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"model_discovery":"failed"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	credentials := s.pool.Credentials()
	if len(credentials) != 1 || credentials[0].Status != "pending_models" {
		t.Fatalf("persisted credentials = %#v", credentials)
	}
}

func TestModelRoutesRequireAPIKey(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", []string{"key"})
	for _, path := range []string{"/v1/models", "/v1/models/grok-4"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without key: status = %d", path, rec.Code)
		}

		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer key")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with key: status = %d, body=%s", path, rec.Code, rec.Body.String())
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
			ID      string `json:"id"`
			Created int64  `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(response.Data))
	for _, model := range response.Data {
		ids = append(ids, model.ID)
		if want := s.pool.ModelFirstSeen()[model.ID]; want <= 0 || model.Created != want {
			t.Fatalf("model %q created = %d, want persisted first-seen %d", model.ID, model.Created, want)
		}
	}
	if got, want := strings.Join(ids, ","), "grok-alpha,grok-beta,grok-shared"; got != want {
		t.Fatalf("models = %q, want %q", got, want)
	}

	single := httptest.NewRecorder()
	s.Handler().ServeHTTP(single, httptest.NewRequest(http.MethodGet, "/v1/models/grok-shared", nil))
	if single.Code != http.StatusOK {
		t.Fatalf("single model status = %d, body=%s", single.Code, single.Body.String())
	}
	var selected struct {
		Created int64 `json:"created"`
	}
	if err := json.Unmarshal(single.Body.Bytes(), &selected); err != nil {
		t.Fatal(err)
	}
	if want := s.pool.ModelFirstSeen()["grok-shared"]; selected.Created != want {
		t.Fatalf("single model created = %d, want list first-seen %d", selected.Created, want)
	}
}

func TestModelsEndpointSerializesConservativeXGrokMetadata(t *testing.T) {
	dir := t.TempDir()
	writeCredentialFileModels(t, dir, "subject-a", "token-a", []string{"grok-shared"})
	writeCredentialFileModels(t, dir, "subject-b", "token-b", []string{"grok-shared"})
	cfg := config.Config{
		ChatProxyBaseURL: "http://127.0.0.1:1", ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, ModelsRefreshInterval: 6 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024, RetryMaxAttempts: 3,
		RetryBaseDelay: time.Millisecond, RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour,
		ClientName: "grok-shell", ClientVersion: "0.2.102", ClientSurface: "headless", ClientMode: "headless",
		ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	accountIDs := s.pool.AccountIDs()
	if len(accountIDs) != 2 {
		t.Fatalf("account count = %d, want 2", len(accountIDs))
	}
	provisionalCreated := s.pool.ModelFirstSeen()["grok-shared"]
	if provisionalCreated <= 0 {
		t.Fatal("provisional model has no first-seen timestamp")
	}
	firstSeen := time.Now().Add(-2 * time.Minute).UTC().Truncate(time.Second)
	descriptors := [][]modelcatalog.ModelDescriptor{
		{{
			ID: "grok-shared", WireModel: "wire-responses", Backend: modelcatalog.BackendResponses,
			ContextWindow: 500000, MaxCompletionTokens: 32768, SupportsReasoningEffort: true,
			ReasoningEfforts: []string{"low", "medium", "high"}, SupportsBackendSearch: true, StreamToolCalls: true,
		}},
		{{
			ID: "grok-shared", WireModel: "wire-messages", Backend: modelcatalog.BackendMessages,
			ContextWindow: 300000, MaxCompletionTokens: 16384, SupportsReasoningEffort: true,
			ReasoningEfforts: []string{"low", "high", "xhigh"}, StreamToolCalls: true,
		}},
	}
	for index, accountID := range accountIDs {
		if err := s.pool.UpdateModelDescriptors(accountID, descriptors[index], fmt.Sprintf(`"etag-%d"`, index), firstSeen.Add(time.Duration(index)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
			XGrok   struct {
				APIBackends             []string `json:"api_backends"`
				ContextWindow           uint64   `json:"context_window"`
				MaxCompletionTokens     uint32   `json:"max_completion_tokens"`
				ReasoningEfforts        []string `json:"reasoning_efforts"`
				SupportsReasoningEffort bool     `json:"supports_reasoning_effort"`
				SupportsBackendSearch   bool     `json:"supports_backend_search"`
				StreamToolCalls         bool     `json:"stream_tool_calls"`
			} `json:"x_grok"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Object != "list" || len(response.Data) != 1 {
		t.Fatalf("response = %#v", response)
	}
	model := response.Data[0]
	if model.ID != "grok-shared" || model.Object != "model" || model.OwnedBy != "xai" || model.Created != provisionalCreated {
		t.Fatalf("model identity = %#v", model)
	}
	if !reflect.DeepEqual(model.XGrok.APIBackends, []string{"messages", "responses"}) ||
		model.XGrok.ContextWindow != 300000 || model.XGrok.MaxCompletionTokens != 16384 ||
		!reflect.DeepEqual(model.XGrok.ReasoningEfforts, []string{"high", "low"}) ||
		!model.XGrok.SupportsReasoningEffort || model.XGrok.SupportsBackendSearch || !model.XGrok.StreamToolCalls {
		t.Fatalf("x_grok = %#v", model.XGrok)
	}
}

func TestRemovedRoutesAre404(t *testing.T) {
	h := newTestHandler(t, "http://127.0.0.1:1", nil)
	for _, path := range []string{"/docs", "/openapi.json", "/v1/health", "/v1/auth/status", "/v1/admin/credentials"} {
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
	if got := requestAffinity(req, body); got.Key != "previous:resp" || got.Mode != auth.AffinityHard {
		t.Fatalf("previous response affinity = %q", got)
	}
	withoutPrevious := map[string]any{
		"prompt_cache_key": "cache", "user": "user",
		"metadata": map[string]any{"user_id": "metadata-user"},
	}
	if got := requestAffinity(req, withoutPrevious); got.Key != "session:secret-session" || got.Mode != auth.AffinityHard {
		t.Fatalf("header affinity = %q", got)
	}
	conv := conversationID(requestAffinity(req, withoutPrevious))
	if conv == "" || strings.Contains(conv, "secret-session") || conv != conversationID(auth.Affinity{Key: "session:secret-session", Mode: auth.AffinityHard}) {
		t.Fatalf("conversation id is not stable and opaque: %q", conv)
	}
	req.Header.Del("X-Grok-Session-ID")
	if got := requestAffinity(req, body); got.Key != "previous:resp" || got.Mode != auth.AffinityHard {
		t.Fatalf("previous response affinity = %q", got)
	}
	delete(body, "previous_response_id")
	if got := requestAffinity(req, body); got.Key != "cache:cache" || got.Mode != auth.AffinitySoft {
		t.Fatalf("prompt cache affinity = %q", got)
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

func newTestHandlerForBackend(t *testing.T, upstream string, keys []string, backend modelcatalog.Backend) http.Handler {
	return newTestHandlerWithTokensForBackend(t, upstream, keys, []string{"upstream-token"}, backend)
}

func newAdminTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	cfg := config.Config{
		ChatProxyBaseURL: upstream, ChatProxyVersion: "v1", AuthsDir: filepath.Join(t.TempDir(), "auths"),
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, AccountMaxInflight: 2,
		ModelsRefreshInterval: 6 * time.Hour, RetryMaxAttempts: 1, RetryBaseDelay: time.Millisecond,
		RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 128,
		ClientName: "grok-shell", ClientVersion: "0.2.93", ClientSurface: "tui",
		ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", StreamCompression: "identity",
		APIKeys: []string{"client-key"}, AdminKey: "admin-secret",
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func adminCredentialPayload(t *testing.T, subject, token string) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"access_token": token, "refresh_token": "refresh-secret", "client_id": "client-secret",
		"sub": subject, "expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func newTestHandlerWithTokens(t *testing.T, upstream string, keys, tokens []string) http.Handler {
	return newTestHandlerWithTokensAndCompression(t, upstream, keys, tokens, "")
}

func newTestHandlerWithTokensForBackend(t *testing.T, upstream string, keys, tokens []string, backend modelcatalog.Backend) http.Handler {
	return newTestHandlerWithTokensForBackendAndCompression(t, upstream, keys, tokens, backend, "")
}

func newTestHandlerWithTokensAndCompression(t *testing.T, upstream string, keys, tokens []string, compression string) http.Handler {
	return newTestHandlerWithTokensForBackendAndCompression(t, upstream, keys, tokens, "", compression)
}

func newTestHandlerWithTokensForBackendAndCompression(t *testing.T, upstream string, keys, tokens []string, backend modelcatalog.Backend, compression string) http.Handler {
	t.Helper()
	dir := t.TempDir()
	for i, token := range tokens {
		writeCredentialFile(t, dir, fmt.Sprintf("test-%d", i), token)
	}
	cfg := config.Config{ChatProxyBaseURL: upstream, ChatProxyVersion: "v1", AuthsDir: dir, AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, AffinityTTL: time.Hour, AffinityMaxEntries: 1024, RetryMaxAttempts: 3, RetryBaseDelay: time.Millisecond, RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour, ClientName: "grok-shell", ClientVersion: "0.2.93", ClientSurface: "tui", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli", StreamCompression: compression, APIKeys: keys}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if backend != "" {
		for _, accountID := range s.pool.AccountIDs() {
			if err := s.pool.UpdateModelDescriptors(accountID, []modelcatalog.ModelDescriptor{
				{ID: "grok-4", WireModel: "grok-4", Backend: backend, SupportedInAPI: true},
				{ID: "grok-build", WireModel: "grok-build", Backend: backend, SupportedInAPI: true},
			}, "", time.Now()); err != nil {
				s.Close()
				t.Fatal(err)
			}
		}
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
