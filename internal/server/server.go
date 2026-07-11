package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/anthropic"
	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/openai"
)

type Server struct {
	cfg    config.Config
	pool   *auth.Pool
	client *grok.Client
	mux    *http.ServeMux
}

func New(cfg config.Config) (*Server, error) {
	httpClient, err := grok.NewHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: cfg.AuthsDir, Surface: cfg.ClientSurface,
		ReloadInterval: cfg.AuthsReloadInterval, RefreshConcurrency: cfg.AuthRefreshConcurrency,
		AccountMaxInflight: cfg.AccountMaxInflight,
		AffinityTTL:        cfg.AffinityTTL, AffinityMaxEntries: cfg.AffinityMaxEntries,
	}, httpClient)
	if err != nil {
		return nil, err
	}
	client, err := grok.NewClient(cfg, pool, httpClient)
	if err != nil {
		pool.Close()
		return nil, err
	}
	modelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err = client.InitializeModels(modelCtx)
	cancel()
	if err != nil {
		client.Close()
		pool.Close()
		return nil, fmt.Errorf("initialize model catalog: %w", err)
	}
	s := &Server{cfg: cfg, pool: pool, client: client, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

func (s *Server) Close() { s.client.Close(); s.pool.Close() }

func (s *Server) Handler() http.Handler {
	return recoverer(requestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mux.ServeHTTP(w, r)
	})))
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.root)
	s.protected("GET /v1/models", s.models)
	s.protected("GET /v1/models/{model_id}", s.model)
	s.mux.HandleFunc("GET /v1/auth/api-key", s.apiKeyStatus)

	s.protected("POST /v1/chat/completions", s.chat)
	s.protected("POST /v1/responses", s.responses)
	s.protected("POST /v1/messages", s.messages)
	s.protected("GET /v1/grok/settings", s.proxyGET("settings", false))
	s.protected("GET /v1/grok/user", s.proxyGET("user?include=subscription", false))
	s.protected("GET /v1/grok/billing", s.proxyGET("billing?format=credits", false))
	s.protected("GET /v1/grok/mcp/configs", s.proxyGET("mcp/configs", false))
	s.protected("GET /v1/grok/mcp/tools/list", s.proxyGET("mcp/tools/list", false))
	s.protected("GET /v1/grok/feedback/config", s.proxyGET("feedback/config", true))
}

func (s *Server) protected(pattern string, handler http.HandlerFunc) {
	s.mux.Handle(pattern, s.apiKeyGate(handler))
}

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	// A ServeMux pattern ending in "/" is a subtree match; keep the root
	// endpoint exact so unknown paths still receive the expected 404.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "grokcli2api-go", "version": config.Version,
		"openai_compat_endpoints":    []string{"/v1/chat/completions", "/v1/responses", "/v1/models"},
		"anthropic_compat_endpoints": []string{"/v1/messages"},
	})
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	ids := s.pool.Models()
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, grok.Model(id))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) model(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("model_id")
	if !s.pool.HasModel(id) {
		writeError(w, http.StatusNotFound, "unknown model: "+id, "invalid_request_error", "404")
		return
	}
	writeJSON(w, http.StatusOK, grok.Model(id))
}

