package openai

import (
	"strings"
	"testing"
)

func TestToolReplayRestoresCallsFromPreviousResponseID(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	RememberCompletedResponse(cache, "grok-4.5", map[string]any{
		"id":    "resp_turn1",
		"model": "grok-4.5",
		"output": []any{
			map[string]any{
				"id": "fc_1", "type": "function_call", "call_id": "call_1",
				"name": "lookup", "arguments": `{"q":"weather"}`,
			},
		},
	}, "")

	// Alma multi-turn: only tool output + previous_response_id.
	wire, _, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model":                "grok-4.5",
		"previous_response_id": "resp_turn1",
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "sunny"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "thanks"}}},
		},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["previous_response_id"]; ok {
		t.Fatal("previous_response_id should be stripped before upstream")
	}
	input := wire["input"].([]any)
	if len(input) < 2 {
		t.Fatalf("input = %#v", input)
	}
	call := input[0].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "lookup" {
		t.Fatalf("restored call = %#v", call)
	}
	output := input[1].(map[string]any)
	if output["type"] != "function_call_output" || output["call_id"] != "call_1" {
		t.Fatalf("tool output = %#v", output)
	}
}

func TestToolReplayExpandsItemReference(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	RememberCompletedResponse(cache, "grok-4.5", map[string]any{
		"id":    "resp_turn1",
		"model": "grok-4.5",
		"output": []any{
			map[string]any{
				"id": "fc_item", "type": "function_call", "call_id": "call_ref",
				"name": "search", "arguments": `{"q":"x"}`,
			},
		},
	}, "")

	wire, _, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "continue"},
			map[string]any{"type": "item_reference", "id": "fc_item"},
			map[string]any{"type": "function_call_output", "call_id": "call_ref", "output": "ok"},
		},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %#v", input)
	}
	if String(input[1].(map[string]any), "type", "") != "function_call" {
		t.Fatalf("expanded item = %#v", input[1])
	}
}

func TestToolReplayPreservesNamespacedFunctionAlias(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	RememberCompletedResponse(cache, "grok-4.5", map[string]any{
		"id": "resp_ns",
		"output": []any{map[string]any{
			"id": "fc_ns", "type": "function_call", "call_id": "call_ns",
			"name": "fetch", "namespace": "mcp__github__", "arguments": `{}`,
		}},
	}, "")
	tools := []any{map[string]any{
		"type": "namespace", "name": "mcp__github__",
		"tools": []any{map[string]any{"type": "function", "name": "fetch", "parameters": objectSchema()}},
	}}
	tests := map[string]map[string]any{
		"previous_response_id": {
			"model": "grok-4.5", "previous_response_id": "resp_ns", "tools": tools,
			"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_ns", "output": "ok"}},
		},
		"item_reference": {
			"model": "grok-4.5", "tools": tools,
			"input": []any{
				map[string]any{"type": "item_reference", "id": "fc_ns"},
				map[string]any{"type": "function_call_output", "call_id": "call_ns", "output": "ok"},
			},
		},
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			wire, _, err := PrepareCompatibleResponsesWithCache(body, cache)
			if err != nil {
				t.Fatal(err)
			}
			input := wire["input"].([]any)
			call := input[0].(map[string]any)
			if got := String(call, "name", ""); got != "mcp__github__fetch" {
				t.Fatalf("replayed call name = %q, want namespaced tool alias; call = %#v", got, call)
			}
			if _, ok := toolsByName(t, wire["tools"])["mcp__github__fetch"]; !ok {
				t.Fatalf("namespaced tool missing from wire tools: %#v", wire["tools"])
			}
		})
	}
}

func TestNormalizeReplayItemsPreservesNamespace(t *testing.T) {
	tests := []map[string]any{
		{
			"type": "function_call", "call_id": "call_fn", "name": "fetch",
			"namespace": "mcp__github__", "arguments": `{}`,
		},
		{
			"type": "custom_tool_call", "call_id": "call_custom", "name": "exec",
			"namespace": "plugin__", "input": "echo ok",
		},
	}
	for _, item := range tests {
		got := normalizeReplayItems([]map[string]any{item})
		if len(got) != 1 || got[0]["namespace"] != item["namespace"] {
			t.Fatalf("normalized replay item lost namespace: got %#v, input %#v", got, item)
		}
	}
}

