package grok

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
)

const quotaErrorCode = "personal-team-blocked:spending-limit"

type APIError struct {
	Status          int
	Body            string
	RequestID       string
	UpstreamCode    string
	UpstreamMessage string
	RetryAfter      time.Duration
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
	cfg        config.Config
	pool       *auth.Pool
	http       *http.Client
	modelsMu   sync.Mutex
	modelStart sync.Once
	modelClose chan struct{}
	modelWG    sync.WaitGroup
	closeOnce  sync.Once
}

type SSEEvent struct {
	Event string
	Data  []byte
	ID    string
	Retry string
}

type EventStream struct {
	response      *http.Response
	scanner       *bufio.Scanner
	done          bool
	pool          *auth.Pool
	lease         *auth.Lease
	accountID     string
	model         string
	quotaCooldown time.Duration
	closeOnce     sync.Once
}

func NewHTTPClient(cfg config.Config) (*http.Client, error) {
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
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func NewClient(cfg config.Config, pool *auth.Pool, httpClient *http.Client) (*Client, error) {
	if cfg.RetryMaxAttempts < 1 {
		cfg.RetryMaxAttempts = 3
	}
	if cfg.RetryBaseDelay <= 0 {
		cfg.RetryBaseDelay = 200 * time.Millisecond
	}
	if cfg.RateLimitCooldown <= 0 {
		cfg.RateLimitCooldown = time.Minute
	}
	if cfg.QuotaCooldown <= 0 {
		cfg.QuotaCooldown = 24 * time.Hour
	}
	if cfg.ModelsRefreshInterval <= 0 {
		cfg.ModelsRefreshInterval = 6 * time.Hour
	}
	if httpClient == nil {
		var err error
		httpClient, err = NewHTTPClient(cfg)
		if err != nil {
			return nil, err
		}
	}
	return &Client{cfg: cfg, pool: pool, http: httpClient, modelClose: make(chan struct{})}, nil
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

func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.modelClose)
		c.modelWG.Wait()
		c.http.CloseIdleConnections()
	})
}

func (c *Client) InitializeModels(ctx context.Context) error {
	if err := c.RefreshModels(ctx, false); err != nil && len(c.pool.Models()) == 0 {
		return err
	}
	if len(c.pool.Models()) == 0 {
		return errors.New("no models discovered from credential accounts")
	}
	c.modelStart.Do(func() {
		c.modelWG.Add(1)
		go c.modelRefreshLoop()
	})
	return nil
}

func (c *Client) RefreshModels(ctx context.Context, force bool) error {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	ids := c.pool.AccountIDs()
	if !force {
		ids = c.pool.AccountsNeedingModelRefresh(c.cfg.ModelsRefreshInterval)
	}
	if len(ids) == 0 {
		return nil
	}
	workers := c.cfg.AuthRefreshConcurrency
	if workers < 1 {
		workers = 4
	}
	if workers > len(ids) {
		workers = len(ids)
	}
	jobs := make(chan string)
	results := make(chan error, len(ids))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				models, err := c.fetchAccountModels(ctx, id, false)
				if err == nil {
					err = c.pool.UpdateModels(id, models, time.Now())
				}
				results <- err
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case jobs <- id:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	c.pool.RebuildSchedulingSnapshot()
	slog.Info("account model catalogs refreshed", "requested", len(ids), "succeeded", succeeded, "failed", len(ids)-succeeded, "models", len(c.pool.Models()))
	if succeeded != len(ids) {
		return fmt.Errorf("model discovery failed for %d of %d credential accounts", len(ids)-succeeded, len(ids))
	}
	return nil
}