func (s *Server) apiKeyStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": len(s.cfg.APIKeys) > 0, "key_count": len(s.cfg.APIKeys)})
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	if err := openai.ValidateChatRequest(body); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error(), "invalid_request_error", "422")
		return
	}
	wire := openai.PrepareChat(body)
	model := openai.String(body, "model", "")
	affinity := requestAffinity(r, body)
	convID := conversationID(affinity)
	if !openai.IsStreaming(body) {
		payload, err := s.client.DoJSON(r.Context(), http.MethodPost, "chat/completions", wire, affinity, convID, model, false)
		if err != nil {
			s.writeClientError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, openai.Normalize(payload, model, false))
		return
	}
	s.streamChat(w, r, wire, affinity, convID, model)
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	native := isGrokBuildClient(r)
	if err := openai.ValidateResponsesRequest(body, native); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error(), "invalid_request_error", "422")
		return
	}
	var compat *openai.ResponsesCompatibility
	var wire map[string]any
	if native {
		wire = openai.PrepareNativeResponses(body)
	} else {
		var err error
		wire, compat, err = openai.PrepareCompatibleResponses(body)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error(), "invalid_request_error", "422")
			return
		}
	}
	model := openai.String(body, "model", "")
	affinity := requestAffinity(r, body)
	convID := conversationID(affinity)
	if !openai.IsStreaming(body) {
		payload, err := s.client.DoJSON(r.Context(), http.MethodPost, "responses", wire, affinity, convID, fmt.Sprint(wire["model"]), true)
		if err != nil {
			s.writeClientError(w, err)
			return
		}
		if native {
			writeJSON(w, http.StatusOK, payload)
		} else {
			writeJSON(w, http.StatusOK, compat.NormalizeResponse(payload, model))
		}
		return
	}
	s.streamResponses(w, r, wire, affinity, convID, model, native, compat)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	version := strings.TrimSpace(r.Header.Get("anthropic-version"))
	if version == "" {
		version = anthropic.DefaultVersion
	}
	slog.Debug("anthropic request", "version", version)
	body, ok := decodeAnthropicRequest(w, r)
	if !ok {
		return
	}
	prepared, err := anthropic.Prepare(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	for _, field := range prepared.Warnings {
		slog.Warn("anthropic compatibility field stripped", "field", field, "path", r.URL.Path)
	}
	model := openai.String(body, "model", "")
	affinity := requestAffinity(r, body)
	convID := conversationID(affinity)
	if !openai.IsStreaming(body) {
		payload, err := s.client.DoJSON(r.Context(), http.MethodPost, "responses", prepared.Body, affinity, convID, fmt.Sprint(prepared.Body["model"]), true)
		if err != nil {
			s.writeAnthropicClientError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, anthropic.NormalizeResponse(payload, model))
		return
	}
	s.streamAnthropic(w, r, prepared.Body, affinity, convID, model)
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, wire map[string]any, affinity, convID, model string) {
	stream, err := s.client.OpenStream(r.Context(), "chat/completions", wire, affinity, convID, fmt.Sprint(wire["model"]), false)
	if err != nil {
		s.writeClientError(w, err)
		return
	}
	defer stream.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	roleSent := false
	for {
		event, ok, nextErr := stream.Next()
		if nextErr != nil {
			if !errors.Is(nextErr, context.Canceled) {
				_ = writeSSE(w, clientErrorPayload(nextErr))
			}
			break
		}
		if !ok || string(event.Data) == "[DONE]" {
			break
		}
		var chunk map[string]any
		if json.Unmarshal(event.Data, &chunk) == nil {
			chunk = openai.NormalizeChat(chunk, model, true)
			if !roleSent && openai.EnsureAssistantRole(chunk) {
				roleSent = true
			}
			_ = writeSSE(w, chunk)
		} else {
			_ = writeSSEData(w, event.Data)
		}
		flush()
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flush()
}

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, wire map[string]any, affinity, convID, model string, native bool, compat *openai.ResponsesCompatibility) {
	stream, err := s.client.OpenStream(r.Context(), "responses", wire, affinity, convID, fmt.Sprint(wire["model"]), true)
	if err != nil {
		s.writeClientError(w, err)
		return
	}
	defer stream.Close()
	prepareSSE(w)
	flush := flusher(w)
	for {
		event, ok, nextErr := stream.Next()
		if nextErr != nil {
			if !errors.Is(nextErr, context.Canceled) {
				payload, _ := json.Marshal(openai.ResponseStreamError(nextErr.Error(), "upstream_error"))
				_ = writeRawSSE(w, grok.SSEEvent{Event: "error", Data: payload})
				flush()
			}
			return
		}
		if !ok {
			return
		}
		if string(event.Data) == "[DONE]" && !native {
			continue
		}
		if !native {
			translated, translateErr := compat.TranslateStream(event.Event, event.Data)
			if translateErr != nil {
				payload, _ := json.Marshal(openai.ResponseStreamError(translateErr.Error(), "compatibility_error"))
				_ = writeRawSSE(w, grok.SSEEvent{Event: "error", Data: payload})
				flush()
				return
			}
			for index, output := range translated {
				translatedEvent := grok.SSEEvent{Event: output.Event, Data: output.Data}
				if index == 0 {
					translatedEvent.ID = event.ID
					translatedEvent.Retry = event.Retry
				}
				if translatedEvent.Event == "" {
					var data map[string]any
					if json.Unmarshal(translatedEvent.Data, &data) == nil {
						translatedEvent.Event = openai.EventType("", data)
					}
				}
				if err := writeRawSSE(w, translatedEvent); err != nil {
					return
				}
			}
			flush()
			continue
		}
		if err := writeRawSSE(w, event); err != nil {
			return
		}
		flush()
	}
}

