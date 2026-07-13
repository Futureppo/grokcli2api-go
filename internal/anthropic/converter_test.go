package anthropic

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Futureppo/grokcli2api-go/internal/grok"
)

func TestPrepareMapsMultimodalToolsAndThinking(t *testing.T) {
	body := map[string]any{
		"model": "grok-4", "max_tokens": float64(4096), "stream": true,
		"system": []any{map[string]any{"type": "text", "text": "be helpful"}},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "inspect"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}},
			}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"q": "x"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}}},
		},
		"tools":    []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}},
		"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(12000)},
	}
	prepared, err := Prepare(body)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Body["model"] != "grok-4" || prepared.Body["max_output_tokens"] != float64(4096) {
		t.Fatalf("prepared=%#v", prepared.Body)
	}
	reasoning := prepared.Body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning=%#v", reasoning)
	}
	encoded, _ := json.Marshal(prepared.Body["input"])
	text := string(encoded)
	for _, expected := range []string{"input_image", "function_call", "function_call_output", "data:image/png;base64,AAAA"} {
		if !contains(text, expected) {
			t.Fatalf("%q missing from %s", expected, text)
		}
	}
	tools := prepared.Body["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "lookup" {
		t.Fatalf("tool=%#v", tool)
	}
}

func TestPrepareCanonicalizesClaudeCodeTools(t *testing.T) {
	properties := make(map[string]any, 512)
	for i := 0; i < 512; i++ {
		properties[fmt.Sprintf("field_%03d", i)] = map[string]any{
			"type": "string", "description": "A deliberately verbose Claude Code tool property used for regression coverage.",
		}
	}
	schema := map[string]any{
		"type": "object", "properties": properties, "required": []any{"field_000"}, "additionalProperties": false,
	}
	body := map[string]any{
		"model": "grok-4", "max_tokens": float64(1024),
		"messages": []any{map[string]any{"role": "user", "content": "inspect the repository"}},
		"tools": []any{map[string]any{
			"name": "Read", "description": "Read a file", "input_schema": schema, "strict": true,
			"cache_control": map[string]any{"type": "ephemeral"},
		}},
		"mcp_servers": []any{map[string]any{"name": "github", "url": "https://example.test/mcp"}},
	}
	prepared, err := Prepare(body)
	if err != nil {
		t.Fatal(err)
	}
	tools := prepared.Body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools=%#v", tools)
	}
	function := tools[0].(map[string]any)
	parameters, ok := function["parameters"].(map[string]any)
	if function["type"] != "function" || function["name"] != "Read" || !ok || len(parameters["properties"].(map[string]any)) != 512 || function["strict"] != true {
		t.Fatalf("function=%#v", function)
	}
	if _, exists := function["cache_control"]; exists {
		t.Fatalf("unsupported cache_control leaked upstream: %#v", function)
	}
	mcp := tools[1].(map[string]any)
	if mcp["type"] != "mcp" || mcp["server_label"] != "github" {
		t.Fatalf("mcp=%#v", mcp)
	}
	encoded, err := json.Marshal(prepared.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) < 32<<10 || !contains(string(encoded), `"type":"function"`) {
		t.Fatalf("large request was not serialized with a tool type: bytes=%d", len(encoded))
	}
}