func TestToolReplayPrunesOrphanOutputs(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	wire, _, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{"type": "item_reference", "id": "missing"},
			map[string]any{"type": "function_call_output", "call_id": "orphan", "output": "x"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	if len(input) != 1 || String(input[0].(map[string]any), "type", "") != "message" {
		t.Fatalf("expected only message after orphan prune, got %#v", input)
	}
}

func TestPruneOrphanToolOutputsKeepsSliceWhenUnchanged(t *testing.T) {
	input := []any{
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	}
	body := map[string]any{"input": input}
	pruneOrphanToolOutputs(body)
	got := body["input"].([]any)
	if &got[0] != &input[0] {
		t.Fatal("unchanged input was copied")
	}
}

func TestToolReplayRestoresFromStreamIndexedPreviousResponse(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	// Stream path: output_item.done indexes under prev-resp + item id.
	RememberStreamToolCall(cache, "grok-4.5", map[string]any{
		"id": "fc_stream", "type": "function_call", "call_id": "call_stream",
		"name": "lookup", "arguments": `{"q":"x"}`,
	}, "resp_stream", "")

	// Alma multi-turn path: previous_response_id + item_reference + output.
	wire, _, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model":                "grok-4.5",
		"previous_response_id": "resp_stream",
		"input": []any{
			map[string]any{"type": "item_reference", "id": "fc_stream"},
			map[string]any{"type": "function_call_output", "call_id": "call_stream", "output": "result"},
		},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	if len(input) < 2 {
		t.Fatalf("input = %#v", input)
	}
	call := input[0].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_stream" {
		t.Fatalf("restored call = %#v", call)
	}
}

func TestToolReplayUsesClientModelNotUpstreamFreeAlias(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	// Upstream rewrites free-tier responses to grok-4.5-build-free.
	RememberCompletedResponse(cache, "grok-4.5", map[string]any{
		"id":    "resp_free",
		"model": "grok-4.5-build-free",
		"output": []any{
			map[string]any{
				"id": "fc_free", "type": "function_call", "call_id": "call_free",
				"name": "lookup", "arguments": `{"q":"x"}`,
			},
		},
	}, "")

	// Next Alma turn still asks for the client model name.
	wire, _, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model":                "grok-4.5",
		"previous_response_id": "resp_free",
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "call_free", "output": "ok"},
		},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	if len(input) < 1 || String(input[0].(map[string]any), "type", "") != "function_call" {
		t.Fatalf("client model lookup missed free-tier cache entry: %#v", input)
	}
}

func TestPrepareCompatibleResponsesHardensArgumentAndOutputShapes(t *testing.T) {
	wire, _, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{
				"type": "function_call", "call_id": "c1", "name": "lookup",
				"arguments": map[string]any{"q": "x"},
			},
			map[string]any{
				"type": "function_call_output", "call_id": "c1",
				"output": []any{
					map[string]any{"type": "output_text", "text": "line1"},
					map[string]any{"type": "output_text", "text": "line2"},
				},
			},
		},
		"tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": objectSchema()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	call := input[0].(map[string]any)
	args, ok := call["arguments"].(string)
	if !ok || !strings.Contains(args, `"q"`) {
		t.Fatalf("arguments not stringified: %#v", call["arguments"])
	}
	output := input[1].(map[string]any)
	if got, ok := output["output"].(string); !ok || !strings.Contains(got, "line1") || !strings.Contains(got, "line2") {
		t.Fatalf("output not flattened: %#v", output["output"])
	}
}

func TestPrepareCompatibleResponsesDropsServerToolHistory(t *testing.T) {
	wire, _, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
			map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed"},
			map[string]any{"type": "x_search_call", "id": "xs_1", "status": "completed"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "again"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("expected server tool history dropped, got %#v", input)
	}
	for _, raw := range input {
		if String(raw.(map[string]any), "type", "") != "message" {
			t.Fatalf("unexpected item %#v", raw)
		}
	}
}

func TestPrepareCompatibleResponsesStripsNullReasoningFields(t *testing.T) {
	// Live 422: reasoning with content:null fails ModelInput untagged enum.
	wire, _, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{
				"type": "reasoning", "id": "rs_1",
				"summary":           []any{map[string]any{"type": "summary_text", "text": "think"}},
				"content":           nil,
				"encrypted_content": nil,
			},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input = %#v", input)
	}
	reasoning := input[0].(map[string]any)
	if reasoning["type"] != "reasoning" {
		t.Fatalf("reasoning dropped unexpectedly: %#v", reasoning)
	}
	if _, ok := reasoning["content"]; ok {
		t.Fatalf("content null must be stripped: %#v", reasoning)
	}
	if _, ok := reasoning["encrypted_content"]; ok {
		t.Fatalf("encrypted_content must be stripped: %#v", reasoning)
	}
}
