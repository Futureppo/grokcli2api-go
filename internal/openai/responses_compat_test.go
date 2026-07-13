package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareCompatibleResponsesDefersNamespaceToolsThroughClientSearch(t *testing.T) {
	body := map[string]any{
		"model": "grok-4.5", "input": "hello",
		"tools": []any{
			map[string]any{
				"type": "namespace", "name": "mcp__calendar__", "description": "Calendar tools",
				"tools": []any{
					map[string]any{"type": "function", "name": "create", "description": "Create event", "defer_loading": true, "parameters": objectSchema()},
					map[string]any{"type": "function", "name": "list", "description": "List events", "parameters": objectSchema()},
				},
			},
			map[string]any{"type": "tool_search", "execution": "client", "description": "Find tools", "parameters": objectSchema()},
		},
	}
	wire, compat, err := PrepareCompatibleResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	tools := toolsByName(t, wire["tools"])
	if _, ok := tools["mcp__calendar__create"]; ok {
		t.Fatal("deferred function was exposed before tool search")
	}
	if _, ok := tools["mcp__calendar__list"]; !ok {
		t.Fatal("non-deferred namespace function was not flattened")
	}
	search, ok := tools["grokcli2api_tool_search"]
	if !ok || !strings.Contains(String(search, "description", ""), "mcp__calendar__") {
		t.Fatalf("tool search shim = %#v", search)
	}
	if compat.aliases["mcp__calendar__list"].Namespace != "mcp__calendar__" {
		t.Fatalf("namespace mapping = %#v", compat.aliases["mcp__calendar__list"])
	}
}

func TestPrepareCompatibleResponsesLoadsToolSearchOutput(t *testing.T) {
	body := map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{"type": "tool_search_call", "call_id": "search-1", "execution": "client", "arguments": map[string]any{"goal": "calendar"}},
			map[string]any{
				"type": "tool_search_output", "call_id": "search-1", "execution": "client", "status": "completed",
				"tools": []any{map[string]any{"type": "function", "name": "calendar_create", "defer_loading": true, "parameters": objectSchema()}},
			},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
		},
	}
	wire, _, err := PrepareCompatibleResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	tools := toolsByName(t, wire["tools"])
	loaded, ok := tools["calendar_create"]
	if !ok {
		t.Fatalf("loaded tools = %#v", tools)
	}
	if _, exists := loaded["defer_loading"]; exists {
		t.Fatal("loaded tool retained defer_loading")
	}
	input := wire["input"].([]any)
	if len(input) != 2 || String(input[0].(map[string]any), "type", "") != "message" {
		t.Fatalf("rewritten input = %#v", input)
	}
}

func TestPrepareCompatibleResponsesRewritesCodexInputItems(t *testing.T) {
	body := map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{"type": "agent_message", "author": "worker", "recipient": "root", "content": []any{map[string]any{"type": "input_text", "text": "done"}}},
			map[string]any{"type": "local_shell_call", "call_id": "shell-1", "status": "completed", "action": map[string]any{"type": "exec", "command": []any{"echo", "ok"}}},
			map[string]any{"type": "mcp_tool_call_output", "call_id": "mcp-1", "output": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}},
			map[string]any{"type": "custom_tool_call", "call_id": "custom-1", "name": "exec", "namespace": "plugin__", "input": "echo ok"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "custom-1", "name": "exec", "output": "ok"},
		},
		"tools": []any{map[string]any{"type": "custom", "name": "exec", "description": "Execute", "format": map[string]any{"type": "text"}}},
	}
	wire, _, err := PrepareCompatibleResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	want := []string{"message", "message", "message", "function_call", "function_call_output"}
	for index, expected := range want {
		if got := String(input[index].(map[string]any), "type", ""); got != expected {
			t.Fatalf("input[%d] type = %q, want %q", index, got, expected)
		}
	}
	call := input[3].(map[string]any)
	if call["name"] != "plugin__exec" || !strings.Contains(String(call, "arguments", ""), "echo ok") {
		t.Fatalf("custom call = %#v", call)
	}
	tools := toolsByName(t, wire["tools"])
	if _, ok := tools["exec"]; !ok {
		t.Fatalf("custom wrapper missing: %#v", tools)
	}
}