func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, wire map[string]any, affinity, convID, model string) {
	stream, err := s.client.OpenStream(r.Context(), "responses", wire, affinity, convID, fmt.Sprint(wire["model"]), true)
	if err != nil {
		s.writeAnthropicClientError(w, err)
		return
	}
	defer stream.Close()
	prepareSSE(w)
	flush := flusher(w)
	translator := anthropic.NewStreamTranslator(model)
	for {
		event, ok, nextErr := stream.Next()
		if nextErr != nil {
			if !errors.Is(nextErr, context.Canceled) {
				_ = writeAnthropicSSE(w, anthropic.Event{Name: "error", Data: anthropic.Error(nextErr.Error(), "api_error")})
				flush()
			}
			return
		}
		if !ok {
			for _, translated := range translator.Finish() {
				_ = writeAnthropicSSE(w, translated)
			}
			flush()
			return
		}
		translated, err := translator.Handle(event)
		if err != nil {
			_ = writeAnthropicSSE(w, anthropic.Event{Name: "error", Data: anthropic.Error(err.Error(), "api_error")})
			flush()
			return
		}
		for _, outgoing := range translated {
			if err := writeAnthropicSSE(w, outgoing); err != nil {
				return
			}
			flush()
		}
	}
}

func (s *Server) proxyGET(path string, trace bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		affinity := requestAffinity(r, nil)
		payload, err := s.client.DoJSON(r.Context(), http.MethodGet, path, nil, affinity, conversationID(affinity), "", trace)
		if err != nil {
			s.writeClientError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, payload)
	}
}

func (s *Server) apiKeyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.APIKeys) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		candidate := strings.TrimSpace(r.Header.Get("api-key"))
		if value := strings.TrimSpace(r.Header.Get("x-api-key")); value != "" {
			candidate = value
		}
		if authz := r.Header.Get("Authorization"); len(authz) >= 7 && strings.EqualFold(authz[:7], "Bearer ") {
			candidate = strings.TrimSpace(authz[7:])
		}
		valid := 0
		for _, key := range s.cfg.APIKeys {
			valid |= constantEqual(candidate, key)
		}
		if valid != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			if r.URL.Path == "/v1/messages" {
				writeAnthropicError(w, http.StatusUnauthorized, "invalid or missing API key", "authentication_error")
			} else {
				writeError(w, http.StatusUnauthorized, "invalid or missing API key", "invalid_request_error", "invalid_api_key")
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) writeClientError(w http.ResponseWriter, err error) {
	var modelUnavailable *auth.ModelUnavailableError
	if errors.As(err, &modelUnavailable) {
		writeError(w, http.StatusNotFound, modelUnavailable.Error(), "invalid_request_error", "model_not_found")
		return
	}
	var unavailable *auth.UnavailableError
	if errors.As(err, &unavailable) {
		writeUnavailable(w, unavailable, false)
		return
	}
	var upstream *grok.APIError
	if errors.As(err, &upstream) {
		status := upstream.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		setRetryAfter(w, upstream.RetryAfter)
		writeJSON(w, status, upstreamError(upstream))
		return
	}
	if errors.Is(err, auth.ErrNoAuth) {
		writeError(w, http.StatusServiceUnavailable, "no usable upstream accounts", "upstream_error", "503")
		return
	}
	writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "502")
}

