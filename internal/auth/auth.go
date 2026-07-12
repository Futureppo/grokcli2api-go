package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrNoAuth = errors.New("no usable credentials in auths directory")

var (
	ErrInvalidCredentialJSON = errors.New("invalid credential JSON")
	ErrInvalidCredential     = errors.New("invalid credential")
)

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

type RefreshError struct {
	Permanent bool
	Status    int
	Code      string
}

func (e *RefreshError) Error() string {
	if e.Code != "" {
		return "OAuth refresh failed: " + e.Code
	}
	return fmt.Sprintf("OAuth refresh failed with status %d", e.Status)
}

type credential struct {
	Path            string
	Raw             map[string]any
	AccessToken     string
	RefreshToken    string
	TokenURL        string
	ClientID        string
	Subject         string
	Surface         string
	ExpiresAt       time.Time
	ExpiresIn       time.Duration
	Models          []string
	ModelsUpdatedAt time.Time
}

func loadCredential(path, surface string) (*credential, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCredential(b, path, surface)
}

func parseCredential(b []byte, path, surface string) (*credential, error) {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidCredentialJSON, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%w: credential must be a JSON object", ErrInvalidCredential)
	}
	node := credentialNode(raw)
	access := firstString(node, "access_token", "AccessToken", "key", "session_token", "SessionToken")
	refresh := firstString(node, "refresh_token", "RefreshToken")
	if access == "" && refresh == "" {
		return nil, fmt.Errorf("%w: credential has neither access_token nor refresh_token", ErrInvalidCredential)
	}
	accessClaims := jwtClaims(access)
	idClaims := jwtClaims(firstString(node, "id_token", "IDToken"))
	subject := firstString(node, "sub", "user_id", "userId", "UserId")
	if subject == "" {
		subject = claimString(accessClaims, "sub")
	}
	if subject == "" {
		subject = claimString(idClaims, "sub")
	}
	if subject == "" {
		return nil, fmt.Errorf("%w: credential has no stable subject", ErrInvalidCredential)
	}
	clientID := firstString(node, "client_id", "clientId")
	if clientID == "" {
		clientID = claimString(accessClaims, "client_id")
	}
	if clientID == "" {
		clientID = claimAudience(idClaims)
	}
	expiresIn := time.Duration(number(node["expires_in"])) * time.Second
	expiresAt := firstTime(node, "expired", "expires_at", "ExpiresAt", "expiry", "expiration")
	if expiresAt.IsZero() {
		expiresAt = unixClaimTime(accessClaims, "exp")
	}
	if expiresAt.IsZero() && expiresIn > 0 {
		if refreshed := firstTime(node, "last_refresh"); !refreshed.IsZero() {
			expiresAt = refreshed.Add(expiresIn)
		}
	}
	return &credential{
		Path: path, Raw: raw, AccessToken: access, RefreshToken: refresh,
		TokenURL: firstNonEmpty(firstString(node, "token_endpoint"), "https://auth.x.ai/oauth2/token"),
		ClientID: clientID, Subject: subject, Surface: defaultSurface(surface),
		ExpiresAt: expiresAt, ExpiresIn: expiresIn,
		Models: stringSlice(node["models"]), ModelsUpdatedAt: firstTime(node, "models_updated_at"),
	}, nil
}

func credentialNode(raw map[string]any) map[string]any {
	if firstString(raw, "access_token", "refresh_token", "key") != "" {
		return raw
	}
	if node, ok := raw["tokens"].(map[string]any); ok {
		return node
	}
	for _, value := range raw {
		if node, ok := value.(map[string]any); ok {
			if firstString(node, "access_token", "refresh_token", "key") != "" {
				return node
			}
		}
	}
	return raw
}

func (c *credential) session() Session {
	var expires *float64
	if !c.ExpiresAt.IsZero() {
		v := float64(c.ExpiresAt.UnixNano()) / 1e9
		expires = &v
	}
	return Session{Token: c.AccessToken, Surface: c.Surface, UserID: c.Subject, ObtainedAt: float64(time.Now().UnixNano()) / 1e9, ExpiresAt: expires}
}

func (c *credential) needsRefresh(now time.Time, jitter time.Duration) bool {
	if c.AccessToken == "" {
		return true
	}
	if c.ExpiresAt.IsZero() {
		return false
	}
	return !now.Add(2*time.Minute + jitter).Before(c.ExpiresAt)
}

func (c *credential) usable(now time.Time) bool {
	if c.AccessToken == "" {
		return false
	}
	if c.ExpiresAt.IsZero() {
		return true
	}
	return now.Add(time.Minute).Before(c.ExpiresAt)
}

