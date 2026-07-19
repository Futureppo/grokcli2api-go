package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/anthropic"
	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/inference"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

type streamAtomKind uint8

const (
	streamText streamAtomKind = iota + 1
	streamReasoning
	streamToolStart
	streamToolDelta
	streamToolDone
	streamUsageAtom
	streamTerminal
	streamErrorAtom
)

type streamAtom struct {
	kind      streamAtomKind
	index     int
	id        string
	name      string
	toolKind  string
	namespace string
	execution string
	delta     string
	arguments any
	usage     canonicalUsage
	stop      string
	status    string
	message   string
}

type upstreamMessageBlock struct {
	kind            string
	initialInput    any
	hasInitialInput bool
	sawInputDelta   bool
}

type upstreamChatTool struct {
	id      string
	name    string
	started bool
	pending strings.Builder
}

// backendStreamAdapter decodes any of the three upstream stream dialects and
// re-encodes it for the public endpoint. Native dialects use a light pass
// through, while cross-protocol routes share this canonical event state.
type backendStreamAdapter struct {
	adapter inference.ResponseAdapter
	model   string
	id      string

	terminal bool
	success  bool
	started  bool
	sawTool  bool
	usage    canonicalUsage
	stop     string

	chatRoleSent bool
	chatToolArgs map[int]*strings.Builder

	responseCreated      bool
	responseMessage      bool
	responseText         bool
	responseTextDone     bool
	responseReason       bool
	responseReasonDone   bool
	responseMessageIndex int
	responseReasonIndex  int
	responseNextIndex    int
	responseTextValue    strings.Builder
	responseReasonValue  strings.Builder
	responseTools        map[int]canonicalToolCall
	responseArgs         map[int]*strings.Builder
	responseToolIndexes  map[int]int
	responseToolDone     map[int]bool
	nativeAliases        map[string]inference.ToolAlias
	nativeArgs           map[string]*strings.Builder

	messageStarted        bool
	messageBlocks         map[int]string
	upstreamChatTools     map[int]*upstreamChatTool
	upstreamMessageBlocks map[int]upstreamMessageBlock
	messageNext           int
	messageOptions        anthropic.ResponseOptions
	responsesToMessages   *anthropic.StreamTranslator
}

func newBackendStreamAdapter(adapter inference.ResponseAdapter, model string, messageOptions ...anthropic.ResponseOptions) *backendStreamAdapter {
	stream := &backendStreamAdapter{
		adapter: adapter, model: model, id: grok.NewID(),
		responseTools: make(map[int]canonicalToolCall), responseArgs: make(map[int]*strings.Builder),
		responseToolIndexes: make(map[int]int), responseToolDone: make(map[int]bool),
		responseMessageIndex: -1, responseReasonIndex: -1,
		nativeAliases: make(map[string]inference.ToolAlias), nativeArgs: make(map[string]*strings.Builder),
		chatToolArgs:  make(map[int]*strings.Builder),
		messageBlocks: make(map[int]string), upstreamChatTools: make(map[int]*upstreamChatTool),
		upstreamMessageBlocks: make(map[int]upstreamMessageBlock),
	}
	if len(messageOptions) > 0 {
		stream.messageOptions = messageOptions[0]
	}
	if adapter.ClientProtocol == inference.ProtocolMessages && adapter.UpstreamBackend == modelcatalog.BackendResponses {
		stream.responsesToMessages = anthropic.NewStreamTranslatorWithOptions(model, stream.messageOptions)
	}
	return stream
}

func (s *backendStreamAdapter) Terminal() bool { return s != nil && s.terminal }
func (s *backendStreamAdapter) Success() bool  { return s != nil && s.terminal && s.success }
func (s *backendStreamAdapter) ResponseID() string {
	if s == nil || s.adapter.ClientProtocol != inference.ProtocolResponses || s.id == "" {
		return ""
	}
	if strings.HasPrefix(s.id, "resp_") {
		return s.id
	}
	return "resp_" + s.id
}

func (s *backendStreamAdapter) Handle(event grok.SSEEvent) ([]grok.SSEEvent, error) {
	if s == nil || s.terminal {
		return nil, nil
	}
	if s.responsesToMessages != nil {
		return s.handleResponsesToMessages(event)
	}
	if s.nativePair() {
		return s.handleNative(event)
	}
	atoms, err := decodeStreamAtoms(s.adapter.UpstreamBackend, event, s.upstreamChatTools, s.upstreamMessageBlocks)
	if err != nil {
		return nil, err
	}
	var output []grok.SSEEvent
	for _, atom := range atoms {
		if atom.name != "" {
			if alias, ok := s.adapter.ToolAliases[atom.name]; ok {
				if alias.Name != "" {
					atom.name = alias.Name
				}
				atom.toolKind, atom.namespace, atom.execution = alias.Kind, alias.Namespace, alias.Execution
			}
		}
		if atom.kind == streamUsageAtom {
			s.usage = mergeUsage(s.usage, atom.usage)
		}
		if atom.kind == streamToolStart && atom.name != "" {
			s.sawTool = true
		}
		if atom.stop != "" {
			s.stop = atom.stop
		}
		encoded, encodeErr := s.encode(atom)
		if encodeErr != nil {
			return nil, encodeErr
		}
		output = append(output, encoded...)
	}
	return output, nil
}

