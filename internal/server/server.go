package server

import (
	"context"
	"crypto/subtle"
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
	store  *auth.Store
	client *grok.Client
	mux    *http.ServeMux
}

func New(cfg config.Config) (*Server, error) {
	var provider auth.Provider
	switch {
	case cfg.SessionToken != "":
		provider = auth.FixedProvider{Token: cfg.SessionToken, Surface: cfg.ClientSurface}
	case cfg.AuthFile != "":
		provider = auth.FileProvider{Path: cfg.AuthFile, Surface: cfg.ClientSurface}
	case cfg.OAuthClientID != "":
		provider = auth.OAuthProvider{}
	default:
		provider = auth.NoopProvider{}
	}
	store := auth.NewStore(provider)
	client, err := grok.NewClient(cfg, store)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, store: store, client: client, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

func (s *Server) Close() { s.client.Close() }

func (s *Server) Handler() http.Handler {
	return recoverer(requestLogger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mux.ServeHTTP(w, r)
	})))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.root)
	s.mux.HandleFunc("GET /docs", s.docs)
	s.mux.HandleFunc("GET /openapi.json", s.openapi)
	s.mux.HandleFunc("GET /v1/health", s.health)
	s.mux.HandleFunc("GET /v1/models", s.models)
	s.mux.HandleFunc("GET /v1/models/{model_id}", s.model)
	s.mux.HandleFunc("GET /v1/auth/api-key", s.apiKeyStatus)

	s.protected("POST /v1/auth/refresh", s.authRefresh)
	s.protected("GET /v1/auth/status", s.authStatus)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "grokcli2api-go", "version": config.Version, "docs": "/docs",
		"openai_compat_endpoints":    []string{"/v1/chat/completions", "/v1/responses", "/v1/models", "/v1/health"},
		"anthropic_compat_endpoints": []string{"/v1/messages"},
	})
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, grok.Models())
}

func (s *Server) model(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("model_id")
	if !grok.HasModel(id) {
		writeError(w, http.StatusNotFound, "unknown model: "+id, "invalid_request_error", "404")
		return
	}
	writeJSON(w, http.StatusOK, grok.Model(id))
}

func (s *Server) apiKeyStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": len(s.cfg.APIKeys) > 0, "key_count": len(s.cfg.APIKeys)})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	payload, err := s.client.DoJSON(r.Context(), http.MethodGet, "models", nil, "", "", false)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "upstream": payload})
		return
	}
	if errors.Is(err, auth.ErrNoAuth) || strings.Contains(err.Error(), "no auth configured") {
		writeJSON(w, http.StatusOK, map[string]any{"status": "no_auth"})
		return
	}
	var upstream *grok.APIError
	if errors.As(err, &upstream) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "degraded", "upstream_status": upstream.Status})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "degraded", "error": err.Error()})
}

func (s *Server) authRefresh(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.ForceRefresh(r.Context())
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.Current(r.Context())
	if err != nil {
		writeAuthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"surface": session.Surface, "user_id": session.UserID, "expired": session.Expired(), "expires_at": session.ExpiresAt})
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
	model := openai.String(body, "model", "grok-build")
	convID := openai.String(body, "user", grok.NewID())
	if !openai.IsStreaming(body) {
		payload, err := s.client.DoJSON(r.Context(), http.MethodPost, "chat/completions", wire, convID, model, false)
		if err != nil {
			s.writeClientError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, openai.Normalize(payload, model, false))
		return
	}
	s.streamChat(w, r, wire, convID, model)
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
	wire := openai.PrepareResponses(body)
	if native {
		wire = openai.PrepareNativeResponses(body)
	}
	model := openai.String(body, "model", "grok-build")
	convID := openai.String(body, "user", grok.NewID())
	if !openai.IsStreaming(body) {
		payload, err := s.client.DoJSON(r.Context(), http.MethodPost, "responses", wire, convID, fmt.Sprint(wire["model"]), true)
		if err != nil {
			s.writeClientError(w, err)
			return
		}
		if native {
			writeJSON(w, http.StatusOK, payload)
		} else {
			writeJSON(w, http.StatusOK, openai.NormalizeResponse(payload, model))
		}
		return
	}
	s.streamResponses(w, r, wire, convID, model, native)
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
	model := openai.String(body, "model", "grok-build")
	convID := openai.String(prepared.Body, "user", grok.NewID())
	if !openai.IsStreaming(body) {
		payload, err := s.client.DoJSON(r.Context(), http.MethodPost, "responses", prepared.Body, convID, fmt.Sprint(prepared.Body["model"]), true)
		if err != nil {
			s.writeAnthropicClientError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, anthropic.NormalizeResponse(payload, model))
		return
	}
	s.streamAnthropic(w, r, prepared.Body, convID, model)
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, wire map[string]any, convID, model string) {
	stream, err := s.client.OpenStream(r.Context(), "chat/completions", wire, convID, fmt.Sprint(wire["model"]), false)
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

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, wire map[string]any, convID, model string, native bool) {
	stream, err := s.client.OpenStream(r.Context(), "responses", wire, convID, fmt.Sprint(wire["model"]), true)
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
		if !native && event.Event == "" {
			var data map[string]any
			if json.Unmarshal(event.Data, &data) == nil {
				event.Event = openai.EventType("", data)
			}
		}
		if err := writeRawSSE(w, event); err != nil {
			return
		}
		flush()
	}
}

