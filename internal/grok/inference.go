package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/inference"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

// InferenceOptions contains request state which is independent from a rendered
// wire body. PinnedAccount is used only for a verified, state-preserving hard
// affinity; conversions that drop state must leave it empty.
type InferenceOptions struct {
	Affinity      auth.Affinity
	PinnedAccount string
	// ExpectedBackend is populated from a verified hard-affinity binding. A
	// stateful request cannot be replayed against a different wire protocol if
	// an idle catalog refresh changes the bound account's descriptor.
	ExpectedBackend modelcatalog.Backend
	Identity        RequestIdentity
}

type InferenceResult struct {
	Payload   map[string]any
	Attempt   inference.RenderedAttempt
	AccountID string
	Identity  RequestIdentity
	Headers   http.Header
}

type InferenceStream struct {
	*EventStream
	Attempt   inference.RenderedAttempt
	AccountID string
	Identity  RequestIdentity
	Headers   http.Header
}

func (c *Client) DoInference(ctx context.Context, plan *inference.RequestPlan, options InferenceOptions) (*InferenceResult, error) {
	if plan == nil {
		return nil, errors.New("nil inference request plan")
	}
	identity := completeInferenceIdentity(options.Identity)
	preferred := preferredBackends(plan.Protocol())
	used := make(map[string]struct{})
	refreshed := make(map[string]bool)
	preferredID := options.PinnedAccount
	pinned := options.PinnedAccount != ""
	maxAttempts := c.cfg.RetryMaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	transportAttempts := 0
	var lastErr error

	for transportAttempts < maxAttempts {
		lease, descriptor, described, err := c.acquireInferenceLease(ctx, options.Affinity, plan.Model(), preferred, preferredID, pinned, used)
		preferredID = ""
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		accountID := lease.AccountID()
		generation := lease.Generation()
		used[accountID] = struct{}{}
		descriptor, described, available := c.refreshIdleInferenceCatalog(ctx, lease, plan.Model(), descriptor, described)
		if !available {
			lease.Release()
			lastErr = &auth.ModelUnavailableError{Model: plan.Model()}
			if pinned {
				return nil, stateUnavailableError()
			}
			continue
		}
		if !described {
			descriptor = provisionalDescriptor(plan.Protocol(), plan.Model())
		}
		if pinned && !backendMatches(options.ExpectedBackend, descriptor.Backend) {
			lease.Release()
			return nil, stateUnavailableError()
		}
		attempt, renderErr := plan.Render(descriptor)
		if renderErr != nil {
			lease.Release()
			lastErr = renderErr
			if pinned {
				return nil, renderErr
			}
			continue
		}
		if descriptor.MaxRetries != nil {
			limit := int(*descriptor.MaxRetries) + 1
			if limit < maxAttempts {
				maxAttempts = limit
			}
		}
		payload, marshalErr := json.Marshal(attempt.Body)
		if marshalErr != nil {
			lease.Release()
			return nil, marshalErr
		}
		attemptIdentity := identityWithLeaseDefaults(identity, lease)
		attemptIdentity.Model = wireModel(attempt.Body, descriptor, plan.Model())
		attemptIdentity.IdleTimeout = descriptor.InferenceIdleTimeout
		attemptIdentity.CompactionAtTokens = descriptor.CompactionAtTokens
		attemptIdentity.CompactionsRemaining = descriptor.CompactionsRemaining
		// Account switches retain logical identity while model routing metadata
		// is recomputed from the selected descriptor on every attempt.
		identity.AgentID = attemptIdentity.AgentID
		transportAttempts++
		resp, wrote, requestErr := c.doWithIdentity(ctx, lease, http.MethodPost, attempt.Path, payload, attemptIdentity, attempt.Backend != modelcatalog.BackendChatCompletions, false, nil)
		if requestErr != nil {
			lease.Release()
			lastErr = requestErr
			if ctx.Err() != nil || wrote || pinned || transportAttempts >= maxAttempts {
				return nil, requestErr
			}
			if err := c.backoff(ctx, transportAttempts); err != nil {
				return nil, err
			}
			continue
		}
		c.observeModelHeaders(accountID, plan.Model(), resp.Header)
		body, readErr := readResponseBody(resp, 16<<20)
		headers := resp.Header.Clone()
		resp.Body.Close()
		if readErr != nil {
			lease.Release()
			return nil, readErr
		}
		if resp.StatusCode >= 400 {
			apiErr := parseAPIError(resp, body)
			lease.Release()
			lastErr = apiErr
			if isPermanentAccountDenial(apiErr) {
				c.deletePermanentlyDeniedCredential(accountID)
				if pinned {
					return nil, stateUnavailableError()
				}
				continue
			}
			if isAuthError(apiErr) && !refreshed[accountID] {
				refreshed[accountID] = true
				if refreshErr := c.pool.RefreshIfUnchanged(ctx, accountID, generation); refreshErr == nil {
					// Preserve the historical one-time refresh retry even when the
					// configured transport retry budget is one.
					transportAttempts--
					delete(used, accountID)
					preferredID = accountID
					continue
				}
				c.pool.Disable(accountID, "authentication_failed")
				if pinned {
					return nil, stateUnavailableError()
				}
				continue
			}
			if isAuthError(apiErr) {
				c.pool.Disable(accountID, "authentication_failed")
				if pinned {
					return nil, stateUnavailableError()
				}
				continue
			}
			retryable := c.handleRetryable(accountID, plan.Model(), apiErr)
			if apiErr.ShouldRetry != nil && !*apiErr.ShouldRetry {
				retryable = false
			}
			if !retryable || pinned || transportAttempts >= maxAttempts {
				return nil, apiErr
			}
			if err := c.backoffForAPI(ctx, transportAttempts, apiErr); err != nil {
				return nil, err
			}
			continue
		}
		lease.Release()
		output := make(map[string]any)
		if len(body) > 0 && json.Unmarshal(body, &output) != nil {
			return nil, errors.New("upstream returned a non-JSON inference response")
		}
		bindingAffinity := options.Affinity
		if !attemptPreservesAffinity(*attempt, bindingAffinity) {
			c.pool.Unbind(options.Affinity, plan.Model())
			bindingAffinity = auth.Affinity{Tenant: bindingAffinity.Tenant}
		}
		c.pool.Bind(bindingAffinity, plan.Model(), accountID)
		if id := responseID(output); id != "" {
			c.pool.BindResponseIDForTenant(options.Affinity.Tenant, id, plan.Model(), accountID)
		}
		return &InferenceResult{
			Payload: output, Attempt: *attempt, AccountID: accountID,
			Identity: attemptIdentity, Headers: headers,
		}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, auth.ErrNoAuth
}

func (c *Client) OpenInference(ctx context.Context, plan *inference.RequestPlan, options InferenceOptions) (*InferenceStream, error) {
	if plan == nil {
		return nil, errors.New("nil inference request plan")
	}
	identity := completeInferenceIdentity(options.Identity)
	preferred := preferredBackends(plan.Protocol())
	used := make(map[string]struct{})
	refreshed := make(map[string]bool)
	preferredID := options.PinnedAccount
	pinned := options.PinnedAccount != ""
	maxAttempts := c.cfg.RetryMaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	transportAttempts := 0
	var lastErr error

	for transportAttempts < maxAttempts {
		lease, descriptor, described, err := c.acquireInferenceLease(ctx, options.Affinity, plan.Model(), preferred, preferredID, pinned, used)
		preferredID = ""
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		accountID := lease.AccountID()
		generation := lease.Generation()
		used[accountID] = struct{}{}
		descriptor, described, available := c.refreshIdleInferenceCatalog(ctx, lease, plan.Model(), descriptor, described)
		if !available {
			lease.Release()
			lastErr = &auth.ModelUnavailableError{Model: plan.Model()}
			if pinned {
				return nil, stateUnavailableError()
			}
			continue
		}
		if !described {
			descriptor = provisionalDescriptor(plan.Protocol(), plan.Model())
		}
		if pinned && !backendMatches(options.ExpectedBackend, descriptor.Backend) {
			lease.Release()
			return nil, stateUnavailableError()
		}
		attempt, renderErr := plan.Render(descriptor)
		if renderErr != nil {
			lease.Release()
			lastErr = renderErr
			if pinned {
				return nil, renderErr
			}
			continue
		}
		if descriptor.MaxRetries != nil {
			limit := int(*descriptor.MaxRetries) + 1
			if limit < maxAttempts {
				maxAttempts = limit
			}
		}
		payload, marshalErr := json.Marshal(attempt.Body)
		if marshalErr != nil {
			lease.Release()
			return nil, marshalErr
		}
		attemptIdentity := identityWithLeaseDefaults(identity, lease)
		attemptIdentity.Model = wireModel(attempt.Body, descriptor, plan.Model())
		attemptIdentity.IdleTimeout = descriptor.InferenceIdleTimeout
		attemptIdentity.CompactionAtTokens = descriptor.CompactionAtTokens
		attemptIdentity.CompactionsRemaining = descriptor.CompactionsRemaining
		identity.AgentID = attemptIdentity.AgentID
		transportAttempts++
		resp, wrote, requestErr := c.doWithIdentity(ctx, lease, http.MethodPost, attempt.Path, payload, attemptIdentity, attempt.Backend != modelcatalog.BackendChatCompletions, true, nil)
		if requestErr != nil {
			lease.Release()
			lastErr = requestErr
			if ctx.Err() != nil || wrote || pinned || transportAttempts >= maxAttempts {
				return nil, requestErr
			}
			if err := c.backoff(ctx, transportAttempts); err != nil {
				return nil, err
			}
			continue
		}
		c.observeModelHeaders(accountID, plan.Model(), resp.Header)
		if resp.StatusCode >= 400 {
			body, readErr := readResponseBody(resp, 4<<20)
			resp.Body.Close()
			lease.Release()
			if readErr != nil {
				return nil, readErr
			}
			apiErr := parseAPIError(resp, body)
			lastErr = apiErr
			if isPermanentAccountDenial(apiErr) {
				c.deletePermanentlyDeniedCredential(accountID)
				if pinned {
					return nil, stateUnavailableError()
				}
				continue
			}
			if isAuthError(apiErr) && !refreshed[accountID] {
				refreshed[accountID] = true
				if refreshErr := c.pool.RefreshIfUnchanged(ctx, accountID, generation); refreshErr == nil {
					transportAttempts--
					delete(used, accountID)
					preferredID = accountID
					continue
				}
				c.pool.Disable(accountID, "authentication_failed")
				if pinned {
					return nil, stateUnavailableError()
				}
				continue
			}
			if isAuthError(apiErr) {
				c.pool.Disable(accountID, "authentication_failed")
				if pinned {
					return nil, stateUnavailableError()
				}
				continue
			}
			retryable := c.handleRetryable(accountID, plan.Model(), apiErr)
			if apiErr.ShouldRetry != nil && !*apiErr.ShouldRetry {
				retryable = false
			}
			if !retryable || pinned || transportAttempts >= maxAttempts {
				return nil, apiErr
			}
			if err := c.backoffForAPI(ctx, transportAttempts, apiErr); err != nil {
				return nil, err
			}
			continue
		}
		if err := decodeResponseBody(resp); err != nil {
			resp.Body.Close()
			lease.Release()
			return nil, fmt.Errorf("decode upstream stream: %w", err)
		}
		if attemptIdentity.IdleTimeout > 0 {
			resp.Body = newIdleReadCloser(resp.Body, attemptIdentity.IdleTimeout)
		}
		bindingAffinity := options.Affinity
		if !attemptPreservesAffinity(*attempt, bindingAffinity) {
			c.pool.Unbind(options.Affinity, plan.Model())
			bindingAffinity = auth.Affinity{Tenant: bindingAffinity.Tenant}
		}
		c.pool.Bind(bindingAffinity, plan.Model(), accountID)
		headers := resp.Header.Clone()
		eventStream := &EventStream{
			response: resp, scanner: newSSEScanner(resp.Body), pool: c.pool, lease: lease,
			accountID: accountID, model: plan.Model(), quotaCooldown: c.cfg.QuotaCooldown,
			timing: RequestTimingFromContext(ctx),
		}
		return &InferenceStream{
			EventStream: eventStream, Attempt: *attempt, AccountID: accountID,
			Identity: attemptIdentity, Headers: headers,
		}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, auth.ErrNoAuth
}

const inferenceCatalogIdleRefresh = 10 * time.Minute

// refreshIdleInferenceCatalog mirrors the CLI's session-resume behavior. A
// session credential which has not been used for ten minutes refreshes
// /models-v2 before its request is rendered, so backend, wire slug and
// reasoning capabilities are taken from the newest account-local descriptor.
// Catalog refresh is best effort: an unavailable metadata endpoint must not
// make an otherwise usable inference credential fail. A successful refresh
// which removes the requested model does make this lease ineligible.
func (c *Client) refreshIdleInferenceCatalog(
	ctx context.Context,
	lease *auth.Lease,
	model string,
	descriptor modelcatalog.ModelDescriptor,
	described bool,
) (modelcatalog.ModelDescriptor, bool, bool) {
	now := time.Now()
	previous, seen := c.lastInference.Swap(lease.AccountID(), now)
	if lease.Session().IsAPIKey() {
		return descriptor, described, true
	}
	last, ok := previous.(time.Time)
	if !seen || !ok || last.IsZero() || now.Before(last) || now.Sub(last) < inferenceCatalogIdleRefresh {
		return descriptor, described, true
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := c.RefreshAccountModelsV2(refreshCtx, lease.AccountID())
	cancel()
	if err != nil {
		return descriptor, described, true
	}
	refreshed, ok := c.pool.AccountDescriptor(lease.AccountID(), model)
	if !ok {
		return modelcatalog.ModelDescriptor{}, false, false
	}
	return refreshed, true, true
}

func (c *Client) acquireInferenceLease(ctx context.Context, affinity auth.Affinity, model string, preferred []modelcatalog.Backend, preferredID string, requireDescriptor bool, exclude map[string]struct{}) (*auth.Lease, modelcatalog.ModelDescriptor, bool, error) {
	if preferredID != "" {
		lease, err := c.pool.AcquireAccountForModel(ctx, preferredID, model)
		if err != nil {
			if requireDescriptor {
				var unavailable *auth.UnavailableError
				if errors.As(err, &unavailable) && unavailable.Cooling {
					return nil, modelcatalog.ModelDescriptor{}, false, err
				}
				return nil, modelcatalog.ModelDescriptor{}, false, stateUnavailableError()
			}
			return nil, modelcatalog.ModelDescriptor{}, false, err
		}
		descriptor, described := lease.Descriptor()
		return lease, descriptor, described, nil
	}
	lease, err := c.pool.AcquireForBackends(ctx, affinity, model, preferred, exclude)
	if err != nil {
		return nil, modelcatalog.ModelDescriptor{}, false, err
	}
	descriptor, described := lease.Descriptor()
	return lease, descriptor, described, nil
}

func completeInferenceIdentity(identity RequestIdentity) RequestIdentity {
	if identity.RequestID == "" {
		identity.RequestID = NewID()
	}
	if identity.SessionID == "" {
		identity.SessionID = NewSessionID()
	}
	if identity.ConversationID == "" {
		identity.ConversationID = identity.SessionID
	}
	if identity.TurnIndex == nil {
		turn := uint64(0)
		identity.TurnIndex = &turn
	}
	return identity
}

func preferredBackends(protocol inference.Protocol) []modelcatalog.Backend {
	switch protocol {
	case inference.ProtocolResponses:
		return []modelcatalog.Backend{modelcatalog.BackendResponses, modelcatalog.BackendChatCompletions, modelcatalog.BackendMessages}
	case inference.ProtocolMessages:
		return []modelcatalog.Backend{modelcatalog.BackendMessages, modelcatalog.BackendResponses, modelcatalog.BackendChatCompletions}
	default:
		return []modelcatalog.Backend{modelcatalog.BackendChatCompletions, modelcatalog.BackendResponses, modelcatalog.BackendMessages}
	}
}

func provisionalDescriptor(_ inference.Protocol, model string) modelcatalog.ModelDescriptor {
	// A legacy []string catalog has no apiBackend metadata. Apply the same
	// contract as a structured descriptor with an omitted backend instead of
	// guessing from the downstream protocol.
	return modelcatalog.ModelDescriptor{
		ID: model, WireModel: model, Backend: modelcatalog.BackendChatCompletions, SupportedInAPI: true,
	}
}

func backendMatches(expected, actual modelcatalog.Backend) bool {
	if expected == "" {
		return true
	}
	expected = modelcatalog.Backend(strings.ToLower(strings.TrimSpace(string(expected))))
	switch expected {
	case modelcatalog.BackendChatCompletions, modelcatalog.BackendResponses, modelcatalog.BackendMessages:
	default:
		return false
	}
	return expected == modelcatalog.ModelDescriptor{Backend: actual}.Normalize().Backend
}

func wireModel(body map[string]any, descriptor modelcatalog.ModelDescriptor, fallback string) string {
	if model, ok := body["model"].(string); ok && strings.TrimSpace(model) != "" {
		return strings.TrimSpace(model)
	}
	if descriptor.WireModel != "" {
		return descriptor.WireModel
	}
	return fallback
}

func attemptPreservesAffinity(attempt inference.RenderedAttempt, affinity auth.Affinity) bool {
	if affinity.Mode != auth.AffinityHard {
		return true
	}
	// The explicit downstream session is carried in modern request headers and
	// survives every backend renderer. previous_response_id and thinking
	// signatures live in protocol bodies and follow the renderer's state flag.
	if strings.HasPrefix(affinity.Key, "session:") {
		return true
	}
	return attempt.PreservesState && !attempt.DroppedState
}

func stateUnavailableError() error {
	return &APIError{
		Status: http.StatusServiceUnavailable, UpstreamCode: "upstream_state_unavailable",
		UpstreamMessage: "upstream state is unavailable for the bound account",
	}
}
