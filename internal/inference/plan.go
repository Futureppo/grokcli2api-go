// Package inference owns the protocol-neutral request plan used by the HTTP
// handlers.  A plan is deliberately account independent: an account may
// advertise a different wire model or API backend, so callers must render the
// plan again after every account selection (including retries).
package inference

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

// Protocol is the public protocol presented to the downstream client.
type Protocol string

const (
	ProtocolChatCompletions Protocol = "chat_completions"
	ProtocolResponses       Protocol = "responses"
	ProtocolMessages        Protocol = "messages"
)

// PlanOptions describes request-origin details that cannot be inferred from
// the JSON body. NativeCLI is intentionally meaningful only for Responses:
// regular OpenAI callers default store to true, while Grok CLI callers default
// it to false.
type PlanOptions struct {
	NativeCLI bool
	// Tenant is the already-derived, non-secret continuity namespace. Empty
	// uses the documented shared public tenant.
	Tenant string
}

// ResponseAdapter tells the server which response translation is required.
// Equal protocols/backends are pass-through apart from public-envelope
// sanitization. Keeping this as data avoids coupling the retry transport to a
// particular stream translator.
type ResponseAdapter struct {
	ClientProtocol  Protocol
	UpstreamBackend modelcatalog.Backend
	NativeCLI       bool
	// ToolAliases is request-local and is rebuilt on every account attempt.
	// Keys are wire names; values describe the downstream identity to restore
	// in non-stream and streaming responses.
	ToolAliases map[string]ToolAlias
}

type ToolAlias struct {
	Kind      string
	Name      string
	Namespace string
	Execution string
}

// StateHandle identifies opaque upstream continuation state supplied by the
// downstream caller. Values are kept only in the immutable in-memory request
// plan; persisted continuity records hash the complete tenant-scoped key.
type StateHandle struct {
	Kind  StateHandleKind
	Value string
}

type StateHandleKind string

const (
	StatePreviousResponse StateHandleKind = "previous_response"
	StateOpaqueToken      StateHandleKind = "opaque_token"
)

func (a ResponseAdapter) RestoreTool(wireName string) (ToolAlias, bool) {
	alias, ok := a.ToolAliases[wireName]
	return alias, ok
}

// RenderedAttempt is a fresh wire request for one selected account. Body never
// aliases the body supplied to NewRequestPlan or a previous attempt.
type RenderedAttempt struct {
	Backend         modelcatalog.Backend
	Path            string
	Body            map[string]any
	Stream          bool
	Adapter         ResponseAdapter
	ReasoningEffort string
	PreservesState  bool
	DroppedState    bool
}

var ErrNoRepresentableInput = errors.New("request has no representable input after cleaning")

// RequestError represents a public request error. Param uses the public JSON
// path, never an upstream path.
type RequestError struct {
	Param   string
	Message string
	Cause   error
}

func (e *RequestError) Error() string { return e.Message }
func (e *RequestError) Unwrap() error { return e.Cause }

func requestError(param, message string) error {
	return &RequestError{Param: param, Message: message}
}

func noInput(protocol Protocol) error {
	return &RequestError{
		Param:   inputParam(protocol),
		Message: "request has no representable input after cleaning",
		Cause:   ErrNoRepresentableInput,
	}
}

func inputParam(protocol Protocol) string {
	if protocol == ProtocolResponses {
		return "input"
	}
	return "messages"
}

// RequestPlan contains an immutable deep copy of a validated public request.
// The fields stay private so a handler cannot accidentally make retries depend
// on mutations performed by a renderer or compatibility adapter.
type RequestPlan struct {
	protocol       Protocol
	body           map[string]any
	model          string
	stream         bool
	nativeCLI      bool
	tenant         string
	effort         string
	effortSupplied bool
	hasState       bool
	stateHandles   []StateHandle
}

func (p *RequestPlan) Protocol() Protocol { return p.protocol }
func (p *RequestPlan) Model() string      { return p.model }
func (p *RequestPlan) Streaming() bool    { return p.stream }
func (p *RequestPlan) Tenant() string     { return p.tenant }
func (p *RequestPlan) HasState() bool     { return p.hasState }

// StateHandles returns a detached list of all syntactically usable
// continuation handles in the public request. Whether a handle reaches the
// wire remains descriptor-specific and must be determined from Render.
func (p *RequestPlan) StateHandles() []StateHandle {
	if p == nil || len(p.stateHandles) == 0 {
		return nil
	}
	return append([]StateHandle(nil), p.stateHandles...)
}

