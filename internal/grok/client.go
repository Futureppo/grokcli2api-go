package grok

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
)

type APIError struct {
	Status          int
	Body            string
	RequestID       string
	UpstreamCode    string
	UpstreamMessage string
}

func (e *APIError) Error() string {
	parts := []string{fmt.Sprintf("upstream grok %d", e.Status)}
	if e.UpstreamCode != "" {
		parts = append(parts, "code="+e.UpstreamCode)
	}
	if e.UpstreamMessage != "" {
		parts = append(parts, e.UpstreamMessage)
	} else if e.Body != "" {
		body := e.Body
		if len(body) > 200 {
			body = body[:200]
		}
		parts = append(parts, body)
	}
	return strings.Join(parts, " | ")
}

type Client struct {
	cfg       config.Config
	store     *auth.Store
	http      *http.Client
	agentID   string
	sessionID string
}

// SSEEvent is one complete Server-Sent Event. Data contains the joined data
// lines (separated by newlines), without the "data:" prefix.
type SSEEvent struct {
	Event string
	Data  []byte
	ID    string
	Retry string
}

// EventStream owns an upstream streaming response. Call Close when iteration
// is stopped before Next reports EOF.
type EventStream struct {
	response *http.Response
	scanner  *bufio.Scanner
	done     bool
}

func NewClient(cfg config.Config, store *auth.Store) (*Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 32
	transport.IdleConnTimeout = 90 * time.Second
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 90 * time.Second
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.TLSInsecureSkipVerify} // #nosec G402: explicit operator option
	if len(cfg.NoProxy) > 0 {
		existing := strings.TrimSpace(os.Getenv("NO_PROXY"))
		joined := strings.Join(cfg.NoProxy, ",")
		if existing != "" {
			joined = existing + "," + joined
		}
		_ = os.Setenv("NO_PROXY", joined)
	}
	if cfg.ProxyURL != "" {
		proxyURL, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid GROK_PROXY_URL: %w", err)
		}
		proxy := http.ProxyURL(proxyURL)
		patterns := splitProxyPatterns(os.Getenv("NO_PROXY"))
		transport.Proxy = func(req *http.Request) (*url.URL, error) {
			if bypassProxy(req.URL.Hostname(), patterns) {
				return nil, nil
			}
			return proxy(req)
		}
	} else {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &Client{
		cfg: cfg, store: store,
		http:    &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		agentID: NewAgentID(), sessionID: NewSessionID(),
	}, nil
}

func splitProxyPatterns(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func bypassProxy(host string, patterns []string) bool {
	host = strings.Trim(strings.ToLower(host), "[]")
	ip := net.ParseIP(host)
	for _, raw := range patterns {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		if pattern == "*" {
			return true
		}
		if _, network, err := net.ParseCIDR(pattern); err == nil && ip != nil && network.Contains(ip) {
			return true
		}
		if candidateHost, _, err := net.SplitHostPort(pattern); err == nil {
			pattern = candidateHost
		}
		pattern = strings.TrimPrefix(pattern, "*.")
		pattern = strings.TrimPrefix(pattern, ".")
		if host == pattern || strings.HasSuffix(host, "."+pattern) {
			return true
		}
	}
	return false
}

func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) URL(path string) string {
	return c.cfg.ChatProxyBaseURL + "/" + c.cfg.ChatProxyVersion + "/" + strings.TrimLeft(path, "/")
}

func (c *Client) DoJSON(ctx context.Context, method, path string, body map[string]any, convID, model string, trace bool) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := c.request(ctx, method, path, reader, convID, model, trace)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	defer resp.Body.Close()
	payload, err := readResponseBody(resp, 16<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, parseAPIError(resp, payload)
	}
	if len(payload) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return map[string]any{"raw": string(payload)}, nil
	}
	return out, nil
}

func (c *Client) StreamJSON(ctx context.Context, path string, body map[string]any, convID, model string, trace bool, handle func(map[string]any) error) error {
	stream, err := c.OpenStream(ctx, path, body, convID, model, trace)
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		event, ok, err := stream.Next()
		if err != nil {
			return err
		}
		if !ok || string(event.Data) == "[DONE]" {
			return nil
		}
		var chunk map[string]any
		if json.Unmarshal(event.Data, &chunk) != nil {
			continue
		}
		if err := handle(chunk); err != nil {
			return err
		}
	}
}

// OpenStream opens and validates the upstream response before the caller
// commits downstream HTTP headers. This preserves upstream 4xx/5xx statuses
// for streaming requests.
func (c *Client) OpenStream(ctx context.Context, path string, body map[string]any, convID, model string, trace bool) (*EventStream, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := c.request(ctx, http.MethodPost, path, bytes.NewReader(b), convID, model, trace)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if trace {
		req.Header.Set("User-Agent", chatUserAgent(c.cfg))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream request: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		payload, readErr := readResponseBody(resp, 4<<20)
		if readErr != nil {
			return nil, readErr
		}
		return nil, parseAPIError(resp, payload)
	}
	scanner := bufio.NewScanner(responseReader(resp))
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	return &EventStream{response: resp, scanner: scanner}, nil
}

func (s *EventStream) Close() error {
	if s.response == nil || s.response.Body == nil {
		return nil
	}
	err := s.response.Body.Close()
	s.response = nil
	return err
}

// Next parses a complete SSE record, including multi-line data fields.
func (s *EventStream) Next() (SSEEvent, bool, error) {
	if s.done {
		return SSEEvent{}, false, nil
	}
	var event SSEEvent
	hasField := false
	for s.scanner.Scan() {
		line := strings.TrimSuffix(s.scanner.Text(), "\r")
		if line == "" {
			if hasField {
				return event, true, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field, value = line, ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event.Event = value
			hasField = true
		case "data":
			if event.Data != nil {
				event.Data = append(event.Data, '\n')
			}
			event.Data = append(event.Data, value...)
			hasField = true
		case "id":
			event.ID = value
			hasField = true
		case "retry":
			event.Retry = value
			hasField = true
		}
	}
	s.done = true
	if err := s.scanner.Err(); err != nil {
		return SSEEvent{}, false, err
	}
	if hasField {
		return event, true, nil
	}
	return SSEEvent{}, false, nil
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader, convID, model string, trace bool) (*http.Request, error) {
	session, err := c.store.Current(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL(path), body)
	if err != nil {
		return nil, err
	}
	req.Header = BuildHeaders(c.cfg, session, c.agentID, c.sessionID, convID, model, trace)
	return req, nil
}

func parseAPIError(resp *http.Response, body []byte) *APIError {
	e := &APIError{Status: resp.StatusCode, Body: string(body), RequestID: resp.Header.Get("x-grok-req-id")}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) == nil {
		e.UpstreamCode, _ = parsed["code"].(string)
		e.UpstreamMessage, _ = parsed["error"].(string)
		if inner, ok := parsed["error"].(map[string]any); ok {
			if e.UpstreamCode == "" {
				e.UpstreamCode, _ = inner["code"].(string)
			}
			if e.UpstreamMessage == "" {
				e.UpstreamMessage, _ = inner["message"].(string)
			}
		}
		if e.UpstreamMessage == "" {
			e.UpstreamMessage, _ = parsed["message"].(string)
		}
	}
	return e
}

func responseReader(resp *http.Response) io.Reader {
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		if gz, err := gzip.NewReader(resp.Body); err == nil {
			return gz
		}
	}
	return resp.Body
}

func readResponseBody(resp *http.Response, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(responseReader(resp), max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("upstream response exceeds %d bytes", max)
	}
	return b, nil
}
