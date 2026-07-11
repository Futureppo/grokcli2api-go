package anthropic

import (
	"encoding/json"
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
	out := NormalizeResponse(raw, "grok-4")
	if out["type"] != "message" || out["stop_reason"] != "tool_use" {
		t.Fatalf("out=%#v", out)
	}
	blocks := out["content"].([]any)
	if len(blocks) != 3 || blocks[0].(map[string]any)["type"] != "thinking" || blocks[2].(map[string]any)["type"] != "tool_use" {
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