func (s *backendStreamAdapter) handleResponsesToMessages(event grok.SSEEvent) ([]grok.SSEEvent, error) {
	if string(event.Data) == "[DONE]" || len(event.Data) == 0 {
		return nil, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		return nil, fmt.Errorf("decode Responses stream event: %w", err)
	}
	kind := event.Event
	if kind == "" {
		kind = stringAt(payload, "type")
	}
	if response, ok := payload["response"].(map[string]any); ok {
		if id := stringAt(response, "id"); id != "" {
			s.id = id
		}
	}
	if kind == "response.failed" || kind == "error" {
		message := "upstream response failed"
		if raw := payload["error"]; raw != nil {
			message = errorMessage(raw)
		} else if response, ok := payload["response"].(map[string]any); ok && response["error"] != nil {
			message = errorMessage(response["error"])
		}
		s.terminal, s.success = true, false
		return s.encodeError(message, "api_error"), nil
	}
	translated, err := s.responsesToMessages.Handle(event)
	if err != nil {
		return nil, err
	}
	out := make([]grok.SSEEvent, 0, len(translated))
	for _, translatedEvent := range translated {
		out = append(out, namedEvent(translatedEvent.Name, translatedEvent.Data))
	}
	if kind == "response.completed" || kind == "response.incomplete" {
		s.terminal, s.success = true, true
	}
	return out, nil
}

func (s *backendStreamAdapter) Finish() []grok.SSEEvent {
	if s == nil || s.terminal {
		return nil
	}
	// A Chat finish_reason is a real terminal. The trailing [DONE] sentinel is
	// conventional and usage may follow the finish chunk, so Handle waits for
	// it; if the transport closes after finish_reason, finish successfully.
	if s.adapter.UpstreamBackend == modelcatalog.BackendChatCompletions && s.stop != "" {
		if s.nativePair() {
			s.terminal, s.success = true, true
			return s.chatDoneEvent()
		}
		encoded, _ := s.encode(streamAtom{kind: streamTerminal, status: "completed", stop: s.stop})
		if s.adapter.ClientProtocol == inference.ProtocolChatCompletions && s.Success() {
			encoded = append(encoded, s.chatDoneEvent()...)
		}
		return encoded
	}
	s.terminal = true
	s.success = false
	return s.encodeError("upstream stream ended before a terminal event", "upstream_stream_incomplete")
}

func (s *backendStreamAdapter) chatDoneEvent() []grok.SSEEvent {
	return []grok.SSEEvent{{Data: []byte("[DONE]")}}
}

func (s *backendStreamAdapter) nativePair() bool {
	return s.adapter.ClientProtocol == inference.ProtocolChatCompletions && s.adapter.UpstreamBackend == modelcatalog.BackendChatCompletions ||
		s.adapter.ClientProtocol == inference.ProtocolResponses && s.adapter.UpstreamBackend == modelcatalog.BackendResponses ||
		s.adapter.ClientProtocol == inference.ProtocolMessages && s.adapter.UpstreamBackend == modelcatalog.BackendMessages
}

func (s *backendStreamAdapter) handleNative(event grok.SSEEvent) ([]grok.SSEEvent, error) {
	switch s.adapter.ClientProtocol {
	case inference.ProtocolChatCompletions:
		if string(event.Data) == "[DONE]" {
			// [DONE] is the Chat wire terminal. Some compatible deployments
			// omit a separate finish_reason chunk; the explicit sentinel is
			// still a successful terminal (plain EOF is not).
			if s.stop == "" {
				s.stop = "stop"
			}
			s.terminal, s.success = true, true
			return nil, nil
		}
		var chunk map[string]any
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			return nil, fmt.Errorf("decode chat stream event: %w", err)
		}
		chunk = normalizeStreamChatChunk(chunk, s.model)
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if delta != nil && !s.chatRoleSent {
				if stringAt(delta, "role") == "" {
					delta["role"] = "assistant"
				}
				s.chatRoleSent = true
			}
			if finish := stringAt(choice, "finish_reason"); finish != "" {
				s.stop = finish
			}
		}
		if rawError, exists := chunk["error"]; exists {
			s.terminal, s.success = true, false
			return s.encodeError(errorMessage(rawError), "upstream_error"), nil
		}
		if usage, ok := chunk["usage"].(map[string]any); ok {
			s.usage = canonicalUsage{Input: intAt(usage, "prompt_tokens"), Output: intAt(usage, "completion_tokens")}
		}
		return []grok.SSEEvent{{Data: mustJSON(chunk)}}, nil
	case inference.ProtocolResponses:
		if string(event.Data) == "[DONE]" {
			return nil, nil
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("decode Responses stream event: %w", err)
		}
		kind := firstNonEmptyString(payload, "type")
		if kind == "" {
			kind = event.Event
		}
		if !knownNativeResponsesEvent(kind) {
			if !s.adapter.NativeCLI {
				return nil, nil
			}
			event.Event = kind
			event.Data = mustJSON(payload)
			return []grok.SSEEvent{event}, nil
		}
		if rewritten, handled := s.rewriteNativeResponsesToolEvent(event, kind, payload); handled {
			return rewritten, nil
		}
		payload = restoreResponsesPayloadAliases(payload, s.adapter.ToolAliases)
		if response, ok := payload["response"].(map[string]any); ok {
			response["model"] = s.model
			if id := stringAt(response, "id"); id != "" {
				s.id = id
			}
		}
		switch kind {
		case "response.completed", "response.incomplete":
			s.terminal, s.success = true, true
		case "response.failed", "error":
			s.terminal, s.success = true, false
		}
		event.Event = kind
		event.Data = mustJSON(payload)
		return []grok.SSEEvent{event}, nil
	case inference.ProtocolMessages:
		var payload map[string]any
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("decode Messages stream event: %w", err)
		}
		kind := event.Event
		if kind == "" {
			kind = stringAt(payload, "type")
		}
		if !knownNativeMessagesEvent(kind) {
			if !s.adapter.NativeCLI {
				return nil, nil
			}
			event.Event = kind
			event.Data = mustJSON(payload)
			return []grok.SSEEvent{event}, nil
		}
		if message, ok := payload["message"].(map[string]any); ok {
			message["model"] = s.model
		}
		switch kind {
		case "message_stop":
			s.terminal, s.success = true, true
		case "error":
			s.terminal, s.success = true, false
		}
		event.Event = kind
		event.Data = mustJSON(payload)
		return []grok.SSEEvent{event}, nil
	}
	return nil, nil
}