func TestPrepareCompatibleResponsesDropsUnknownInput(t *testing.T) {
	wire, _, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5", "input": []any{
			map[string]any{"type": "future_item"},
			map[string]any{"type": "message", "role": "user", "phase": "commentary", "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn-1"}, "content": []any{map[string]any{"type": "input_text", "text": "hello"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %#v", input)
	}
	message := input[0].(map[string]any)
	if _, ok := message["phase"]; ok {
		t.Fatalf("phase leaked upstream: %#v", message)
	}
	if _, ok := message["internal_chat_message_metadata_passthrough"]; ok {
		t.Fatalf("internal metadata leaked upstream: %#v", message)
	}
}

func TestPrepareCompatibleResponsesRewritesAssistantOutputTextHistory(t *testing.T) {
	wire, _, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{
				"type": "message", "role": "assistant", "id": "msg_1", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "first response"}},
			},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	message := input[0].(map[string]any)
	content := message["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "first response" {
		t.Fatalf("assistant history content = %#v", content)
	}
}

func TestPrepareCompatibleResponsesDoesNotAddMissingMessageContent(t *testing.T) {
	wire, _, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{"type": "message", "role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	message := input[0].(map[string]any)
	if _, exists := message["content"]; exists {
		t.Fatalf("missing content was added: %#v", message)
	}
}

func TestPrepareCompatibleResponsesRewritesAnthropicImageContent(t *testing.T) {
	wire, _, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5",
		"input": []any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "describe"},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := wire["input"].([]any)
	message := input[0].(map[string]any)
	content := message["content"].([]any)
	image := content[1].(map[string]any)
	if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,AAAA" {
		t.Fatalf("image content = %#v", image)
	}
}

func TestPrepareCompatibleResponsesNormalizesCodexHostedTools(t *testing.T) {
	wire, _, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5", "input": "hello",
		"tool_choice": map[string]any{"type": "image_generation"},
		"tools": []any{
			map[string]any{
				"type": "web_search", "external_web_access": true, "indexed_web_access": true,
				"search_content_types": []any{"text"}, "search_context_size": "high",
				"user_location": map[string]any{"type": "approximate", "country": "CN"},
				"filters":       map[string]any{"allowed_domains": []any{"a.test", "b.test", "c.test", "d.test", "e.test", "f.test"}},
			},
			map[string]any{"type": "image_generation", "quality": "auto"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tools := wire["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %#v", tools)
	}
	web := tools[0].(map[string]any)
	for _, key := range []string{"external_web_access", "indexed_web_access", "search_content_types", "filters", "allowed_domains"} {
		if _, ok := web[key]; ok {
			t.Fatalf("%s leaked upstream: %#v", key, web)
		}
	}
	if len(web) != 1 || web["type"] != "web_search" {
		t.Fatalf("web search tool = %#v", web)
	}
	image := tools[1].(map[string]any)
	if image["type"] != "image_generation" || image["quality"] != "auto" {
		t.Fatalf("image tool = %#v", image)
	}
	if wire["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %#v", wire["tool_choice"])
	}
}