// NewRequestPlan validates only public required structure and fields that are
// intentionally retained. Unsupported fields and content are left for the
// account-specific renderer to discard silently.
func NewRequestPlan(protocol Protocol, body map[string]any, options PlanOptions) (*RequestPlan, error) {
	if body == nil {
		return nil, requestError("", "request body must be a JSON object")
	}
	switch protocol {
	case ProtocolChatCompletions, ProtocolResponses, ProtocolMessages:
	default:
		return nil, requestError("", "unsupported public protocol")
	}

	model, ok := body["model"].(string)
	model = strings.TrimSpace(model)
	if !ok || model == "" {
		return nil, requestError("model", "model is required and must be a non-empty string")
	}
	stream := false
	if raw, exists := body["stream"]; exists {
		var valid bool
		stream, valid = raw.(bool)
		if !valid {
			return nil, requestError("stream", "stream must be a boolean")
		}
	}

	switch protocol {
	case ProtocolChatCompletions:
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) == 0 {
			return nil, requestError("messages", "messages is required and must be a non-empty array")
		}
	case ProtocolResponses:
		raw, exists := body["input"]
		if !exists {
			raw, exists = body["messages"]
		}
		if !exists || raw == nil {
			return nil, requestError("input", "input is required and must be a string or array")
		}
		switch raw.(type) {
		case string, []any:
		default:
			return nil, requestError("input", "input is required and must be a string or array")
		}
	case ProtocolMessages:
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) == 0 {
			return nil, requestError("messages", "messages is required and must be a non-empty array")
		}
		max, ok := jsonNumber(body["max_tokens"])
		if !ok || !finite(max) || max <= 0 || math.Trunc(max) != max {
			return nil, requestError("max_tokens", "max_tokens is required and must be a positive integer")
		}
	}

	effort, supplied, err := requestEffort(protocol, body)
	if err != nil {
		return nil, err
	}
	tenant := strings.TrimSpace(options.Tenant)
	if tenant == "" {
		tenant = "public"
	}
	p := &RequestPlan{
		protocol: protocol, body: cloneMap(body), model: model, stream: stream,
		nativeCLI: options.NativeCLI, tenant: tenant, effort: effort, effortSupplied: supplied,
	}
	p.stateHandles = requestStateHandles(protocol, body)
	p.hasState = len(p.stateHandles) > 0
	return p, nil
}

func requestEffort(protocol Protocol, body map[string]any) (string, bool, error) {
	read := func(raw any, path string) (string, bool, error) {
		value, ok := raw.(string)
		if !ok {
			return "", false, requestError(path, path+" must be a string")
		}
		return value, true, nil
	}
	readObject := func(raw any, path string) (map[string]any, error) {
		object, ok := raw.(map[string]any)
		if !ok {
			return nil, requestError(path, path+" must be an object")
		}
		return object, nil
	}

	switch protocol {
	case ProtocolChatCompletions:
		if raw, exists := body["reasoning_effort"]; exists {
			return read(raw, "reasoning_effort")
		}
		if raw, exists := body["reasoning"]; exists {
			object, err := readObject(raw, "reasoning")
			if err != nil {
				return "", false, err
			}
			if rawEffort, exists := object["effort"]; exists {
				return read(rawEffort, "reasoning.effort")
			}
		}
	case ProtocolResponses:
		if raw, exists := body["reasoning"]; exists {
			object, err := readObject(raw, "reasoning")
			if err != nil {
				return "", false, err
			}
			if rawEffort, exists := object["effort"]; exists {
				return read(rawEffort, "reasoning.effort")
			}
		}
		if raw, exists := body["reasoning_effort"]; exists {
			return read(raw, "reasoning_effort")
		}
	case ProtocolMessages:
		if raw, exists := body["output_config"]; exists {
			object, err := readObject(raw, "output_config")
			if err != nil {
				return "", false, err
			}
			if rawEffort, exists := object["effort"]; exists {
				return read(rawEffort, "output_config.effort")
			}
		}
		// Accept the OpenAI alias as an input convenience. It is never emitted
		// on the Messages wire.
		if raw, exists := body["reasoning_effort"]; exists {
			return read(raw, "reasoning_effort")
		}
	}
	return "", false, nil
}