func knownNativeResponsesEvent(kind string) bool {
	switch kind {
	case "error", "response.created", "response.in_progress", "response.queued",
		"response.completed", "response.incomplete", "response.failed",
		"response.doom_loop_check",
		"response.output_item.added", "response.output_item.done",
		"response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done",
		"response.refusal.delta", "response.refusal.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return true
	}
	for _, prefix := range []string{
		"response.web_search_call.", "response.x_search_call.", "response.file_search_call.",
		"response.image_generation_call.", "response.code_interpreter_call.",
		"response.computer_call.", "response.mcp_call.", "response.mcp_list_tools.",
		"response.audio.", "response.output_audio.",
	} {
		if strings.HasPrefix(kind, prefix) {
			return true
		}
	}
	return false
}

func knownNativeMessagesEvent(kind string) bool {
	switch kind {
	case "message_start", "content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop", "ping", "error":
		return true
	default:
		return false
	}
}

// rewriteNativeResponsesToolEvent keeps the request-local aliases invisible on
// the public Responses stream. Custom and tool-search calls are represented as
// ordinary functions on the upstream wire, so their argument fragments cannot
// be forwarded until the complete JSON value can be decoded safely.
func (s *backendStreamAdapter) rewriteNativeResponsesToolEvent(event grok.SSEEvent, kind string, payload map[string]any) ([]grok.SSEEvent, bool) {
	switch kind {
	case "response.output_item.added":
		item, _ := payload["item"].(map[string]any)
		s.rememberNativeResponseAlias(payload, item)
	case "response.function_call_arguments.delta":
		alias, arguments, ok := s.nativeResponseAlias(payload, nil)
		if !ok || (alias.Kind != "custom" && alias.Kind != "tool_search") {
			return nil, false
		}
		arguments.WriteString(stringAt(payload, "delta"))
		return nil, true
	case "response.function_call_arguments.done":
		alias, arguments, ok := s.nativeResponseAlias(payload, nil)
		if !ok || (alias.Kind != "custom" && alias.Kind != "tool_search") {
			return nil, false
		}
		if alias.Kind == "tool_search" {
			return nil, true
		}
		raw := payload["arguments"]
		if text, ok := raw.(string); !ok || text == "" {
			raw = arguments.String()
		}
		input := aliasedCustomInput(raw)
		delta := cloneJSONMap(payload)
		delta["type"] = "response.custom_tool_call_input.delta"
		delta["delta"] = input
		delete(delta, "arguments")
		done := cloneJSONMap(payload)
		done["type"] = "response.custom_tool_call_input.done"
		done["input"] = input
		delete(done, "arguments")
		delete(done, "delta")
		first := event
		first.Event = "response.custom_tool_call_input.delta"
		first.Data = mustJSON(delta)
		return []grok.SSEEvent{
			first,
			{Event: "response.custom_tool_call_input.done", Data: mustJSON(done)},
		}, true
	case "response.output_item.done":
		item, _ := payload["item"].(map[string]any)
		if _, arguments, ok := s.nativeResponseAlias(payload, item); ok && arguments.Len() > 0 {
			if raw, exists := item["arguments"]; !exists || raw == "" {
				item["arguments"] = arguments.String()
			}
		}
	}
	return nil, false
}

func (s *backendStreamAdapter) rememberNativeResponseAlias(payload, item map[string]any) {
	if item == nil || stringAt(item, "type") != "function_call" {
		return
	}
	alias, ok := s.adapter.ToolAliases[stringAt(item, "name")]
	if !ok {
		return
	}
	keys := nativeResponseCallKeys(payload, item)
	if len(keys) == 0 {
		return
	}
	arguments := &strings.Builder{}
	for _, key := range keys {
		s.nativeAliases[key] = alias
		s.nativeArgs[key] = arguments
	}
}

func (s *backendStreamAdapter) nativeResponseAlias(payload, item map[string]any) (inference.ToolAlias, *strings.Builder, bool) {
	for _, key := range nativeResponseCallKeys(payload, item) {
		alias, ok := s.nativeAliases[key]
		if !ok {
			continue
		}
		arguments := s.nativeArgs[key]
		if arguments == nil {
			arguments = &strings.Builder{}
			s.nativeArgs[key] = arguments
		}
		return alias, arguments, true
	}
	if item != nil {
		alias, ok := s.adapter.ToolAliases[stringAt(item, "name")]
		if ok {
			arguments := &strings.Builder{}
			for _, key := range nativeResponseCallKeys(payload, item) {
				s.nativeAliases[key] = alias
				s.nativeArgs[key] = arguments
			}
			return alias, arguments, true
		}
	}
	return inference.ToolAlias{}, nil, false
}