func (c *credential) refresh(ctx context.Context, client *http.Client) (*credential, error) {
	if c.RefreshToken == "" || c.ClientID == "" {
		return nil, &RefreshError{Permanent: true, Code: "missing_refresh_metadata"}
	}
	u, err := url.Parse(c.TokenURL)
	if err != nil || !allowedTokenEndpoint(u) {
		return nil, &RefreshError{Permanent: true, Code: "invalid_token_endpoint"}
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
		"client_id":     {c.ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	_ = json.Unmarshal(b, &payload)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := firstString(payload, "error", "code")
		return nil, &RefreshError{Permanent: resp.StatusCode == 400 || resp.StatusCode == 401, Status: resp.StatusCode, Code: code}
	}
	access := firstString(payload, "access_token")
	if access == "" {
		return nil, &RefreshError{Permanent: true, Status: resp.StatusCode, Code: "missing_access_token"}
	}
	next := *c
	next.Raw = cloneMap(c.Raw)
	node := credentialNode(next.Raw)
	node["access_token"] = access
	next.AccessToken = access
	if refresh := firstString(payload, "refresh_token"); refresh != "" {
		node["refresh_token"] = refresh
		next.RefreshToken = refresh
	}
	if idToken := firstString(payload, "id_token"); idToken != "" {
		node["id_token"] = idToken
	}
	expiresSeconds := number(payload["expires_in"])
	if expiresSeconds <= 0 {
		expiresSeconds = int64(c.ExpiresIn / time.Second)
	}
	if expiresSeconds <= 0 {
		expiresSeconds = 21600
	}
	now := time.Now().UTC()
	next.ExpiresIn = time.Duration(expiresSeconds) * time.Second
	next.ExpiresAt = now.Add(next.ExpiresIn)
	node["expires_in"] = expiresSeconds
	node["last_refresh"] = now.Format(time.RFC3339Nano)
	node["expired"] = next.ExpiresAt.Format(time.RFC3339Nano)
	if err := writeCredentialAtomic(c.Path, next.Raw); err != nil {
		return nil, err
	}
	return &next, nil
}

func writeCredentialAtomic(path string, raw map[string]any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return writeCredentialAtomicMode(path, raw, info.Mode().Perm())
}

func writeCredentialAtomicMode(path string, raw map[string]any, mode os.FileMode) error {
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".grok-auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func allowedTokenEndpoint(u *url.URL) bool {
	if u == nil || u.Hostname() == "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func accountID(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:12])
}

func deterministicJitter(id string) time.Duration {
	sum := sha256.Sum256([]byte(id))
	seconds := int64(sum[0])<<8 | int64(sum[1])
	return time.Duration(seconds%300) * time.Second
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randomUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}

func unixClaimTime(claims map[string]any, key string) time.Time {
	if claims == nil {
		return time.Time{}
	}
	seconds := number(claims[key])
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func claimString(claims map[string]any, key string) string {
	if claims == nil {
		return ""
	}
	v, _ := claims[key].(string)
	return v
}

func claimAudience(claims map[string]any) string {
	if claims == nil {
		return ""
	}
	switch v := claims["aud"].(type) {
	case string:
		return v
	case []any:
		if len(v) > 0 {
			s, _ := v[0].(string)
			return s
		}
	}
	return ""
}

func firstTime(node map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		value, ok := node[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case string:
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				return t
			}
			if seconds, err := strconv.ParseInt(v, 10, 64); err == nil && seconds > 0 {
				return time.Unix(seconds, 0)
			}
		case float64:
			if v > 0 {
				return time.Unix(int64(v), 0)
			}
		case json.Number:
			if seconds, err := v.Int64(); err == nil && seconds > 0 {
				return time.Unix(seconds, 0)
			}
		}
	}
	return time.Time{}
}

func firstString(node map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := node[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func number(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func stringSlice(value any) []string {
	seen := map[string]struct{}{}
	var out []string
	switch values := value.(type) {
	case []any:
		for _, item := range values {
			if model, ok := item.(string); ok && strings.TrimSpace(model) != "" {
				model = strings.TrimSpace(model)
				if _, duplicate := seen[model]; !duplicate {
					seen[model] = struct{}{}
					out = append(out, model)
				}
			}
		}
	case []string:
		for _, model := range values {
			model = strings.TrimSpace(model)
			if model != "" {
				if _, duplicate := seen[model]; !duplicate {
					seen[model] = struct{}{}
					out = append(out, model)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func cloneMap(raw map[string]any) map[string]any {
	b, _ := json.Marshal(raw)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func defaultSurface(v string) string {
	if v == "" {
		return "tui"
	}
	return v
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