func requestStateHandles(protocol Protocol, body map[string]any) []StateHandle {
	seen := make(map[string]struct{})
	var handles []StateHandle
	add := func(kind StateHandleKind, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := string(kind) + "\x00" + value
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		handles = append(handles, StateHandle{Kind: kind, Value: value})
	}
	switch protocol {
	case ProtocolResponses:
		if id, ok := body["previous_response_id"].(string); ok {
			add(StatePreviousResponse, id)
		}
		input, _ := body["input"].([]any)
		for _, rawItem := range input {
			item, _ := rawItem.(map[string]any)
			if strings.EqualFold(trimmedString(item["type"]), "reasoning") {
				if encrypted, ok := item["encrypted_content"].(string); ok {
					add(StateOpaqueToken, encrypted)
				}
			}
		}
	case ProtocolMessages:
		messages, _ := body["messages"].([]any)
		for _, rawMessage := range messages {
			message, _ := rawMessage.(map[string]any)
			parts, _ := message["content"].([]any)
			for _, rawPart := range parts {
				part, _ := rawPart.(map[string]any)
				kind := trimmedString(part["type"])
				if strings.EqualFold(kind, "thinking") || strings.EqualFold(kind, "redacted_thinking") {
					if signature, ok := part["signature"].(string); ok {
						add(StateOpaqueToken, signature)
					}
				}
			}
		}
	}
	return handles
}

// Render creates the attempt for a selected account/model descriptor.
func (p *RequestPlan) Render(descriptor modelcatalog.ModelDescriptor) (*RenderedAttempt, error) {
	descriptor = descriptor.Normalize()
	backend := descriptor.Backend
	if backend == "" {
		backend = modelcatalog.BackendChatCompletions
	}
	wireModel := strings.TrimSpace(descriptor.WireModel)
	if wireModel == "" {
		wireModel = p.model
	}

	var (
		body        map[string]any
		preserves   bool
		toolAliases map[string]ToolAlias
		err         error
	)
	switch backend {
	case modelcatalog.BackendChatCompletions:
		body, err = p.renderChat(wireModel, descriptor)
	case modelcatalog.BackendResponses:
		body, preserves, toolAliases, err = p.renderResponses(wireModel, descriptor)
	case modelcatalog.BackendMessages:
		body, preserves, err = p.renderMessages(wireModel, descriptor)
	default:
		return nil, fmt.Errorf("unsupported model backend %q", backend)
	}
	if err != nil {
		return nil, err
	}
	if len(toolAliases) == 0 && p.protocol == ProtocolResponses {
		toolAliases = p.canonical().toolAliases
	}

	effort := ""
	if p.effortSupplied {
		effort = modelcatalog.NormalizeReasoningEffort(p.effort, descriptor)
		switch backend {
		case modelcatalog.BackendChatCompletions:
			body["reasoning_effort"] = effort
		case modelcatalog.BackendResponses:
			reasoning, _ := body["reasoning"].(map[string]any)
			reasoning = cloneMap(reasoning)
			reasoning["effort"] = effort
			body["reasoning"] = reasoning
			delete(body, "reasoning_effort")
		case modelcatalog.BackendMessages:
			wireEffort := effort
			if effort == "xhigh" {
				wireEffort = "max"
			}
			output, _ := body["output_config"].(map[string]any)
			output = cloneMap(output)
			output["effort"] = wireEffort
			body["output_config"] = output
			delete(body, "reasoning_effort")
			delete(body, "reasoning")
		}
	}

	return &RenderedAttempt{
		Backend: backend, Path: backendPath(backend), Body: cloneMap(body), Stream: p.stream,
		Adapter: ResponseAdapter{
			ClientProtocol: p.protocol, UpstreamBackend: backend, NativeCLI: p.nativeCLI,
			ToolAliases: cloneToolAliases(toolAliases),
		},
		ReasoningEffort: effort, PreservesState: preserves,
		DroppedState: p.hasState && !preserves,
	}, nil
}

func cloneToolAliases(input map[string]ToolAlias) map[string]ToolAlias {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]ToolAlias, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func backendPath(backend modelcatalog.Backend) string {
	switch backend {
	case modelcatalog.BackendResponses:
		return "responses"
	case modelcatalog.BackendMessages:
		return "messages"
	default:
		return "chat/completions"
	}
}

func jsonNumber(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	case uint:
		return float64(value), true
	case uint32:
		return float64(value), true
	case uint64:
		return float64(value), true
	default:
		return 0, false
	}
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = cloneValue(value)
	}
	return out
}

func cloneValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneMap(value)
	case []any:
		out := make([]any, len(value))
		for index := range value {
			out[index] = cloneValue(value[index])
		}
		return out
	default:
		return value
	}
}