func nativeResponseCallKeys(payload, item map[string]any) []string {
	seen := make(map[string]struct{})
	keys := make([]string, 0, 5)
	add := func(prefix, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := prefix + value
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for _, values := range []map[string]any{payload, item} {
		if values == nil {
			continue
		}
		add("item:", firstNonEmptyString(values, "item_id", "id"))
		add("call:", stringAt(values, "call_id"))
		if _, exists := values["output_index"]; exists {
			add("index:", fmt.Sprint(intAt(values, "output_index")))
		}
	}
	return keys
}

func decodeStreamAtoms(backend modelcatalog.Backend, event grok.SSEEvent, chatTools map[int]*upstreamChatTool, messageBlocks map[int]upstreamMessageBlock) ([]streamAtom, error) {
	switch backend {
	case modelcatalog.BackendResponses:
		return decodeResponsesAtoms(event)
	case modelcatalog.BackendMessages:
		return decodeMessagesAtoms(event, messageBlocks)
	default:
		return decodeChatAtoms(event, chatTools)
	}
}

func decodeChatAtoms(event grok.SSEEvent, tools map[int]*upstreamChatTool) ([]streamAtom, error) {
	if string(event.Data) == "[DONE]" {
		return []streamAtom{{kind: streamTerminal, status: "completed"}}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		return nil, fmt.Errorf("decode chat stream event: %w", err)
	}
	if rawError, exists := payload["error"]; exists {
		return []streamAtom{{kind: streamErrorAtom, message: errorMessage(rawError)}}, nil
	}
	var atoms []streamAtom
	if usage, ok := payload["usage"].(map[string]any); ok {
		atoms = append(atoms, streamAtom{kind: streamUsageAtom, usage: canonicalUsage{
			Input: intAt(usage, "prompt_tokens", "input_tokens"), Output: intAt(usage, "completion_tokens", "output_tokens"),
		}})
	}
	choices, _ := payload["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if text := flattenText(delta["content"]); text != "" {
			atoms = append(atoms, streamAtom{kind: streamText, delta: text})
		}
		if reasoning := firstNonEmptyString(delta, "reasoning_content", "reasoning", "thinking"); reasoning != "" {
			atoms = append(atoms, streamAtom{kind: streamReasoning, delta: reasoning})
		}
		calls, _ := delta["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			index := int(intAt(call, "index"))
			function, _ := call["function"].(map[string]any)
			state := tools[index]
			if state == nil {
				state = &upstreamChatTool{}
				if tools != nil {
					tools[index] = state
				}
			}
			if id := firstNonEmptyString(call, "id", "call_id"); id != "" {
				state.id = id
			}
			if name := stringAt(function, "name"); name != "" {
				state.name = name
			}
			arguments := stringAt(function, "arguments")
			if !state.started {
				if arguments != "" {
					state.pending.WriteString(arguments)
				}
				if state.name != "" {
					state.started = true
					atoms = append(atoms, streamAtom{kind: streamToolStart, index: index, id: state.id, name: state.name})
					if state.pending.Len() > 0 {
						atoms = append(atoms, streamAtom{kind: streamToolDelta, index: index, delta: state.pending.String()})
						state.pending.Reset()
					}
				}
			} else if arguments != "" {
				atoms = append(atoms, streamAtom{kind: streamToolDelta, index: index, delta: arguments})
			}
		}
		if finish := stringAt(choice, "finish_reason"); finish != "" {
			// Chat streams may emit the usage-only chunk after the finish
			// choice. Remember the reason and wait for [DONE] before declaring
			// success so usage is not lost and an early EOF is not forged into
			// a successful terminal.
			atoms = append(atoms, streamAtom{kind: streamUsageAtom, stop: finish})
		}
	}
	return atoms, nil
}