func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, wire map[string]any, convID, model string) {
	stream, err := s.client.OpenStream(r.Context(), "responses", wire, convID, fmt.Sprint(wire["model"]), true)
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
		payload, err := s.client.DoJSON(r.Context(), http.MethodGet, path, nil, "", "", trace)
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
	var upstream *grok.APIError
	if errors.As(err, &upstream) {
		status := upstream.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, upstreamError(upstream))
		return
	}
	if errors.Is(err, auth.ErrNoAuth) || strings.Contains(err.Error(), "auth") {
		writeAuthError(w, err)
		return
	}
	writeError(w, http.StatusBadGateway, err.Error(), "upstream_error", "502")
}

func (s *Server) writeAnthropicClientError(w http.ResponseWriter, err error) {
	var upstream *grok.APIError
	if errors.As(err, &upstream) {
		status := upstream.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		kind := anthropicErrorType(status)
		message := upstream.UpstreamMessage
		if message == "" {
			message = upstream.Error()
		}
		writeAnthropicError(w, status, message, kind)
		return
	}
	if errors.Is(err, auth.ErrNoAuth) || strings.Contains(strings.ToLower(err.Error()), "auth") {
		writeAnthropicError(w, http.StatusUnauthorized, err.Error(), "authentication_error")
		return
	}
	writeAnthropicError(w, http.StatusBadGateway, err.Error(), "api_error")
}

func clientErrorPayload(err error) map[string]any {
	var upstream *grok.APIError
	if errors.As(err, &upstream) {
		return upstreamError(upstream)
	}
	typeName, code := "upstream_error", "502"
	if errors.Is(err, auth.ErrNoAuth) || strings.Contains(err.Error(), "auth") {
		typeName, code = "auth_error", "401"
	}
	return openai.Error(err.Error(), typeName, code)
}

func upstreamError(e *grok.APIError) map[string]any {
	kind := "invalid_request_error"
	if e.Status >= 500 {
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

func writeAuthError(w http.ResponseWriter, err error) {
	writeError(w, http.StatusUnauthorized, err.Error(), "auth_error", "401")
}
func writeError(w http.ResponseWriter, status int, message, kind, code string) {
	writeJSON(w, status, openai.Error(message, kind, code))
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

func (s *Server) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><html><head><title>grokcli2api-go</title><link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css"></head><body><div id="swagger-ui"></div><script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script><script>SwaggerUIBundle({url:'/openapi.json',dom_id:'#swagger-ui'})</script></body></html>`)
}

func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	paths := map[string]any{}
	for _, entry := range []struct{ path, method, summary string }{
		{"/v1/health", "get", "Health probe"}, {"/v1/models", "get", "List models"},
		{"/v1/chat/completions", "post", "Create chat completion"}, {"/v1/responses", "post", "Create response"},
		{"/v1/messages", "post", "Create Anthropic-compatible message"},
		{"/v1/auth/status", "get", "Authentication status"}, {"/v1/auth/refresh", "post", "Refresh authentication"},
	} {
		paths[entry.path] = map[string]any{entry.method: map[string]any{"summary": entry.summary, "responses": map[string]any{"200": map[string]any{"description": "Success"}}}}
	}
	writeJSON(w, http.StatusOK, map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "grokcli2api-go", "version": config.Version}, "paths": paths})
}
