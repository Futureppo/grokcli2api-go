package openai

import "testing"

func TestPrepareChatPreservesExtensions(t *testing.T) {
	body := map[string]any{"model": "grok-4", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "reasoning_effort": "low"}
	out := PrepareChat(body)
	if out["reasoning_effort"] != "low" {
		t.Fatal("extension field lost")
	}
	if out["stream"] != false {
		t.Fatal("stream default should be false")
	}
}

func TestPrepareResponsesMapsModel(t *testing.T) {
	body := map[string]any{"model": "grok-4.5", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true}
	out := PrepareResponses(body)
	if out["model"] != "grok-build" {
		t.Fatalf("model = %v", out["model"])
	}
	if out["stream"] != true {
		t.Fatal("stream flag lost")
	}
	if _, ok := out["input"].([]any); !ok {
		t.Fatalf("input = %#v", out["input"])
	}
}

func TestPrepareResponsesUsesNativeInputAndLegacyAliases(t *testing.T) {
	body := map[string]any{
		"model": "grok-4", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"max_completion_tokens": float64(123), "response_format": map[string]any{"type": "json_object"},
	}
	out := PrepareResponses(body)
	if _, ok := out["messages"]; ok {
		t.Fatal("legacy messages field was not removed")
	}
	if _, ok := out["input"].([]any); !ok || out["max_output_tokens"] != float64(123) {
		t.Fatalf("aliases not mapped: %#v", out)
	}
	text, _ := out["text"].(map[string]any)
	if _, ok := text["format"].(map[string]any); !ok {
		t.Fatalf("response_format not mapped: %#v", out["text"])
	}
}

func TestNormalizeResponseDoesNotCreateChatEnvelope(t *testing.T) {
	out := NormalizeResponse(map[string]any{"output": []any{map[string]any{"type": "message"}}}, "grok-4")
	if out["object"] != "response" || out["model"] != "grok-4" {
		t.Fatalf("unexpected response: %#v", out)
	}
	if _, exists := out["choices"]; exists {
		t.Fatal("Responses object must not contain synthesized choices")
	}
}

func TestEnsureAssistantRoleUsesFirstChunk(t *testing.T) {
	chunk := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "hello"}}}}
	if !EnsureAssistantRole(chunk) {
		t.Fatal("role was not inserted")
	}
	choices := chunk["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)
	if delta["role"] != "assistant" {
		t.Fatalf("delta=%#v", delta)
	}
}

func TestErrorHasSingleEnvelope(t *testing.T) {
	err := Error("bad", "auth_error", "401")
	if len(err) != 1 || err["error"] == nil {
		t.Fatalf("unexpected error: %#v", err)
	}
}