func decodeResponsesAtoms(event grok.SSEEvent) ([]streamAtom, error) {
	if string(event.Data) == "[DONE]" {
		return nil, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		return nil, fmt.Errorf("decode Responses stream event: %w", err)
	}
	kind := firstNonEmptyString(payload, "type")
	if kind == "" {
		kind = event.Event
	}
	switch kind {
	case "response.output_text.delta":
		return []streamAtom{{kind: streamText, delta: stringAt(payload, "delta")}}, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return []streamAtom{{kind: streamReasoning, delta: stringAt(payload, "delta")}}, nil
	case "response.output_item.added":
		item, _ := payload["item"].(map[string]any)
		typeName := stringAt(item, "type")
		if typeName == "function_call" || typeName == "custom_tool_call" {
			return []streamAtom{{kind: streamToolStart, index: int(intAt(payload, "output_index")), id: firstNonEmptyString(item, "call_id", "id"), name: stringAt(item, "name")}}, nil
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return []streamAtom{{kind: streamToolDelta, index: int(intAt(payload, "output_index")), id: stringAt(payload, "item_id"), delta: stringAt(payload, "delta")}}, nil
	case "response.output_item.done":
		item, _ := payload["item"].(map[string]any)
		typeName := stringAt(item, "type")
		if typeName == "function_call" || typeName == "custom_tool_call" {
			return []streamAtom{{kind: streamToolDone, index: int(intAt(payload, "output_index")), id: firstNonEmptyString(item, "call_id", "id"), name: stringAt(item, "name"), arguments: parseArguments(first(item, "arguments", "input"))}}, nil
		}
	case "response.completed", "response.incomplete", "response.failed":
		response, _ := payload["response"].(map[string]any)
		status := strings.TrimPrefix(kind, "response.")
		stop := ""
		if incomplete, ok := response["incomplete_details"].(map[string]any); ok {
			stop = stringAt(incomplete, "reason")
		}
		usage, _ := response["usage"].(map[string]any)
		return []streamAtom{
			{kind: streamUsageAtom, usage: canonicalUsage{Input: intAt(usage, "input_tokens"), Output: intAt(usage, "output_tokens")}},
			{kind: streamTerminal, status: status, stop: stop},
		}, nil
	case "error":
		return []streamAtom{{kind: streamErrorAtom, message: errorMessage(payload["error"])}}, nil
	}
	return nil, nil
}

func decodeMessagesAtoms(event grok.SSEEvent, blocks map[int]upstreamMessageBlock) ([]streamAtom, error) {
	var payload map[string]any
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		return nil, fmt.Errorf("decode Messages stream event: %w", err)
	}
	kind := event.Event
	if kind == "" {
		kind = stringAt(payload, "type")
	}
	index := int(intAt(payload, "index"))
	switch kind {
	case "message_start":
		message, _ := payload["message"].(map[string]any)
		usage, _ := message["usage"].(map[string]any)
		return []streamAtom{{kind: streamUsageAtom, usage: canonicalUsage{Input: intAt(usage, "input_tokens"), Output: intAt(usage, "output_tokens")}}}, nil
	case "content_block_start":
		block, _ := payload["content_block"].(map[string]any)
		blockKind := stringAt(block, "type")
		if blocks != nil {
			state := upstreamMessageBlock{kind: blockKind}
			if input, exists := block["input"]; exists {
				state.initialInput = input
				state.hasInitialInput = true
			}
			blocks[index] = state
		}
		switch blockKind {
		case "text":
			return []streamAtom{{kind: streamText, index: index, delta: stringAt(block, "text")}}, nil
		case "thinking":
			return []streamAtom{{kind: streamReasoning, index: index, delta: firstNonEmptyString(block, "thinking", "text")}}, nil
		case "tool_use":
			return []streamAtom{{kind: streamToolStart, index: index, id: stringAt(block, "id"), name: stringAt(block, "name")}}, nil
		}
	case "content_block_delta":
		delta, _ := payload["delta"].(map[string]any)
		switch stringAt(delta, "type") {
		case "text_delta":
			return []streamAtom{{kind: streamText, index: index, delta: stringAt(delta, "text")}}, nil
		case "thinking_delta":
			return []streamAtom{{kind: streamReasoning, index: index, delta: firstNonEmptyString(delta, "thinking", "text")}}, nil
		case "input_json_delta":
			if blocks != nil {
				state := blocks[index]
				state.sawInputDelta = true
				blocks[index] = state
			}
			return []streamAtom{{kind: streamToolDelta, index: index, delta: stringAt(delta, "partial_json")}}, nil
		}
	case "content_block_stop":
		state := blocks[index]
		delete(blocks, index)
		if state.kind == "tool_use" {
			atom := streamAtom{kind: streamToolDone, index: index}
			// Anthropic normally starts tool_use with input:{} and streams the
			// actual JSON in later deltas. Some compatible backends instead put
			// the complete input on content_block_start and emit no deltas. Keep
			// that value only in the latter form so an empty placeholder is never
			// prepended to streamed JSON.
			if state.hasInitialInput && !state.sawInputDelta {
				atom.arguments = state.initialInput
			}
			return []streamAtom{atom}, nil
		}
		return nil, nil
	case "message_delta":
		delta, _ := payload["delta"].(map[string]any)
		usage, _ := payload["usage"].(map[string]any)
		return []streamAtom{{kind: streamUsageAtom, usage: canonicalUsage{Output: intAt(usage, "output_tokens")}, stop: stringAt(delta, "stop_reason")}}, nil
	case "message_stop":
		return []streamAtom{{kind: streamTerminal, status: "completed"}}, nil
	case "error":
		return []streamAtom{{kind: streamErrorAtom, message: errorMessage(payload["error"])}}, nil
	}
	return nil, nil
}

func (s *backendStreamAdapter) encode(atom streamAtom) ([]grok.SSEEvent, error) {
	switch s.adapter.ClientProtocol {
	case inference.ProtocolChatCompletions:
		return s.encodeChat(atom), nil
	case inference.ProtocolMessages:
		return s.encodeMessages(atom), nil
	default:
		return s.encodeResponses(atom), nil
	}
}

func (s *backendStreamAdapter) encodeChat(atom streamAtom) []grok.SSEEvent {
	if atom.kind == streamErrorAtom {
		s.terminal, s.success = true, false
		return s.encodeError(atom.message, "upstream_error")
	}
	if atom.kind == streamUsageAtom {
		return nil
	}
	if atom.kind == streamTerminal {
		if s.terminal {
			return nil
		}
		s.terminal = true
		s.success = atom.status == "completed" || atom.status == "incomplete"
		result := canonicalResult{StopReason: firstNonEmpty(atom.stop, s.stop)}
		if s.sawTool {
			result.Tools = []canonicalToolCall{{}}
		}
		finish := chatStopReason(result)
		if atom.status == "failed" {
			return s.encodeError("upstream response failed", "upstream_error")
		}
		return []grok.SSEEvent{{Data: mustJSON(map[string]any{
			"id": "chatcmpl-" + s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
			"usage":   chatUsage(s.usage),
		})}}
	}
	delta := map[string]any{}
	if !s.chatRoleSent {
		delta["role"] = "assistant"
		s.chatRoleSent = true
	}
	switch atom.kind {
	case streamText:
		delta["content"] = atom.delta
	case streamReasoning:
		delta["reasoning_content"] = atom.delta
	case streamToolStart, streamToolDelta, streamToolDone:
		function := map[string]any{}
		if atom.name != "" {
			function["name"] = atom.name
		}
		arguments := atom.delta
		switch atom.kind {
		case streamToolStart:
			if _, exists := s.chatToolArgs[atom.index]; !exists {
				s.chatToolArgs[atom.index] = &strings.Builder{}
			}
		case streamToolDelta:
			builder := s.chatToolArgs[atom.index]
			if builder == nil {
				builder = &strings.Builder{}
				s.chatToolArgs[atom.index] = builder
			}
			builder.WriteString(atom.delta)
		case streamToolDone:
			builder := s.chatToolArgs[atom.index]
			if atom.arguments != nil {
				complete := argumentsString(atom.arguments)
				if builder == nil || builder.Len() == 0 {
					arguments = complete
				} else if strings.HasPrefix(complete, builder.String()) {
					// Most Responses streams include both deltas and an authoritative
					// full value on output_item.done. Emit only a missing suffix; sending
					// the complete value again would corrupt Chat's concatenated JSON.
					arguments = strings.TrimPrefix(complete, builder.String())
				} else {
					arguments = ""
				}
			}
			delete(s.chatToolArgs, atom.index)
		}
		if arguments != "" {
			function["arguments"] = arguments
		}
		call := map[string]any{"index": atom.index, "type": "function", "function": function}
		if atom.id != "" {
			call["id"] = atom.id
		}
		delta["tool_calls"] = []any{call}
	default:
		return nil
	}
	return []grok.SSEEvent{{Data: mustJSON(map[string]any{
		"id": "chatcmpl-" + s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	})}}
}