func TestPreparePreservesAnthropicWebSearchTool(t *testing.T) {
	prepared, err := Prepare(map[string]any{
		"model": "grok-4", "max_tokens": float64(128),
		"messages": []any{map[string]any{"role": "user", "content": "search"}},
		"tools":    []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": float64(3)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tool := prepared.Body["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "web_search" || tool["max_uses"] != float64(3) {
		t.Fatalf("tool=%#v", tool)
	}
}

func TestPrepareHandlesStopLocallyAndCapsDomains(t *testing.T) {
	prepared, err := Prepare(map[string]any{
		"model": "grok-4", "max_tokens": float64(128),
		"messages":       []any{map[string]any{"role": "user", "content": "search"}},
		"stop_sequences": []any{"done"},
		"tools": []any{map[string]any{
			"type": "web_search_20250305", "name": "web_search",
			"allowed_domains": []any{"a.test", "b.test", "c.test", "d.test", "e.test", "f.test"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := prepared.Body["stop"]; ok {
		t.Fatalf("stop leaked upstream: %#v", prepared.Body)
	}
	if !slices.Equal(prepared.Options.StopSequences, []string{"done"}) {
		t.Fatalf("stop sequences = %#v", prepared.Options.StopSequences)
	}
	tool := prepared.Body["tools"].([]any)[0].(map[string]any)
	if domains := tool["allowed_domains"].([]any); len(domains) != 5 {
		t.Fatalf("allowed_domains = %#v", domains)
	}
}

func TestPrepareMapsAnthropicUserIDToResponsesSafetyIdentifier(t *testing.T) {
	prepared, err := Prepare(map[string]any{
		"model": "grok-4", "max_tokens": float64(128),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"metadata": map[string]any{"user_id": "user-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Body["safety_identifier"] != "user-1" || prepared.Body["user"] != nil {
		t.Fatalf("body = %#v", prepared.Body)
	}
}

func TestValidateRejectsInvalidSamplingOptions(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"model": "grok-4", "max_tokens": float64(128),
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}
	}
	tests := []struct {
		field string
		value any
	}{
		{field: "max_tokens", value: float64(1.5)},
		{field: "stream", value: "true"},
		{field: "temperature", value: float64(2)},
		{field: "top_p", value: float64(-1)},
	}
	for _, test := range tests {
		t.Run(test.field, func(t *testing.T) {
			body := base()
			body[test.field] = test.value
			if err := Validate(body); err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestNormalizeUpstreamToolsInfersOrRejectsMissingType(t *testing.T) {
	body := map[string]any{"tools": []any{map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}
	if err := normalizeUpstreamTools(body); err != nil {
		t.Fatal(err)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("tool=%#v", tool)
	}

	err := normalizeUpstreamTools(map[string]any{"tools": []any{map[string]any{"description": "unknown"}}})
	if err == nil || !contains(err.Error(), "missing type and name") {
		t.Fatalf("err=%v", err)
	}
}

func TestPrepareRejectsToolWithoutTypeOrNameLocally(t *testing.T) {
	_, err := Prepare(map[string]any{
		"model": "grok-4", "max_tokens": float64(128),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"tools":    []any{map[string]any{"description": "unknown tool shape"}},
	})
	if err == nil || !contains(err.Error(), "tools[0] is missing type and name") {
		t.Fatalf("err=%v", err)
	}
}

func TestNormalizeResponseMapsReasoningTextToolAndUsage(t *testing.T) {
	raw := map[string]any{
		"id": "resp_1",
		"output": []any{
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "thought"}}, "encrypted_content": "sig"},
			map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"q":"x"}`},
		},
		"usage": map[string]any{"input_tokens": float64(10), "output_tokens": float64(5)},
	}
	out := NormalizeResponseWithOptions(raw, "grok-4", ResponseOptions{ThinkingEnabled: true})
	if out["type"] != "message" || out["stop_reason"] != "tool_use" {
		t.Fatalf("out=%#v", out)
	}
	blocks := out["content"].([]any)
	if len(blocks) != 3 || blocks[0].(map[string]any)["type"] != "thinking" || blocks[2].(map[string]any)["type"] != "tool_use" || blocks[2].(map[string]any)["id"] != "toolu_call_1" {
		t.Fatalf("blocks=%#v", blocks)
	}
}

func TestStreamTranslatorProducesAnthropicSequence(t *testing.T) {
	translator := NewStreamTranslator("grok-4")
	inputs := []grok.SSEEvent{
		jsonEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_1", "usage": map[string]any{"input_tokens": float64(3)}}}),
		jsonEvent("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_1", "content_index": float64(0), "part": map[string]any{"type": "output_text", "text": ""}}),
		jsonEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "content_index": float64(0), "delta": "hello"}),
		jsonEvent("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": "msg_1", "content_index": float64(0)}),
		jsonEvent("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"usage": map[string]any{"input_tokens": float64(3), "output_tokens": float64(1)}}}),
	}
	var names []string
	for _, input := range inputs {
		events, err := translator.Handle(input)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			names = append(names, event.Name)
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(names) != len(want) {
		t.Fatalf("events=%v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("events=%v", names)
		}
	}
}

func TestStreamTranslatorEmitsThinkingSignature(t *testing.T) {
	translator := NewStreamTranslatorWithOptions("grok-4", ResponseOptions{ThinkingEnabled: true})
	inputs := []grok.SSEEvent{
		jsonEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": float64(0),
			"item": map[string]any{"id": "reasoning-1", "type": "reasoning", "summary": []any{}},
		}),
		jsonEvent("response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": "reasoning-1", "delta": "thought",
		}),
		jsonEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": float64(0),
			"item": map[string]any{"id": "reasoning-1", "type": "reasoning", "encrypted_content": "signature-1"},
		}),
	}
	var deltas []map[string]any
	for _, input := range inputs {
		events, err := translator.Handle(input)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Name == "content_block_delta" {
				deltas = append(deltas, event.Data["delta"].(map[string]any))
			}
		}
	}
	if len(deltas) != 2 || deltas[0]["type"] != "thinking_delta" || deltas[1]["type"] != "signature_delta" || deltas[1]["signature"] != "signature-1" {
		t.Fatalf("deltas = %#v", deltas)
	}
}

func TestPreparePreservesMultimodalToolResult(t *testing.T) {
	prepared, err := Prepare(map[string]any{
		"model": "grok-4", "max_tokens": float64(128),
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": "toolu_1", "name": "capture", "input": map[string]any{},
			}}},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
					map[string]any{"type": "text", "text": "screenshot"},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}},
				},
			}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := prepared.Body["input"].([]any)
	output := input[1].(map[string]any)["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "input_text" || output[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("tool output = %#v", output)
	}
}

func TestPrepareValidatesToolCallRelationships(t *testing.T) {
	base := func(messages []any) map[string]any {
		return map[string]any{"model": "grok-4", "max_tokens": float64(64), "messages": messages}
	}
	tests := []struct {
		name     string
		messages []any
		want     string
	}{
		{
			name: "orphan result",
			messages: []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_missing", "content": "x"},
			}}},
			want: "does not match a pending tool_use",
		},
		{
			name: "missing result",
			messages: []any{map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{}},
			}}},
			want: "must include tool_result",
		},
		{
			name: "result after text",
			messages: []any{
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "late"}, map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "x"}}},
			},
			want: "must precede text and media",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Prepare(base(test.messages))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestNormalizeResponseFiltersUnrequestedThinkingAndNormalizesID(t *testing.T) {
	out := NormalizeResponse(map[string]any{
		"id": "response-1",
		"output": []any{
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "hidden"}}},
			map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}},
		},
	}, "grok-4")
	blocks := out["content"].([]any)
	if out["id"] != "msg_response-1" || len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "text" {
		t.Fatalf("out = %#v", out)
	}
}

func TestNormalizeResponseAppliesStopAcrossTextBlocks(t *testing.T) {
	out := NormalizeResponseWithOptions(map[string]any{
		"id": "response-1",
		"output": []any{map[string]any{"type": "message", "content": []any{
			map[string]any{"type": "output_text", "text": "ABCST"},
			map[string]any{"type": "output_text", "text": "OPXYZ"},
		}}},
	}, "grok-4", ResponseOptions{StopSequences: []string{"STOP"}})
	blocks := out["content"].([]any)
	if out["stop_reason"] != "stop_sequence" || out["stop_sequence"] != "STOP" || len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "ABC" {
		t.Fatalf("out = %#v", out)
	}
}

func TestNormalizeResponseMapsWebSearchServerTool(t *testing.T) {
	raw := map[string]any{
		"id": "response-1",
		"output": []any{
			map[string]any{"type": "web_search_call", "id": "ws_1", "action": map[string]any{"type": "search", "query": "go", "sources": []any{map[string]any{"type": "url", "url": "https://go.dev"}}}},
			map[string]any{"type": "web_search_call", "id": "ws_1", "action": map[string]any{"type": "search", "query": "go", "sources": []any{map[string]any{"type": "url", "url": "https://go.dev"}}}},
			map[string]any{"type": "message", "content": []any{map[string]any{
				"type": "output_text", "text": "Go",
				"annotations": []any{map[string]any{"type": "url_citation", "url": "https://go.dev", "title": "The Go Programming Language", "start_index": float64(0), "end_index": float64(2)}},
			}}},
		},
	}
	out := NormalizeResponse(raw, "grok-4")
	blocks := out["content"].([]any)
	if out["stop_reason"] != "end_turn" || len(blocks) != 3 {
		t.Fatalf("out = %#v", out)
	}
	use := blocks[0].(map[string]any)
	result := blocks[1].(map[string]any)
	text := blocks[2].(map[string]any)
	if use["type"] != "server_tool_use" || use["name"] != "web_search" || use["id"] != "srvtoolu_ws_1" {
		t.Fatalf("server tool = %#v", use)
	}
	results := result["content"].([]any)
	if result["type"] != "web_search_tool_result" || result["tool_use_id"] != use["id"] || len(results) != 1 || results[0].(map[string]any)["title"] != "The Go Programming Language" {
		t.Fatalf("result = %#v", result)
	}
	if len(text["citations"].([]any)) != 1 {
		t.Fatalf("text = %#v", text)
	}
}

func TestStreamTranslatorAppliesStopAcrossDeltas(t *testing.T) {
	translator := NewStreamTranslatorWithOptions("grok-4", ResponseOptions{StopSequences: []string{"STOP"}})
	inputs := []grok.SSEEvent{
		jsonEvent("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "response-1"}}),
		jsonEvent("response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "message-1", "content_index": float64(0), "part": map[string]any{"type": "output_text"}}),
		jsonEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "message-1", "content_index": float64(0), "delta": "ABCST"}),
		jsonEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "message-1", "content_index": float64(0), "delta": "OPXYZ"}),
		jsonEvent("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{}}),
	}
	var text string
	var delta map[string]any
	for _, input := range inputs {
		events, err := translator.Handle(input)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Name == "content_block_delta" {
				part := event.Data["delta"].(map[string]any)
				text += stringValue(part["text"])
			}
			if event.Name == "message_delta" {
				delta = event.Data["delta"].(map[string]any)
			}
		}
	}
	if text != "ABC" || delta["stop_reason"] != "stop_sequence" || delta["stop_sequence"] != "STOP" {
		t.Fatalf("text=%q delta=%#v", text, delta)
	}
}

func TestStreamTranslatorMapsWebSearchResult(t *testing.T) {
	translator := NewStreamTranslator("grok-4")
	added := map[string]any{"id": "ws_1", "type": "web_search_call", "action": map[string]any{"type": "search", "query": "go", "sources": []any{}}}
	done := map[string]any{"id": "ws_1", "type": "web_search_call", "action": map[string]any{"type": "search", "query": "go", "sources": []any{map[string]any{"type": "url", "url": "https://go.dev"}}}}
	inputs := []grok.SSEEvent{
		jsonEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "item": added}),
		jsonEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": done}),
		jsonEvent("response.completed", map[string]any{"type": "response.completed", "response": map[string]any{}}),
	}
	var starts []map[string]any
	var stopReason any
	for _, input := range inputs {
		events, err := translator.Handle(input)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Name == "content_block_start" {
				starts = append(starts, event.Data["content_block"].(map[string]any))
			}
			if event.Name == "message_delta" {
				stopReason = event.Data["delta"].(map[string]any)["stop_reason"]
			}
		}
	}
	if len(starts) != 2 || starts[0]["type"] != "server_tool_use" || starts[0]["name"] != "web_search" || starts[1]["type"] != "web_search_tool_result" || stopReason != "end_turn" {
		t.Fatalf("starts=%#v stop=%#v", starts, stopReason)
	}
}

func jsonEvent(name string, value map[string]any) grok.SSEEvent {
	b, _ := json.Marshal(value)
	return grok.SSEEvent{Event: name, Data: b}
}

func contains(value, substring string) bool {
	for i := 0; i+len(substring) <= len(value); i++ {
		if value[i:i+len(substring)] == substring {
			return true
		}
	}
	return false
}