func TestPrepareCompatibleResponsesDropsEncryptedAgentMessage(t *testing.T) {
	wire, _, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5", "input": []any{
			map[string]any{"type": "agent_message", "author": "worker", "recipient": "root", "content": []any{map[string]any{"type": "encrypted_text", "encrypted_content": "opaque"}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if input := wire["input"].([]any); len(input) != 1 || String(input[0].(map[string]any), "type", "") != "message" {
		t.Fatalf("input = %#v", input)
	}
}

func TestPrepareCompatibleResponsesDropsNonPortableEncryptedReasoning(t *testing.T) {
	wire, compat, err := PrepareCompatibleResponses(map[string]any{
		"model":   "grok-4.5",
		"include": []any{"reasoning.encrypted_content", "web_search_call.action.sources"},
		"input": []any{
			map[string]any{"type": "reasoning", "id": "rs_encrypted", "summary": []any{}, "encrypted_content": "opaque-only"},
			map[string]any{"type": "compaction", "id": "cmp_1", "encrypted_content": "opaque-compaction"},
			map[string]any{"type": "reasoning", "id": "rs_summary", "summary": []any{map[string]any{"type": "summary_text", "text": "usable"}}, "encrypted_content": "opaque-with-summary"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	includes := wire["include"].([]any)
	if len(includes) != 1 || includes[0] != "web_search_call.action.sources" {
		t.Fatalf("include = %#v", includes)
	}
	input := wire["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input = %#v", input)
	}
	reasoning := input[0].(map[string]any)
	if reasoning["id"] != "rs_summary" || reasoning["encrypted_content"] != nil {
		t.Fatalf("reasoning = %#v", reasoning)
	}

	response := compat.NormalizeResponse(map[string]any{"output": []any{
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "new-opaque"},
	}}, "grok-4.5")
	responseReasoning := response["output"].([]any)[0].(map[string]any)
	if _, ok := responseReasoning["encrypted_content"]; ok {
		t.Fatalf("response reasoning leaked encrypted_content: %#v", responseReasoning)
	}

	streamed := translateOne(t, compat, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "stream-opaque"},
	})
	streamReasoning := streamed["item"].(map[string]any)
	if _, ok := streamReasoning["encrypted_content"]; ok {
		t.Fatalf("stream reasoning leaked encrypted_content: %#v", streamReasoning)
	}
}

func TestResponsesCompatibilityRestoresNamespacedAndCustomCalls(t *testing.T) {
	body := map[string]any{
		"model": "grok-4.5", "input": "hello",
		"tools": []any{
			map[string]any{"type": "namespace", "name": "mcp__github__", "tools": []any{map[string]any{"type": "function", "name": "fetch", "parameters": objectSchema()}}},
			map[string]any{"type": "custom", "name": "code", "description": "Run code"},
		},
	}
	_, compat, err := PrepareCompatibleResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{
		"output": []any{
			map[string]any{"type": "function_call", "name": "mcp__github__fetch", "call_id": "call-1", "arguments": `{}`},
			map[string]any{"type": "function_call", "name": "code", "call_id": "call-2", "arguments": `{"input":"print(1)"}`},
		},
	}
	out := compat.NormalizeResponse(raw, "grok-4.5")
	items := out["output"].([]any)
	namespaced := items[0].(map[string]any)
	if namespaced["name"] != "fetch" || namespaced["namespace"] != "mcp__github__" {
		t.Fatalf("namespaced call = %#v", namespaced)
	}
	custom := items[1].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["input"] != "print(1)" {
		t.Fatalf("custom call = %#v", custom)
	}
}

func TestResponsesCompatibilityTranslatesStreamCalls(t *testing.T) {
	body := map[string]any{
		"model": "grok-4.5", "input": "hello",
		"tools": []any{
			map[string]any{"type": "namespace", "name": "ns__", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": objectSchema()}}},
			map[string]any{"type": "custom", "name": "code"},
		},
	}
	_, compat, err := PrepareCompatibleResponses(body)
	if err != nil {
		t.Fatal(err)
	}

	added := translateOne(t, compat, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": "item-1", "type": "function_call", "name": "ns__lookup", "call_id": "call-1", "arguments": ""},
	})
	item := added["item"].(map[string]any)
	if item["name"] != "lookup" || item["namespace"] != "ns__" {
		t.Fatalf("added item = %#v", item)
	}

	_ = translateOne(t, compat, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 1,
		"item": map[string]any{"id": "item-2", "type": "function_call", "name": "code", "call_id": "call-2", "arguments": ""},
	})
	if events := translate(t, compat, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "item-2", "delta": `{"input":"hel`}); len(events) != 0 {
		t.Fatalf("custom delta should be buffered: %#v", events)
	}
	events := translate(t, compat, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": "item-2", "arguments": `{"input":"hello"}`})
	if len(events) != 2 || events[0].Event != "response.custom_tool_call_input.delta" || events[1].Event != "response.custom_tool_call_input.done" {
		t.Fatalf("custom events = %#v", events)
	}
	var done map[string]any
	if err := json.Unmarshal(events[1].Data, &done); err != nil || done["input"] != "hello" {
		t.Fatalf("custom done = %#v err=%v", done, err)
	}
}

func TestResponsesCompatibilityTranslatesToolSearchStream(t *testing.T) {
	_, compat, err := PrepareCompatibleResponses(map[string]any{
		"model": "grok-4.5", "input": "hello",
		"tools": []any{map[string]any{
			"type": "tool_search", "execution": "client", "description": "Find tools",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"goal": map[string]any{"type": "string"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	added := translateOne(t, compat, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": "search-item", "type": "function_call", "name": "grokcli2api_tool_search", "call_id": "search-call", "arguments": ""},
	})
	item := added["item"].(map[string]any)
	if item["type"] != "tool_search_call" || item["execution"] != "client" {
		t.Fatalf("added search item = %#v", item)
	}
	if events := translate(t, compat, "response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "search-item", "delta": `{"goal":"calendar"}`,
	}); len(events) != 0 {
		t.Fatalf("search arguments should be buffered: %#v", events)
	}
	if events := translate(t, compat, "response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": "search-item", "arguments": `{"goal":"calendar"}`,
	}); len(events) != 0 {
		t.Fatalf("search argument completion should be represented by output_item.done: %#v", events)
	}
	done := translateOne(t, compat, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{"id": "search-item", "type": "function_call", "name": "grokcli2api_tool_search", "call_id": "search-call", "arguments": `{"goal":"calendar"}`},
	})
	doneItem := done["item"].(map[string]any)
	arguments, ok := doneItem["arguments"].(map[string]any)
	if doneItem["type"] != "tool_search_call" || !ok || arguments["goal"] != "calendar" {
		t.Fatalf("done search item = %#v", doneItem)
	}
}

func TestResponsesCompatibilityUsesCollisionSafeAliases(t *testing.T) {
	body := map[string]any{
		"model": "grok-4.5", "input": "hello",
		"tools": []any{
			map[string]any{"type": "namespace", "name": "ab", "tools": []any{map[string]any{"type": "function", "name": "c", "parameters": objectSchema()}}},
			map[string]any{"type": "namespace", "name": "a", "tools": []any{map[string]any{"type": "function", "name": "bc", "parameters": objectSchema()}}},
		},
	}
	wire, compat, err := PrepareCompatibleResponses(body)
	if err != nil {
		t.Fatal(err)
	}
	tools := wire["tools"].([]any)
	first := String(tools[0].(map[string]any), "name", "")
	second := String(tools[1].(map[string]any), "name", "")
	if first == second || !strings.HasPrefix(second, "abc__") {
		t.Fatalf("aliases = %q, %q", first, second)
	}
	response := compat.NormalizeResponse(map[string]any{"output": []any{
		map[string]any{"type": "function_call", "name": first, "arguments": `{}`},
		map[string]any{"type": "function_call", "name": second, "arguments": `{}`},
	}}, "grok-4.5")
	items := response["output"].([]any)
	if items[0].(map[string]any)["namespace"] != "ab" || items[1].(map[string]any)["namespace"] != "a" {
		t.Fatalf("restored items = %#v", items)
	}
}

func objectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func toolsByName(t *testing.T, raw any) map[string]map[string]any {
	t.Helper()
	tools, ok := raw.([]any)
	if !ok {
		t.Fatalf("tools = %#v", raw)
	}
	out := make(map[string]map[string]any, len(tools))
	for _, rawTool := range tools {
		tool := rawTool.(map[string]any)
		out[String(tool, "name", String(tool, "type", ""))] = tool
	}
	return out
}

func translateOne(t *testing.T, compat *ResponsesCompatibility, event string, payload map[string]any) map[string]any {
	t.Helper()
	events := translate(t, compat, event, payload)
	if len(events) != 1 {
		t.Fatalf("translated events = %#v", events)
	}
	var out map[string]any
	if err := json.Unmarshal(events[0].Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func translate(t *testing.T, compat *ResponsesCompatibility, event string, payload map[string]any) []StreamEvent {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	events, err := compat.TranslateStream(event, data)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestTranslateStreamDropsNativeEventsAndFiltersNestedResponse(t *testing.T) {
	compat := &ResponsesCompatibility{
		aliases: map[string]toolIdentity{}, originalAliases: map[string]string{}, streamCalls: map[string]*streamToolCall{},
	}
	if events := translate(t, compat, "grok.custom", map[string]any{"type": "grok.custom", "value": true}); len(events) != 0 {
		t.Fatalf("native event leaked: %#v", events)
	}
	completed := translateOne(t, compat, "response.completed", map[string]any{
		"type":     "response.completed",
		"response": map[string]any{"id": "resp_1", "object": "response", "status": "completed", "output": []any{}, "grok_field": true},
	})
	response := completed["response"].(map[string]any)
	if _, ok := response["grok_field"]; ok {
		t.Fatalf("native response field leaked: %#v", response)
	}
}