func (s *Server) writeAnthropicClientError(w http.ResponseWriter, err error) {
	var modelUnavailable *auth.ModelUnavailableError
	if errors.As(err, &modelUnavailable) {
		writeAnthropicError(w, http.StatusBadRequest, modelUnavailable.Error(), "invalid_request_error")
		return
	}
	var unavailable *auth.UnavailableError
	if errors.As(err, &unavailable) {
		writeUnavailable(w, unavailable, true)
		return
	}
	var upstream *grok.APIError
	if errors.As(err, &upstream) {
		status := upstream.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		setRetryAfter(w, upstream.RetryAfter)
		kind := anthropicErrorType(status)
		message := upstream.UpstreamMessage
		if message == "" {
			message = upstream.Error()
		}
		writeAnthropicError(w, status, message, kind)
		return
	}
	if errors.Is(err, auth.ErrNoAuth) {
		writeAnthropicError(w, http.StatusServiceUnavailable, "no usable upstream accounts", "api_error")
		return
	}
	writeAnthropicError(w, http.StatusBadGateway, err.Error(), "api_error")
}

func clientErrorPayload(err error) map[string]any {
	var modelUnavailable *auth.ModelUnavailableError
	if errors.As(err, &modelUnavailable) {
		return openai.Error(modelUnavailable.Error(), "invalid_request_error", "model_not_found")
	}
	var unavailable *auth.UnavailableError
	if errors.As(err, &unavailable) {
		if unavailable.Cooling {
			return openai.Error("all upstream accounts are cooling down", "rate_limit_error", "429")
		}
		return openai.Error("no usable upstream accounts", "upstream_error", "503")
	}
	var upstream *grok.APIError
	if errors.As(err, &upstream) {
		return upstreamError(upstream)
	}
	typeName, code := "upstream_error", "502"
	if errors.Is(err, auth.ErrNoAuth) {
		typeName, code = "upstream_error", "503"
	}
	return openai.Error(err.Error(), typeName, code)
}

func upstreamError(e *grok.APIError) map[string]any {
	kind := "invalid_request_error"
	if e.Status == http.StatusTooManyRequests {
		kind = "rate_limit_error"
	} else if e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden {
		kind = "authentication_error"
	} else if e.Status >= 500 {
		kind = "upstream_error"
	}
	code := e.UpstreamCode
	if code == "" {
		code = fmt.Sprint(e.Status)
	}
	message := e.UpstreamMessage
	if message == "" {
		message = e.Error()
	}
	inner := map[string]any{"message": message, "type": kind, "code": code, "param": nil}
	if code == "personal-team-blocked:spending-limit" {
		inner["hint"] = "your Grok account hit the spending limit. Add credits at https://grok.com/?_s=usage or upgrade at https://grok.com/supergrok."
	}
	return map[string]any{"error": inner}
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	defer r.Body.Close()
	var body map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request_error", "400")
		return nil, false
	}
	if body == nil {
		writeError(w, http.StatusBadRequest, "JSON object required", "invalid_request_error", "400")
		return nil, false
	}
	return body, true
}

func decodeAnthropicRequest(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	defer r.Body.Close()
	var body map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	if err := dec.Decode(&body); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "invalid_request_error")
		return nil, false
	}
	if body == nil {
		writeAnthropicError(w, http.StatusBadRequest, "JSON object required", "invalid_request_error")
		return nil, false
	}
	return body, true
}