func (c *Client) fetchAccountModels(ctx context.Context, accountID string, refreshed bool) ([]string, error) {
	lease, err := c.pool.AcquireAccountForMetadata(ctx, accountID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	resp, _, err := c.do(ctx, lease, http.MethodGet, "models", nil, NewID(), "", false, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := readResponseBody(resp, 4<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		apiErr := parseAPIError(resp, payload)
		if isAuthError(apiErr) && !refreshed && c.pool.Refresh(ctx, accountID) == nil {
			return c.fetchAccountModels(ctx, accountID, true)
		}
		return nil, apiErr
	}
	return parseModelIDs(payload)
}

func parseModelIDs(payload []byte) ([]string, error) {
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	collect := func(values any) {
		items, _ := values.([]any)
		for _, item := range items {
			switch value := item.(type) {
			case string:
				if strings.TrimSpace(value) != "" {
					seen[strings.TrimSpace(value)] = struct{}{}
				}
			case map[string]any:
				id := stringField(value, "id")
				if id == "" {
					id = stringField(value, "name")
				}
				if id != "" {
					seen[id] = struct{}{}
				}
			}
		}
	}
	collect(raw["data"])
	collect(raw["models"])
	models := make([]string, 0, len(seen))
	for id := range seen {
		models = append(models, id)
	}
	sort.Strings(models)
	if len(models) == 0 {
		return nil, errors.New("upstream returned an empty model catalog")
	}
	return models, nil
}

func (c *Client) modelRefreshLoop() {
	defer c.modelWG.Done()
	checkInterval := c.cfg.AuthsReloadInterval
	if checkInterval <= 0 {
		checkInterval = 30 * time.Second
	}
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			if err := c.RefreshModels(ctx, false); err != nil {
				slog.Warn("account model refresh failed", "error", err)
			}
			cancel()
		case <-c.modelClose:
			return
		}
	}
}

func (c *Client) URL(path string) string {
	return c.cfg.ChatProxyBaseURL + "/" + c.cfg.ChatProxyVersion + "/" + strings.TrimLeft(path, "/")
}

