package openai

import (
	"reflect"
	"strings"
	"testing"
)

func TestPrepareChatRebuildsWhitelistAndMapsAliases(t *testing.T) {
	body := map[string]any{
		"model": " grok-4 ", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": float64(64), "max_completion_tokens": float64(128), "maxTokens": float64(256),
		"presencePenalty": float64(0.25), "frequencyPenalty": float64(-0.5),
		"stop_sequences": []any{"done"}, "reasoning": map[string]any{"effort": "adaptive", "summary": "verbose"},
		"user": " session-a ", "metadata": map[string]any{"user_id": "metadata-user", "trace": "secret"},
		"parallel_tool_calls": true, "stream_options": map[string]any{"include_usage": true}, "seed": float64(7),
	}
	original := cloneJSONValue(body)
	prepared := mustPrepareChat(t, body)

	if !reflect.DeepEqual(body, original) {
		t.Fatal("PrepareChat mutated its input")
	}
	want := map[string]any{
		"model": "grok-4", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": false,
		"max_tokens": float64(64), "presence_penalty": float64(0.25), "frequency_penalty": float64(-0.5),
		"stop": []any{"done"}, "reasoning_effort": "medium", "user_id": "session-a",
	}
	if !reflect.DeepEqual(prepared.Body, want) {
		t.Fatalf("body = %#v, want %#v", prepared.Body, want)
	}
	for _, path := range []string{"parallel_tool_calls", "seed", "stream_options", "max_completion_tokens", "reasoning.summary", "metadata.trace"} {
		if !hasChatChange(prepared.Changes, path) {
			t.Fatalf("missing change for %s: %#v", path, prepared.Changes)
		}
	}
}

func TestPrepareChatMapsLegacyFunctionsAndChoice(t *testing.T) {
	prepared := mustPrepareChat(t, map[string]any{
		"model": "grok-4", "messages": []any{map[string]any{"role": "user", "content": "weather"}},
		"functions": []any{map[string]any{
			"name": "weather", "description": "look up weather", "parameters": map[string]any{"type": "object"}, "vendor": true,
		}},
		"function_call": map[string]any{"name": "weather", "ignored": true},
	})
	tools := prepared.Body["tools"].([]any)
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "weather" || function["vendor"] != nil {
		t.Fatalf("function = %#v", function)
	}
	choice := prepared.Body["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["function"].(map[string]any)["name"] != "weather" {
		t.Fatalf("tool_choice = %#v", choice)
	}
	if !hasChatChange(prepared.Changes, "functions") || !hasChatChange(prepared.Changes, "function_call") {
		t.Fatalf("changes = %#v", prepared.Changes)
	}
}

