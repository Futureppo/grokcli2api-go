package openai

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestStoredToolContinuationForwardsPreviousResponseWithoutReplay(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	RememberCompletedResponseWithStore(cache, "grok-4.5", map[string]any{
		"id": "resp_stored", "output": []any{map[string]any{
			"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{}`,
		}},
	}, "", true)
	wire, compat, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model": "grok-4.5", "previous_response_id": "resp_stored",
		"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"}},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if wire["previous_response_id"] != "resp_stored" {
		t.Fatalf("previous_response_id = %#v", wire["previous_response_id"])
	}
	input := wire["input"].([]any)
	if len(input) != 1 || String(input[0].(map[string]any), "type", "") != "function_call_output" {
		t.Fatalf("stored continuation was replayed: %#v", input)
	}
	normalized := compat.NormalizeResponse(map[string]any{"id": "resp_2", "output": []any{}}, "grok-4.5")
	if _, exists := normalized["previous_response_id"]; exists {
		t.Fatalf("stored continuation synthesized previous id: %#v", normalized)
	}
}

func TestUnknownPreviousResponseIDIsForwarded(t *testing.T) {
	wire, _, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model": "grok-4.5", "previous_response_id": "resp_unknown", "input": "continue",
	}, NewToolReplayCache(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if wire["previous_response_id"] != "resp_unknown" {
		t.Fatalf("wire = %#v", wire)
	}
}

func TestStatelessToolReplayRestoresPreviousResponseField(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	RememberCompletedResponseWithStore(cache, "grok-4.5", map[string]any{
		"id": "resp_stateless", "output": []any{map[string]any{
			"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{}`,
		}},
	}, "", false)
	wire, compat, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model": "grok-4.5", "previous_response_id": "resp_stateless",
		"input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"}},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := wire["previous_response_id"]; exists {
		t.Fatalf("stateless replay forwarded previous id: %#v", wire)
	}
	normalized := compat.NormalizeResponse(map[string]any{"id": "resp_2", "output": []any{}}, "grok-4.5")
	if normalized["previous_response_id"] != "resp_stateless" {
		t.Fatalf("normalized previous id = %#v", normalized["previous_response_id"])
	}
}

func TestStatelessParallelToolReplayAcceptsOutOfOrderOutputs(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	RememberCompletedResponseWithStore(cache, "grok-4.5", map[string]any{
		"id": "resp_parallel", "output": []any{
			map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "one", "arguments": `{}`},
			map[string]any{"id": "fc_2", "type": "function_call", "call_id": "call_2", "name": "two", "arguments": `{}`},
		},
	}, "", false)
	wire, _, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model": "grok-4.5", "previous_response_id": "resp_parallel", "parallel_tool_calls": true,
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "call_2", "output": "second"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "first"},
		},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := wire["previous_response_id"]; exists {
		t.Fatalf("previous id leaked after complete replay: %#v", wire)
	}
	input := wire["input"].([]any)
	if len(input) != 4 || String(input[0].(map[string]any), "call_id", "") != "call_1" || String(input[1].(map[string]any), "call_id", "") != "call_2" {
		t.Fatalf("parallel replay = %#v", input)
	}
}

func TestIncompleteStatelessToolReplayPreservesPreviousResponseID(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	RememberCompletedResponseWithStore(cache, "grok-4.5", map[string]any{
		"id": "resp_incomplete", "output": []any{
			map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "one", "arguments": `{}`},
		},
	}, "", false)
	wire, _, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model": "grok-4.5", "previous_response_id": "resp_incomplete",
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "one"},
			map[string]any{"type": "function_call_output", "call_id": "call_missing", "output": "missing"},
		},
	}, cache)
	if err != nil {
		t.Fatal(err)
	}
	if wire["previous_response_id"] != "resp_incomplete" {
		t.Fatalf("incomplete replay lost previous id: %#v", wire)
	}
	input := wire["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("incomplete replay partially mutated input: %#v", input)
	}
}

func TestToolReplayCacheConcurrentAccess(t *testing.T) {
	cache := NewToolReplayCache(time.Minute, 64)
	item := []map[string]any{{"id": "fc", "type": "function_call", "call_id": "call", "name": "lookup", "arguments": `{}`}}
	var wg sync.WaitGroup
	for index := 0; index < 16; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			key := fmt.Sprintf("prev-resp:resp_%d", index%4)
			for iteration := 0; iteration < 100; iteration++ {
				cache.put("grok-4.5", key, item, iteration%2 == 0, true)
				_, _ = cache.getRecord("grok-4.5", key)
			}
		}(index)
	}
	wg.Wait()
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

func TestToolReplayRejectsOrphanOutputs(t *testing.T) {
	cache := NewToolReplayCache(0, 0)
	_, _, err := PrepareCompatibleResponsesWithCache(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{"type": "item_reference", "id": "missing"},
			map[string]any{"type": "function_call_output", "call_id": "orphan", "output": "x"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		},
	}, cache)
	var requestErr *RequestError
	if !errors.As(err, &requestErr) || requestErr.Param != "input[0].call_id" {
		t.Fatalf("error = %#v", err)
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