func writeError(w http.ResponseWriter, status int, message, kind, code string) {
	writeJSON(w, status, openai.Error(message, kind, code))
}

func writeUnavailable(w http.ResponseWriter, unavailable *auth.UnavailableError, anthropicResponse bool) {
	status, message := http.StatusServiceUnavailable, "no usable upstream accounts"
	kind := "upstream_error"
	if unavailable.Cooling {
		status, message, kind = http.StatusTooManyRequests, "all upstream accounts are cooling down", "rate_limit_error"
		if unavailable.RetryAfter > 0 {
			seconds := int64(unavailable.RetryAfter.Round(time.Second) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", fmt.Sprint(seconds))
		}
	}
	if anthropicResponse {
		writeAnthropicError(w, status, message, anthropicErrorType(status))
		return
	}
	writeError(w, status, message, kind, fmt.Sprint(status))
}

func setRetryAfter(w http.ResponseWriter, delay time.Duration) {
	if delay <= 0 {
		return
	}
	seconds := int64(delay.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprint(seconds))
}
func writeAnthropicError(w http.ResponseWriter, status int, message, kind string) {
	writeJSON(w, status, anthropic.Error(message, kind))
}
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
func writeSSE(w io.Writer, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}
func writeSSEData(w io.Writer, data []byte) error {
	for _, line := range strings.Split(string(data), "\n") {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}
func writeRawSSE(w io.Writer, event grok.SSEEvent) error {
	if event.Event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event.Event); err != nil {
			return err
		}
	}
	if event.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", event.ID); err != nil {
			return err
		}
	}
	if event.Retry != "" {
		if _, err := fmt.Fprintf(w, "retry: %s\n", event.Retry); err != nil {
			return err
		}
	}
	return writeSSEData(w, event.Data)
}
func writeAnthropicSSE(w io.Writer, event anthropic.Event) error {
	b, err := json.Marshal(event.Data)
	if err != nil {
		return err
	}
	return writeRawSSE(w, grok.SSEEvent{Event: event.Name, Data: b})
}
func prepareSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}
func flusher(w http.ResponseWriter) func() {
	return func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}
func isGrokBuildClient(r *http.Request) bool {
	for _, name := range []string{"x-grok-client-name", "x-grok-client-identifier", "x-grok-client-surface", "x-grok-client-version"} {
		if strings.TrimSpace(r.Header.Get(name)) != "" {
			return true
		}
	}
	ua := strings.ToLower(r.UserAgent())
	for _, marker := range []string{"grok-build", "grok-shell/", "grok-pager/", "xai-grok-cli/"} {
		if strings.Contains(ua, marker) {
			return true
		}
	}
	return false
}

func requestAffinity(r *http.Request, body map[string]any) string {
	if value := strings.TrimSpace(r.Header.Get("X-Grok-Session-ID")); value != "" {
		return "session:" + value
	}
	if value := openai.String(body, "prompt_cache_key", ""); value != "" {
		return "cache:" + value
	}
	if value := openai.String(body, "previous_response_id", ""); value != "" {
		return "previous:" + value
	}
	if value := openai.String(body, "user", ""); value != "" {
		return "user:" + value
	}
	if metadata, ok := body["metadata"].(map[string]any); ok {
		if value := openai.String(metadata, "user_id", ""); value != "" {
			return "user:" + value
		}
	}
	return ""
}

func conversationID(affinity string) string {
	if affinity == "" {
		return grok.NewID()
	}
	sum := sha256.Sum256([]byte(affinity))
	return hex.EncodeToString(sum[:16])
}
func constantEqual(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b))
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		slog.Debug("request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(start))
	})
}
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				slog.Error("panic", "error", value)
				writeError(w, http.StatusInternalServerError, "internal server error", "server_error", "500")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