func (s *backendStreamAdapter) encodeResponses(atom streamAtom) []grok.SSEEvent {
	if atom.kind == streamErrorAtom {
		s.terminal, s.success = true, false
		return s.encodeError(atom.message, "upstream_error")
	}
	var output []grok.SSEEvent
	if !s.responseCreated && atom.kind != streamUsageAtom {
		s.responseCreated = true
		response := s.responseObject("in_progress")
		output = append(output, namedEvent("response.created", map[string]any{"type": "response.created", "response": response}))
	}
	switch atom.kind {
	case streamText:
		if !s.responseMessage {
			s.responseMessage = true
			s.responseMessageIndex = s.nextResponseOutputIndex()
			output = append(output, namedEvent("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": s.responseMessageIndex,
				"item": map[string]any{"id": "msg_" + s.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
			}))
		}
		if !s.responseText {
			s.responseText = true
			output = append(output, namedEvent("response.content_part.added", map[string]any{
				"type": "response.content_part.added", "item_id": "msg_" + s.id, "output_index": s.responseMessageIndex, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			}))
		}
		s.responseTextValue.WriteString(atom.delta)
		output = append(output, namedEvent("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": "msg_" + s.id, "output_index": s.responseMessageIndex, "content_index": 0, "delta": atom.delta,
		}))
	case streamReasoning:
		if !s.responseReason {
			s.responseReason = true
			s.responseReasonIndex = s.nextResponseOutputIndex()
			output = append(output, namedEvent("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": s.responseReasonIndex,
				"item": map[string]any{"id": "rs_" + s.id, "type": "reasoning", "summary": []any{}},
			}), namedEvent("response.reasoning_summary_part.added", map[string]any{
				"type": "response.reasoning_summary_part.added", "item_id": "rs_" + s.id,
				"output_index": s.responseReasonIndex, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": ""},
			}))
		}
		s.responseReasonValue.WriteString(atom.delta)
		output = append(output, namedEvent("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": "rs_" + s.id, "output_index": s.responseReasonIndex, "summary_index": 0, "delta": atom.delta,
		}))
	case streamToolStart:
		call, exists := s.responseTools[atom.index]
		if exists {
			if atom.id != "" {
				call.ID = atom.id
			}
			if atom.name != "" {
				call.Name = atom.name
			}
			s.responseTools[atom.index] = call
			break
		}
		call = canonicalToolCall{ID: normalizedToolID(atom.id), Name: atom.name, Kind: atom.toolKind, Namespace: atom.namespace, Execution: atom.execution}
		if call.Name == "" && call.Kind != "tool_search" {
			break
		}
		s.responseTools[atom.index] = call
		s.responseArgs[atom.index] = &strings.Builder{}
		s.responseToolIndexes[atom.index] = s.nextResponseOutputIndex()
		output = append(output, namedEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": s.responseToolIndexes[atom.index],
			"item": responseToolItem(call, "in_progress"),
		}))
	case streamToolDelta:
		call := s.responseTools[atom.index]
		if call.ID == "" {
			break
		}
		arguments := s.responseArgs[atom.index]
		if arguments == nil {
			arguments = &strings.Builder{}
			s.responseArgs[atom.index] = arguments
		}
		arguments.WriteString(atom.delta)
		if call.Kind == "custom" || call.Kind == "tool_search" {
			break
		}
		output = append(output, namedEvent("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": responseToolItemID(call), "output_index": s.responseToolIndexes[atom.index], "delta": atom.delta,
		}))
	case streamToolDone:
		output = append(output, s.finishResponseTool(atom.index, atom)...)
	case streamUsageAtom:
		s.usage = mergeUsage(s.usage, atom.usage)
	case streamTerminal:
		if s.terminal {
			return output
		}
		status := atom.status
		if status == "" {
			status = "completed"
		}
		output = append(output, s.finalizeResponsesOutput()...)
		s.terminal = true
		s.success = status == "completed" || status == "incomplete"
		response := s.responseObject(status)
		response["usage"] = responsesUsage(s.usage)
		stop := firstNonEmpty(atom.stop, s.stop)
		if status == "incomplete" || stop == "max_output_tokens" || stop == "max_tokens" || stop == "model_context_window_exceeded" {
			status = "incomplete"
			response["status"] = status
			reason := "max_output_tokens"
			if stop == "model_context_window_exceeded" {
				reason = stop
			}
			response["incomplete_details"] = map[string]any{"reason": reason}
		}
		eventName := "response." + status
		if status != "completed" && status != "incomplete" && status != "failed" {
			eventName = "response.failed"
		}
		output = append(output, namedEvent(eventName, map[string]any{"type": eventName, "response": response}))
	}
	return output
}

func (s *backendStreamAdapter) responseObject(status string) map[string]any {
	return map[string]any{
		"id": "resp_" + s.id, "object": "response", "created_at": time.Now().Unix(), "status": status,
		"model": s.model, "output": s.responsesOutputItems(),
	}
}