func (c *Client) DoJSON(ctx context.Context, method, path string, body map[string]any, affinity, convID, model string, trace bool) (map[string]any, error) {
	payload, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	used := map[string]struct{}{}
	refreshed := map[string]bool{}
	preferredID := ""
	var lastErr error
	for len(used) < c.cfg.RetryMaxAttempts {
		var lease *auth.Lease
		var err error
		if preferredID != "" {
			lease, err = c.pool.AcquireAccount(ctx, preferredID)
			preferredID = ""
		} else {
			lease, err = c.pool.Acquire(ctx, affinity, model, used)
		}
		if err != nil {
			var unavailable *auth.UnavailableError
			if errors.As(err, &unavailable) {
				return nil, err
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		accountID := lease.AccountID()
		used[accountID] = struct{}{}
		resp, wrote, err := c.do(ctx, lease, method, path, payload, convID, model, trace, false)
		if err != nil {
			lease.Release()
			lastErr = err
			if ctx.Err() != nil || wrote || len(used) >= c.cfg.RetryMaxAttempts {
				return nil, err
			}
			if err := c.backoff(ctx, len(used)); err != nil {
				return nil, err
			}
			continue
		}
		data, readErr := readResponseBody(resp, 16<<20)
		resp.Body.Close()
		if readErr != nil {
			lease.Release()
			return nil, readErr
		}
		if resp.StatusCode >= 400 {
			apiErr := parseAPIError(resp, data)
			lease.Release()
			lastErr = apiErr
			if isAuthError(apiErr) && !refreshed[accountID] {
				refreshed[accountID] = true
				if refreshErr := c.pool.Refresh(ctx, accountID); refreshErr == nil {
					delete(used, accountID)
					preferredID = accountID
					continue
				}
				c.pool.Disable(accountID, "authentication_failed")
				continue
			}
			if isAuthError(apiErr) {
				c.pool.Disable(accountID, "authentication_failed")
				if len(used) < c.cfg.RetryMaxAttempts {
					continue
				}
			}
			if !c.handleRetryable(accountID, apiErr) || len(used) >= c.cfg.RetryMaxAttempts {
				if strings.EqualFold(apiErr.UpstreamCode, quotaErrorCode) {
					apiErr.Status = http.StatusTooManyRequests
					apiErr.UpstreamCode = "account_pool_retry_exhausted"
					apiErr.UpstreamMessage = "upstream account retry budget exhausted"
					apiErr.RetryAfter = c.cfg.QuotaCooldown
				}
				return nil, apiErr
			}
			if apiErr.Status >= 500 {
				if err := c.backoff(ctx, len(used)); err != nil {
					return nil, err
				}
			}
			continue
		}
		lease.Release()
		return c.decodeSuccess(data, affinity, model, accountID)
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, auth.ErrNoAuth
}

func (c *Client) decodeSuccess(payload []byte, affinity, model, accountID string) (map[string]any, error) {
	c.pool.Bind(affinity, model, accountID)
	if len(payload) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(payload, &out); err != nil {
		return map[string]any{"raw": string(payload)}, nil
	}
	if id := responseID(out); id != "" {
		c.pool.BindResponseID(id, model, accountID)
	}
	return out, nil
}

func (c *Client) OpenStream(ctx context.Context, path string, body map[string]any, affinity, convID, model string, trace bool) (*EventStream, error) {
	payload, err := marshalBody(body)
	if err != nil {
		return nil, err
	}
	used := map[string]struct{}{}
	refreshed := map[string]bool{}
	preferredID := ""
	var lastErr error
	for len(used) < c.cfg.RetryMaxAttempts {
		var lease *auth.Lease
		var err error
		if preferredID != "" {
			lease, err = c.pool.AcquireAccount(ctx, preferredID)
			preferredID = ""
		} else {
			lease, err = c.pool.Acquire(ctx, affinity, model, used)
		}
		if err != nil {
			var unavailable *auth.UnavailableError
			if errors.As(err, &unavailable) {
				return nil, err
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		accountID := lease.AccountID()
		used[accountID] = struct{}{}
		resp, wrote, err := c.do(ctx, lease, http.MethodPost, path, payload, convID, model, trace, true)
		if err != nil {
			lease.Release()
			lastErr = err
			if ctx.Err() != nil || wrote || len(used) >= c.cfg.RetryMaxAttempts {
				return nil, err
			}
			if err := c.backoff(ctx, len(used)); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode >= 400 {
			data, readErr := readResponseBody(resp, 4<<20)
			resp.Body.Close()
			lease.Release()
			if readErr != nil {
				return nil, readErr
			}
			apiErr := parseAPIError(resp, data)
			lastErr = apiErr
			if isAuthError(apiErr) && !refreshed[accountID] {
				refreshed[accountID] = true
				if c.pool.Refresh(ctx, accountID) == nil {
					delete(used, accountID)
					preferredID = accountID
					continue
				}
				c.pool.Disable(accountID, "authentication_failed")
				used[accountID] = struct{}{}
				continue
			}
			if isAuthError(apiErr) {
				c.pool.Disable(accountID, "authentication_failed")
				if len(used) < c.cfg.RetryMaxAttempts {
					continue
				}
			}
			if !c.handleRetryable(accountID, apiErr) || len(used) >= c.cfg.RetryMaxAttempts {
				if strings.EqualFold(apiErr.UpstreamCode, quotaErrorCode) {
					apiErr.Status = http.StatusTooManyRequests
					apiErr.UpstreamCode = "account_pool_retry_exhausted"
					apiErr.UpstreamMessage = "upstream account retry budget exhausted"
					apiErr.RetryAfter = c.cfg.QuotaCooldown
				}
				return nil, apiErr
			}
			if apiErr.Status >= 500 {
				if err := c.backoff(ctx, len(used)); err != nil {
					return nil, err
				}
			}
			continue
		}
		c.pool.Bind(affinity, model, accountID)
		scanner := bufio.NewScanner(responseReader(resp))
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		return &EventStream{response: resp, scanner: scanner, pool: c.pool, lease: lease, accountID: accountID, model: model, quotaCooldown: c.cfg.QuotaCooldown}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, auth.ErrNoAuth
}

func (c *Client) do(ctx context.Context, lease *auth.Lease, method, path string, payload []byte, convID, model string, trace, stream bool) (*http.Response, bool, error) {
	var wrote atomic.Bool
	requestCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) }})
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(requestCtx, method, c.URL(path), reader)
	if err != nil {
		return nil, false, err
	}
	req.Header = BuildHeaders(c.cfg, lease.Session(), lease.AgentID(), lease.SessionID(), convID, model, trace)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
		if trace {
			req.Header.Set("User-Agent", chatUserAgent(c.cfg))
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrote.Load(), fmt.Errorf("upstream request: %w", err)
	}
	return resp, wrote.Load(), nil
}

func (c *Client) handleRetryable(accountID string, err *APIError) bool {
	if strings.EqualFold(err.UpstreamCode, quotaErrorCode) {
		c.pool.MarkCooldown(accountID, "quota_exhausted", c.cfg.QuotaCooldown)
		return true
	}
	if err.Status == http.StatusTooManyRequests {
		cooldown := err.RetryAfter
		if cooldown <= 0 {
			cooldown = c.cfg.RateLimitCooldown
		}
		if cooldown > 15*time.Minute {
			cooldown = 15 * time.Minute
		}
		c.pool.MarkCooldown(accountID, "rate_limited", cooldown)
		return true
	}
	return err.Status == http.StatusBadGateway || err.Status == http.StatusServiceUnavailable || err.Status == http.StatusGatewayTimeout
}

func isAuthError(err *APIError) bool {
	if err.Status == http.StatusUnauthorized {
		return true
	}
	if err.Status != http.StatusForbidden {
		return false
	}
	text := strings.ToLower(err.UpstreamCode + " " + err.UpstreamMessage)
	return strings.Contains(text, "auth") || strings.Contains(text, "token")
}

func (c *Client) backoff(ctx context.Context, attempt int) error {
	delay := c.cfg.RetryBaseDelay
	if delay <= 0 {
		delay = 200 * time.Millisecond
	}
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= 2*time.Second {
			delay = 2 * time.Second
			break
		}
	}
	if delay > time.Millisecond {
		delay = time.Duration(rand.Int63n(int64(delay))) // #nosec G404: retry jitter is not security-sensitive
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *EventStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.response != nil && s.response.Body != nil {
			err = s.response.Body.Close()
		}
		if s.lease != nil {
			s.lease.Release()
		}
		s.response = nil
	})
	return err
}

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
				s.observe(event.Data)
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
			event.Event, hasField = value, true
		case "data":
			if event.Data != nil {
				event.Data = append(event.Data, '\n')
			}
			event.Data = append(event.Data, value...)
			hasField = true
		case "id":
			event.ID, hasField = value, true
		case "retry":
			event.Retry, hasField = value, true
		}
	}
	s.done = true
	err := s.scanner.Err()
	_ = s.Close()
	if err != nil {
		return SSEEvent{}, false, err
	}
	if hasField {
		s.observe(event.Data)
		return event, true, nil
	}
	return SSEEvent{}, false, nil
}

func (s *EventStream) observe(data []byte) {
	if len(data) == 0 || string(data) == "[DONE]" {
		return
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	if id := responseID(payload); id != "" {
		s.pool.BindResponseID(id, s.model, s.accountID)
	}
	code := stringField(payload, "code")
	if inner, ok := payload["error"].(map[string]any); ok && code == "" {
		code = stringField(inner, "code")
	}
	if strings.EqualFold(code, quotaErrorCode) {
		s.pool.MarkCooldown(s.accountID, "quota_exhausted", s.quotaCooldown)
	}
}

func responseID(payload map[string]any) string {
	if id := stringField(payload, "id"); id != "" {
		return id
	}
	if response, ok := payload["response"].(map[string]any); ok {
		return stringField(response, "id")
	}
	return ""
}

func stringField(payload map[string]any, key string) string {
	v, _ := payload[key].(string)
	return v
}

func marshalBody(body map[string]any) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	return json.Marshal(body)
}

func parseAPIError(resp *http.Response, body []byte) *APIError {
	e := &APIError{Status: resp.StatusCode, Body: string(body), RequestID: resp.Header.Get("x-grok-req-id"), RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
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

func parseRetryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil && when.After(time.Now()) {
		return time.Until(when)
	}
	return 0
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