func TestPrepareChatNormalizesMessagesAndToolHistory(t *testing.T) {
	prepared := mustPrepareChat(t, map[string]any{
		"model": "grok-4",
		"messages": []any{
			map[string]any{"role": "developer", "content": "rules", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "inspect", "cache_control": true},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA", "detail": "low", "vendor": true}},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "BB", "vendor": true}, "cache_control": true},
			}},
			map[string]any{"role": "assistant", "content": nil, "reasoning_content": "need tool", "tool_calls": []any{
				map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"id":1}`, "extra": true}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "name": "lookup", "content": "ok"},
			map[string]any{"role": "user", "content": "continue"},
		},
	})
	messages := prepared.Body["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer role was not normalized: %#v", messages[0])
	}
	parts := messages[1].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["cache_control"] != nil || parts[1].(map[string]any)["image_url"].(map[string]any)["vendor"] != nil {
		t.Fatalf("content metadata leaked: %#v", parts)
	}
	convertedImage := parts[2].(map[string]any)
	if convertedImage["type"] != "image_url" || convertedImage["image_url"].(map[string]any)["url"] != "data:image/png;base64,BB" {
		t.Fatalf("Anthropic image block was not normalized: %#v", convertedImage)
	}
	call := messages[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["extra"] != nil {
		t.Fatalf("tool call metadata leaked: %#v", call)
	}
	for _, path := range []string{"messages[0].role", "messages[0].cache_control", "messages[1].content[0].cache_control", "messages[1].content[2]", "messages[2].tool_calls[0].function.extra"} {
		if !hasChatChange(prepared.Changes, path) {
			t.Fatalf("missing change for %s", path)
		}
	}
}

func TestPrepareChatSanitizesResponseFormatAndSearchParameters(t *testing.T) {
	prepared := mustPrepareChat(t, map[string]any{
		"model": "grok-4", "messages": []any{map[string]any{"role": "user", "content": "find"}},
		"response_format": map[string]any{
			"type": "json_schema", "vendor": true,
			"json_schema": map[string]any{"name": "result", "description": "result", "schema": map[string]any{"type": "object"}, "strict": true, "extra": true},
		},
		"search_parameters": map[string]any{
			"web": true, "safe_search": true, "allowed_websites": []any{"example.com"}, "max_search_results": float64(5), "unknown": true,
		},
	})
	format := prepared.Body["response_format"].(map[string]any)
	if format["vendor"] != nil || format["json_schema"].(map[string]any)["extra"] != nil {
		t.Fatalf("response format was not sanitized: %#v", format)
	}
	search := prepared.Body["search_parameters"].(map[string]any)
	if search["unknown"] != nil || search["web"] != true {
		t.Fatalf("search parameters = %#v", search)
	}
	if !hasChatChange(prepared.Changes, "search_parameters.unknown") {
		t.Fatalf("changes = %#v", prepared.Changes)
	}
}

func TestPrepareChatRejectsInvalidSemanticStructures(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"model": "grok-4", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	}
	tests := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{name: "temperature", edit: func(body map[string]any) { body["temperature"] = 3 }, want: "temperature"},
		{name: "max tokens", edit: func(body map[string]any) { body["max_tokens"] = 1.5 }, want: "positive integer"},
		{name: "reasoning", edit: func(body map[string]any) { body["reasoning_effort"] = "impossible" }, want: "reasoning_effort"},
		{name: "null user", edit: func(body map[string]any) { body["user_id"] = nil }, want: "user_id"},
		{name: "null user content", edit: func(body map[string]any) { body["messages"] = []any{map[string]any{"role": "user", "content": nil}} }, want: "messages[0].content"},
		{name: "image in system message", edit: func(body map[string]any) {
			body["messages"] = []any{map[string]any{"role": "system", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/a.png"}}}}}
		}, want: "only valid for user messages"},
		{name: "unknown tool result", edit: func(body map[string]any) {
			body["messages"] = []any{map[string]any{"role": "tool", "tool_call_id": "missing", "content": "x"}}
		}, want: "does not reference"},
		{name: "unresolved tool call", edit: func(body map[string]any) {
			body["messages"] = []any{map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call-1", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"},
				}},
			}}
		}, want: "unresolved tool call"},
		{name: "tool parameters", edit: func(body map[string]any) {
			body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "x"}}}
		}, want: "parameters"},
		{name: "response schema", edit: func(body map[string]any) {
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "x"}}
		}, want: "schema.schema"},
		{name: "search field", edit: func(body map[string]any) { body["search_parameters"] = map[string]any{"safe_search": "yes"} }, want: "safe_search"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := base()
			test.edit(body)
			_, err := PrepareChat(body)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestPrepareChatStrictModelAppliesAfterAliasMapping(t *testing.T) {
	prepared := mustPrepareChat(t, map[string]any{
		"model": "grok-4.5-fast", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"presencePenalty": 0.2, "frequencyPenalty": 0.3, "stop_sequences": []any{"done"}, "response_format": map[string]any{"type": "json_object"},
	})
	for _, key := range []string{"presence_penalty", "frequency_penalty", "stop"} {
		if _, ok := prepared.Body[key]; ok {
			t.Fatalf("strict model retained %s: %#v", key, prepared.Body)
		}
	}
	if prepared.Body["response_format"] == nil {
		t.Fatalf("supported field was stripped: %#v", prepared.Body)
	}
}

func hasChatChange(changes []ChatChange, path string) bool {
	for _, change := range changes {
		if change.Path == path {
			return true
		}
	}
	return false
}