func (s *backendStreamAdapter) nextResponseOutputIndex() int {
	index := s.responseNextIndex
	s.responseNextIndex++
	return index
}

func responseToolItemID(call canonicalToolCall) string {
	return "fc_" + stablePublicID(call.ID)
}

func responseToolItem(call canonicalToolCall, status string) map[string]any {
	item := map[string]any{
		"id": responseToolItemID(call), "type": "function_call", "status": status,
		"call_id": call.ID, "name": call.Name, "arguments": "",
	}
	if call.Namespace != "" {
		item["namespace"] = call.Namespace
	}
	switch call.Kind {
	case "custom":
		item["type"] = "custom_tool_call"
		delete(item, "arguments")
		item["input"] = call.Arguments
		if status == "in_progress" {
			item["input"] = ""
		}
	case "tool_search":
		item["type"] = "tool_search_call"
		item["execution"] = call.Execution
		item["arguments"] = normalizeArguments(call.Arguments)
		delete(item, "name")
		delete(item, "namespace")
	default:
		if status != "in_progress" {
			item["arguments"] = argumentsString(call.Arguments)
		}
	}
	return item
}

func (s *backendStreamAdapter) finishResponseTool(index int, atom streamAtom) []grok.SSEEvent {
	if s.responseToolDone[index] {
		return nil
	}
	call, ok := s.responseTools[index]
	if !ok || call.ID == "" {
		return nil
	}
	if atom.name != "" {
		call.Name = atom.name
	}
	if atom.id != "" {
		call.ID = atom.id
	}
	rawArguments := any(nil)
	if atom.arguments != nil {
		rawArguments = atom.arguments
	} else if arguments := s.responseArgs[index]; arguments != nil {
		rawArguments = arguments.String()
	}
	outputIndex := s.responseToolIndexes[index]
	var output []grok.SSEEvent
	switch call.Kind {
	case "custom":
		call.Arguments = aliasedCustomInput(rawArguments)
		output = append(output,
			namedEvent("response.custom_tool_call_input.delta", map[string]any{
				"type": "response.custom_tool_call_input.delta", "item_id": responseToolItemID(call),
				"output_index": outputIndex, "delta": call.Arguments,
			}),
			namedEvent("response.custom_tool_call_input.done", map[string]any{
				"type": "response.custom_tool_call_input.done", "item_id": responseToolItemID(call),
				"output_index": outputIndex, "input": call.Arguments,
			}),
		)
	case "tool_search":
		call.Arguments = completedToolArguments(rawArguments)
	default:
		call.Arguments = completedToolArguments(rawArguments)
		arguments := argumentsString(call.Arguments)
		output = append(output, namedEvent("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": responseToolItemID(call),
			"output_index": outputIndex, "arguments": arguments,
		}))
	}
	s.responseTools[index] = call
	s.responseToolDone[index] = true
	output = append(output, namedEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": outputIndex,
		"item": responseToolItem(call, "completed"),
	}))
	return output
}

func completedToolArguments(raw any) any {
	if text, ok := raw.(string); ok && strings.TrimSpace(text) == "" {
		return map[string]any{}
	}
	return parseArguments(raw)
}

func (s *backendStreamAdapter) finalizeResponsesOutput() []grok.SSEEvent {
	var output []grok.SSEEvent
	if s.responseText && !s.responseTextDone {
		s.responseTextDone = true
		text := s.responseTextValue.String()
		part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
		output = append(output,
			namedEvent("response.output_text.done", map[string]any{
				"type": "response.output_text.done", "item_id": "msg_" + s.id,
				"output_index": s.responseMessageIndex, "content_index": 0, "text": text,
			}),
			namedEvent("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "item_id": "msg_" + s.id,
				"output_index": s.responseMessageIndex, "content_index": 0, "part": part,
			}),
			namedEvent("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": s.responseMessageIndex,
				"item": map[string]any{"id": "msg_" + s.id, "type": "message", "status": "completed", "role": "assistant", "content": []any{part}},
			}),
		)
	}
	if s.responseReason && !s.responseReasonDone {
		s.responseReasonDone = true
		text := s.responseReasonValue.String()
		part := map[string]any{"type": "summary_text", "text": text}
		output = append(output,
			namedEvent("response.reasoning_summary_text.done", map[string]any{
				"type": "response.reasoning_summary_text.done", "item_id": "rs_" + s.id,
				"output_index": s.responseReasonIndex, "summary_index": 0, "text": text,
			}),
			namedEvent("response.reasoning_summary_part.done", map[string]any{
				"type": "response.reasoning_summary_part.done", "item_id": "rs_" + s.id,
				"output_index": s.responseReasonIndex, "summary_index": 0, "part": part,
			}),
			namedEvent("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": s.responseReasonIndex,
				"item": map[string]any{"id": "rs_" + s.id, "type": "reasoning", "summary": []any{part}},
			}),
		)
	}
	indices := make([]int, 0, len(s.responseTools))
	for index := range s.responseTools {
		indices = append(indices, index)
	}
	sort.Slice(indices, func(i, j int) bool {
		return s.responseToolIndexes[indices[i]] < s.responseToolIndexes[indices[j]]
	})
	for _, index := range indices {
		output = append(output, s.finishResponseTool(index, streamAtom{})...)
	}
	return output
}

