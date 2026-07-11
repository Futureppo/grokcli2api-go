package grok

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func NewID() string { return randomHex(16) }

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func traceparent() string { return "00-" + randomHex(16) + "-" + randomHex(8) + "-01" }

func platform() string {
	if runtime.GOOS == "windows" {
		return "windows"
	}
	return runtime.GOOS
}

func defaultUserAgent(cfg config.Config) string {
	return fmt.Sprintf("grok-pager/%s %s/%s (%s; %s)", cfg.ClientVersion, cfg.ClientIdentifier, cfg.ClientVersion, platform(), runtime.GOARCH)
}

func chatUserAgent(cfg config.Config) string {
	return fmt.Sprintf("%s/%s (%s; %s)", cfg.ClientIdentifier, cfg.ClientVersion, platform(), runtime.GOARCH)
}

func BuildHeaders(cfg config.Config, session auth.Session, agentID, sessionID, convID, model string, trace bool) http.Header {
	reqID := NewID()
	h := make(http.Header)
	h.Set("x-grok-client-version", cfg.ClientVersion)
	h.Set("x-grok-client-identifier", cfg.ClientIdentifier)
	h.Set("x-grok-client-surface", cfg.ClientSurface)
	h.Set("x-grok-client-name", cfg.ClientName)
	h.Set("x-xai-token-auth", cfg.TokenAuth)
	h.Set("x-grok-agent-id", agentID)
	h.Set("x-grok-session-id", sessionID)
	h.Set("x-grok-conv-id", convID)
	h.Set("x-grok-req-id", reqID)
	h.Set("x-grok-conversation-id", convID)
	h.Set("x-grok-session-id-legacy", sessionID)
	h.Set("x-grok-request-id", reqID)
	if model != "" {
		h.Set("x-grok-model-override", model)
	}
	if session.UserID != "" {
		h.Set("x-userid", session.UserID)
	}
	if trace {
		h.Set("traceparent", traceparent())
		h.Set("tracestate", "")
	}
	h.Set("Authorization", "Bearer "+session.Token)
	h.Set("Accept", "application/json")
	h.Set("Accept-Encoding", "gzip")
	h.Set("User-Agent", defaultUserAgent(cfg))
	return h
}

func NewAgentID() string   { return NewID() }
func NewSessionID() string { return newUUID() }
