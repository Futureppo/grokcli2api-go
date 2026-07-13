package anthropic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Futureppo/grokcli2api-go/internal/grok"
)

type Event struct {
	Name string
	Data map[string]any
}

type streamBlock struct {
	Index     int
	Kind      string
	Arguments string
	SentArgs  bool
	Closed    bool
}

// StreamTranslator converts the richer Responses event stream into the
// ordered Anthropic Messages SSE state machine.
type StreamTranslator struct {
	model        string
	options      ResponseOptions
	messageID    string
	started      bool
	finished     bool
	nextIndex    int
	blocks       map[string]*streamBlock
	inputTokens  float64
	outputTokens float64
	stopReason   string
	stopSequence string
	textKey      string
	stopFilter   *streamStopFilter
}

func NewStreamTranslator(model string) *StreamTranslator {
	return NewStreamTranslatorWithOptions(model, ResponseOptions{})

}

func NewStreamTranslatorWithOptions(model string, options ResponseOptions) *StreamTranslator {
	return &StreamTranslator{
		model: model, options: options, blocks: map[string]*streamBlock{}, stopReason: "end_turn",
		stopFilter: newStreamStopFilter(options.StopSequences),
	}
}

func (t *StreamTranslator) Handle(upstream grok.SSEEvent) ([]Event, error) {
	if len(upstream.Data) == 0 || string(upstream.Data) == "[DONE]" {
		return nil, nil
	}
	var data map[string]any
	if err := json.Unmarshal(upstream.Data, &data); err != nil {
		return nil, fmt.Errorf("invalid Grok Responses event: %w", err)
	}
	kind := upstream.Event
	if kind == "" {
		kind, _ = data["type"].(string)
	}
	if t.stopSequence != "" && kind != "response.completed" && kind != "response.incomplete" && kind != "response.failed" {
		return nil, nil
	}
	var events []Event
	ensureStart := func(response map[string]any) {
		if !t.started {
			events = append(events, t.startEvent(response))
		}
	}

	switch kind {
	case "response.created", "response.in_progress", "response.queued":
		response, _ := data["response"].(map[string]any)
		ensureStart(response)
	case "response.output_item.added":
		ensureStart(nil)
		item, _ := data["item"].(map[string]any)
		itemKind, _ := item["type"].(string)
		key := eventKey(data, item)
		switch itemKind {
		case "reasoning":
			if t.options.ThinkingEnabled {
				events = append(events, t.openBlock(key, "thinking", map[string]any{"type": "thinking", "thinking": ""})...)
			}
		case "function_call", "custom_tool_call":
			t.stopReason = "tool_use"
			id := anthropicID("toolu_", rawItemID(item))
			events = append(events, t.openBlock(key, "tool_use", map[string]any{"type": "tool_use", "id": id, "name": item["name"], "input": map[string]any{}})...)
			if block := t.blocks[key]; block != nil {
				block.Arguments, _ = item["arguments"].(string)
			}
		case "web_search_call":
			id := anthropicID("srvtoolu_", rawItemID(item))
			events = append(events, t.openBlock(key, "server_tool_use", map[string]any{"type": "server_tool_use", "id": id, "name": "web_search", "input": webSearchInput(item)})...)
		case "file_search_call", "code_interpreter_call", "computer_call", "mcp_call",
			"image_generation_call", "local_shell_call", "shell_call", "apply_patch_call", "mcp_list_tools":
			events = append(events, t.openBlock(key, "server_tool_use", map[string]any{"type": "server_tool_use", "id": anthropicID("srvtoolu_", rawItemID(item)), "name": itemKind, "input": item["action"]})...)
		}
	case "response.content_part.added":
		ensureStart(nil)
		part, _ := data["part"].(map[string]any)
		key := eventKey(data, part)
		switch part["type"] {
		case "output_text", "text":
			events = append(events, t.openBlock(key, "text", map[string]any{"type": "text", "text": ""})...)
		case "refusal":
			events = append(events, t.openBlock(key, "text", map[string]any{"type": "text", "text": ""})...)
		}
	case "response.output_text.delta", "response.refusal.delta":
		ensureStart(nil)
		key := eventKey(data, nil)
		if t.blocks[key] == nil {
			events = append(events, t.openBlock(key, "text", map[string]any{"type": "text", "text": ""})...)
		}
		value, _ := data["delta"].(string)
		t.textKey = key
		emit, matched := t.stopFilter.Push(value)
		if emit != "" {
			events = append(events, t.delta(key, map[string]any{"type": "text_delta", "text": emit})...)
		}
		if matched != "" {
			t.stopReason = "stop_sequence"
			t.stopSequence = matched
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if !t.options.ThinkingEnabled {
			break
		}
		ensureStart(nil)
		key := eventKey(data, nil)
		if t.blocks[key] == nil {
			events = append(events, t.openBlock(key, "thinking", map[string]any{"type": "thinking", "thinking": ""})...)
		}
		value, _ := data["delta"].(string)
		events = append(events, t.delta(key, map[string]any{"type": "thinking_delta", "thinking": value})...)
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		ensureStart(nil)
		key := eventKey(data, nil)
		if t.blocks[key] == nil {
			t.stopReason = "tool_use"
			rawID := stringOr(data["call_id"], stringOr(data["item_id"], ""))
			events = append(events, t.openBlock(key, "tool_use", map[string]any{"type": "tool_use", "id": anthropicID("toolu_", rawID), "name": data["name"], "input": map[string]any{}})...)
		}
		value, _ := data["delta"].(string)
		if block := t.blocks[key]; block != nil {
			block.SentArgs = true
		}
		events = append(events, t.delta(key, map[string]any{"type": "input_json_delta", "partial_json": value})...)
	case "response.output_text.annotation.added":
		key := eventKey(data, nil)
		annotation, _ := data["annotation"].(map[string]any)
		if citation := anthropicCitation(annotation, ""); citation != nil {
			events = append(events, t.delta(key, map[string]any{"type": "citations_delta", "citation": citation})...)
		}
	case "response.content_part.done", "response.output_text.done", "response.refusal.done", "response.reasoning_summary_text.done", "response.reasoning_text.done":
		key := eventKey(data, nil)
		if block := t.blocks[key]; block != nil && block.Kind == "text" {
			events = append(events, t.flushText(key)...)
		}
		events = append(events, t.closeBlock(key)...)
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		key := eventKey(data, nil)
		if block := t.blocks[key]; block != nil && !block.SentArgs {
			args := stringOr(data["arguments"], stringOr(data["input"], block.Arguments))
			if args != "" {
				events = append(events, t.delta(key, map[string]any{"type": "input_json_delta", "partial_json": args})...)
				block.SentArgs = true
			}
		}
		events = append(events, t.closeBlock(key)...)
	case "response.output_item.done":
		item, _ := data["item"].(map[string]any)
		key := eventKey(data, item)
		if block := t.blocks[key]; block != nil {
			if block.Kind == "tool_use" && !block.SentArgs {
				args := block.Arguments
				if args == "" {
					args, _ = item["arguments"].(string)
				}
				if args != "" {
					events = append(events, t.delta(key, map[string]any{"type": "input_json_delta", "partial_json": args})...)
				}
			}
			if block.Kind == "thinking" && t.options.ThinkingEnabled {
				if signature, _ := item["encrypted_content"].(string); signature != "" {
					events = append(events, t.delta(key, map[string]any{"type": "signature_delta", "signature": signature})...)
				}
			}
		}
		events = append(events, t.closeBlock(key)...)
		if stringValue(item["type"]) == "web_search_call" {
			rawID := rawItemID(item)
			resultKey := key + ":result"
			result := map[string]any{
				"type": "web_search_tool_result", "tool_use_id": anthropicID("srvtoolu_", rawID),
				"content": webSearchResults([]any{item}, rawID, map[string]string{}),
			}
			events = append(events, t.openBlock(resultKey, "web_search_tool_result", result)...)
			events = append(events, t.closeBlock(resultKey)...)
		}
	case "response.completed", "response.incomplete", "response.failed":
		response, _ := data["response"].(map[string]any)
		ensureStart(response)
		events = append(events, t.finish(response)...)
	case "error":
		return []Event{{Name: "error", Data: Error(stringOr(data["message"], "upstream stream error"), "api_error")}}, nil
	}
	return events, nil
}

func (t *StreamTranslator) Finish() []Event {
	if t.finished {
		return nil
	}
	events := []Event{}
	if !t.started {
		events = append(events, t.startEvent(nil))
	}
	return append(events, t.finish(nil)...)
}

func (t *StreamTranslator) startEvent(response map[string]any) Event {
	t.started = true
	if response != nil {
		t.messageID = anthropicID("msg_", stringOr(response["id"], ""))
		if usage, ok := response["usage"].(map[string]any); ok {
			t.inputTokens = firstNumber(usage, "input_tokens", "prompt_tokens")
		}
	}
	if t.messageID == "" {
		t.messageID = anthropicID("msg_", "")
	}
	return Event{Name: "message_start", Data: map[string]any{"type": "message_start", "message": map[string]any{
		"id": t.messageID, "type": "message", "role": "assistant", "model": t.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": t.inputTokens, "output_tokens": float64(0)},
	}}}
}

func (t *StreamTranslator) openBlock(key, kind string, content map[string]any) []Event {
	if existing := t.blocks[key]; existing != nil {
		return nil
	}
	block := &streamBlock{Index: t.nextIndex, Kind: kind}
	t.nextIndex++
	t.blocks[key] = block
	return []Event{{Name: "content_block_start", Data: map[string]any{"type": "content_block_start", "index": block.Index, "content_block": content}}}
}

func (t *StreamTranslator) delta(key string, delta map[string]any) []Event {
	block := t.blocks[key]
	if block == nil || block.Closed {
		return nil
	}
	return []Event{{Name: "content_block_delta", Data: map[string]any{"type": "content_block_delta", "index": block.Index, "delta": delta}}}
}

func (t *StreamTranslator) closeBlock(key string) []Event {
	block := t.blocks[key]
	if block == nil || block.Closed {
		return nil
	}
	block.Closed = true
	return []Event{{Name: "content_block_stop", Data: map[string]any{"type": "content_block_stop", "index": block.Index}}}
}

func (t *StreamTranslator) flushText(key string) []Event {
	if t.stopFilter == nil || t.stopSequence != "" {
		return nil
	}
	value := t.stopFilter.Flush()
	if value == "" {
		return nil
	}
	return t.delta(key, map[string]any{"type": "text_delta", "text": value})
}

func (t *StreamTranslator) finish(response map[string]any) []Event {
	if t.finished {
		return nil
	}
	var events []Event
	if t.textKey != "" {
		events = append(events, t.flushText(t.textKey)...)
	}
	keys := make([]string, 0, len(t.blocks))
	for key := range t.blocks {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return t.blocks[keys[i]].Index < t.blocks[keys[j]].Index })
	for _, key := range keys {
		events = append(events, t.closeBlock(key)...)
	}
	if response != nil {
		if usage, ok := response["usage"].(map[string]any); ok {
			t.outputTokens = firstNumber(usage, "output_tokens", "completion_tokens")
		}
		if t.stopSequence == "" {
			if sequence, _ := response["stop_sequence"].(string); sequence != "" {
				t.stopReason = "stop_sequence"
				t.stopSequence = sequence
			}
		}
		if t.stopSequence == "" {
			if details, ok := response["incomplete_details"].(map[string]any); ok && details["reason"] == "max_output_tokens" {
				t.stopReason = "max_tokens"
			}
		}
	}
	t.finished = true
	events = append(events,
		Event{Name: "message_delta", Data: map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": t.stopReason, "stop_sequence": nullableString(t.stopSequence)}, "usage": map[string]any{"output_tokens": t.outputTokens}}},
		Event{Name: "message_stop", Data: map[string]any{"type": "message_stop"}},
	)
	return events
}

type streamStopFilter struct {
	sequences []string
	pending   string
	matched   string
}

func newStreamStopFilter(sequences []string) *streamStopFilter {
	return &streamStopFilter{sequences: append([]string(nil), sequences...)}
}

func (f *streamStopFilter) Push(value string) (string, string) {
	if f == nil || len(f.sequences) == 0 {
		return value, ""
	}
	if f.matched != "" {
		return "", f.matched
	}
	f.pending += value
	stopAt := -1
	matched := ""
	for _, sequence := range f.sequences {
		if index := strings.Index(f.pending, sequence); index >= 0 && (stopAt < 0 || index < stopAt) {
			stopAt = index
			matched = sequence
		}
	}
	if stopAt >= 0 {
		emit := f.pending[:stopAt]
		f.pending = ""
		f.matched = matched
		return emit, matched
	}
	hold := 0
	for _, sequence := range f.sequences {
		limit := len(sequence) - 1
		if limit > len(f.pending) {
			limit = len(f.pending)
		}
		for size := limit; size > hold; size-- {
			if strings.HasSuffix(f.pending, sequence[:size]) {
				hold = size
				break
			}
		}
	}
	emitAt := len(f.pending) - hold
	emit := f.pending[:emitAt]
	f.pending = f.pending[emitAt:]
	return emit, ""
}

func (f *streamStopFilter) Flush() string {
	if f == nil || f.matched != "" {
		return ""
	}
	value := f.pending
	f.pending = ""
	return value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func eventKey(data map[string]any, nested map[string]any) string {
	item := stringOr(data["item_id"], "")
	if item == "" && nested != nil {
		item = stringOr(nested["id"], stringOr(nested["call_id"], ""))
	}
	if item == "" {
		item = "item"
	}
	if index, ok := number(data["content_index"]); ok {
		return fmt.Sprintf("%s:%g", item, index)
	}
	return item
}

func stringOr(value any, fallback string) string {
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return fallback
}