func (s *backendStreamAdapter) responsesOutputItems() []any {
	type indexedItem struct {
		index int
		item  any
	}
	items := make([]indexedItem, 0, len(s.responseTools)+2)
	if s.responseReason {
		items = append(items, indexedItem{index: s.responseReasonIndex, item: map[string]any{
			"id": "rs_" + s.id, "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": s.responseReasonValue.String()}},
		}})
	}
	if s.responseMessage {
		items = append(items, indexedItem{index: s.responseMessageIndex, item: map[string]any{
			"id": "msg_" + s.id, "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": s.responseTextValue.String(), "annotations": []any{}}},
		}})
	}
	for upstreamIndex, call := range s.responseTools {
		items = append(items, indexedItem{index: s.responseToolIndexes[upstreamIndex], item: responseToolItem(call, "completed")})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].index < items[j].index })
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item.item)
	}
	return out
}

func (s *backendStreamAdapter) encodeMessages(atom streamAtom) []grok.SSEEvent {
	if atom.kind == streamErrorAtom {
		s.terminal, s.success = true, false
		return s.encodeError(atom.message, "api_error")
	}
	if atom.kind == streamReasoning && !s.messageOptions.ThinkingEnabled {
		return nil
	}
	var output []grok.SSEEvent
	if !s.messageStarted && atom.kind != streamUsageAtom {
		s.messageStarted = true
		output = append(output, namedEvent("message_start", map[string]any{
			"type": "message_start", "message": map[string]any{
				"id": "msg_" + s.id, "type": "message", "role": "assistant", "model": s.model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": s.usage.Input, "output_tokens": 0},
			},
		}))
	}
	switch atom.kind {
	case streamText, streamReasoning:
		kind := "text"
		deltaType, deltaKey := "text_delta", "text"
		if atom.kind == streamReasoning {
			kind, deltaType, deltaKey = "thinking", "thinking_delta", "thinking"
		}
		index := s.messageBlock(kind, &output, map[string]any{"type": kind, deltaKey: ""})
		output = append(output, namedEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": deltaType, deltaKey: atom.delta},
		}))
	case streamToolStart:
		key := fmt.Sprintf("tool:%d", atom.index)
		index := s.messageNext
		s.messageNext++
		s.messageBlocks[index] = key
		output = append(output, namedEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": normalizedToolID(atom.id), "name": atom.name, "input": map[string]any{}},
		}))
	case streamToolDelta:
		index := s.findMessageTool(atom.index)
		output = append(output, namedEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": atom.delta},
		}))
	case streamToolDone:
		index := s.findMessageTool(atom.index)
		output = append(output, namedEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}))
		delete(s.messageBlocks, index)
	case streamUsageAtom:
		s.usage = mergeUsage(s.usage, atom.usage)
	case streamTerminal:
		if s.terminal {
			return output
		}
		for index := range s.messageBlocks {
			output = append(output, namedEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}))
		}
		s.messageBlocks = make(map[int]string)
		s.terminal = true
		s.success = atom.status == "" || atom.status == "completed" || atom.status == "incomplete"
		if !s.success {
			return append(output, s.encodeError("upstream response failed", "api_error")...)
		}
		result := canonicalResult{StopReason: firstNonEmpty(atom.stop, s.stop)}
		if s.sawTool {
			result.Tools = []canonicalToolCall{{}}
		}
		stop := messagesStopReason(result)
		output = append(output,
			namedEvent("message_delta", map[string]any{
				"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": s.usage.Output},
			}),
			namedEvent("message_stop", map[string]any{"type": "message_stop"}),
		)
	}
	return output
}

func (s *backendStreamAdapter) messageBlock(kind string, output *[]grok.SSEEvent, content map[string]any) int {
	for index, existing := range s.messageBlocks {
		if existing == kind {
			return index
		}
	}
	index := s.messageNext
	s.messageNext++
	s.messageBlocks[index] = kind
	*output = append(*output, namedEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": index, "content_block": content,
	}))
	return index
}

func (s *backendStreamAdapter) findMessageTool(upstreamIndex int) int {
	key := fmt.Sprintf("tool:%d", upstreamIndex)
	for index, existing := range s.messageBlocks {
		if existing == key {
			return index
		}
	}
	index := s.messageNext
	s.messageNext++
	s.messageBlocks[index] = key
	return index
}

func (s *backendStreamAdapter) encodeError(message, code string) []grok.SSEEvent {
	if message == "" {
		message = "upstream stream error"
	}
	switch s.adapter.ClientProtocol {
	case inference.ProtocolMessages:
		return []grok.SSEEvent{namedEvent("error", map[string]any{
			"type": "error", "error": map[string]any{"type": "api_error", "message": message},
		})}
	case inference.ProtocolResponses:
		return []grok.SSEEvent{namedEvent("error", map[string]any{
			"type": "error", "code": code, "message": message, "param": nil,
		})}
	default:
		return []grok.SSEEvent{{Data: mustJSON(map[string]any{
			"error": map[string]any{"message": message, "type": "upstream_error", "code": code, "param": nil},
		})}}
	}
}

func namedEvent(name string, payload map[string]any) grok.SSEEvent {
	return grok.SSEEvent{Event: name, Data: mustJSON(payload)}
}

func mustJSON(value any) []byte {
	payload, _ := json.Marshal(value)
	return payload
}

func normalizeStreamChatChunk(chunk map[string]any, model string) map[string]any {
	out := cloneJSONMap(chunk)
	out["model"] = model
	if stringAt(out, "object") == "" {
		out["object"] = "chat.completion.chunk"
	}
	return out
}

func errorMessage(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case map[string]any:
		return firstNonEmptyString(value, "message", "error", "code")
	default:
		return "upstream stream error"
	}
}

func mergeUsage(current, next canonicalUsage) canonicalUsage {
	if next.Input != 0 {
		current.Input = next.Input
	}
	if next.Output != 0 {
		current.Output = next.Output
	}
	if next.Cached != 0 {
		current.Cached = next.Cached
	}
	if next.Reasoning != 0 {
		current.Reasoning = next.Reasoning
	}
	return current
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
