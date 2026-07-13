package openai

import "testing"

func TestPrepareChatPreservesExtensions(t *testing.T) {
	body := map[string]any{"model": "grok-4", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "reasoning_effort": "low"}
	out := mustPrepareChat(t, body).Body
	if out["reasoning_effort"] != "low" {
		t.Fatal("extension field lost")
	}
	if out["stream"] != false {
		t.Fatal("stream default should be false")
	}
}

func TestPrepareChatStripsUnsupportedGrok45PresencePenalty(t *testing.T) {
	body := map[string]any{
		"model": "grok-4.5", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"presence_penalty": float64(0.5), "presencePenalty": float64(0.5),
	}
	out := mustPrepareChat(t, body).Body
	if _, ok := out["presence_penalty"]; ok {
		t.Fatal("presence_penalty was forwarded to grok-4.5")
	}
	if _, ok := out["presencePenalty"]; ok {
		t.Fatal("presencePenalty was forwarded to grok-4.5")
	}
	if body["presence_penalty"] != float64(0.5) || body["presencePenalty"] != float64(0.5) {
		t.Fatal("PrepareChat mutated the caller's request")
	}

	other := mustPrepareChat(t, map[string]any{
		"model": "grok-4", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"presence_penalty": float64(0.5),
	}).Body
	if other["presence_penalty"] != float64(0.5) {
		t.Fatal("presence_penalty should remain available to other models")
	}
}

func TestPrepareChatStripsUnsupportedComposerPresencePenalty(t *testing.T) {
	out := mustPrepareChat(t, map[string]any{
		"model": "composer", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"presence_penalty": float64(0.5), "presencePenalty": float64(0.5),
	}).Body
	if _, ok := out["presence_penalty"]; ok {
		t.Fatal("presence_penalty was forwarded to composer")
	}
	if _, ok := out["presencePenalty"]; ok {
		t.Fatal("presencePenalty was forwarded to composer")
	}
}

func TestPrepareChatNormalizesStrictComposerParameters(t *testing.T) {
	out := mustPrepareChat(t, map[string]any{
		"model": "grok-composer-2.5-fast", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stop": []any{"done"},
		"frequency_penalty": float64(0.2), "frequencyPenalty": float64(0.2),
		"reasoning_effort": "adaptive",
	}).Body
	for _, key := range []string{"stop", "frequency_penalty", "frequencyPenalty"} {
		if _, ok := out[key]; ok {
			t.Fatalf("%s was forwarded: %#v", key, out)
		}
	}
	if out["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort = %#v", out["reasoning_effort"])
	}
}

func TestPrepareResponsesPreservesModel(t *testing.T) {
	body := map[string]any{"model": "grok-4.5", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "stream": true}
	out := PrepareResponses(body)
	if out["model"] != "grok-4.5" {
		t.Fatalf("model = %v", out["model"])
	}
	if out["stream"] != true {
		t.Fatal("stream flag lost")
	}
	if _, ok := out["input"].([]any); !ok {
		t.Fatalf("input = %#v", out["input"])
	}
}

func TestPrepareResponsesStripsUnsupportedGrok45ExternalWebAccess(t *testing.T) {
	body := map[string]any{
		"model": "grok-4.5", "input": "hello",
		"external_web_access": true, "externalWebAccess": true,
	}
	out := PrepareResponses(body)
	if _, ok := out["external_web_access"]; ok {
		t.Fatal("external_web_access was forwarded to grok-4.5")
	}
	if _, ok := out["externalWebAccess"]; ok {
		t.Fatal("externalWebAccess was forwarded to grok-4.5")
	}
	if body["external_web_access"] != true || body["externalWebAccess"] != true {
		t.Fatal("PrepareResponses mutated the caller's request")
	}

	other := PrepareResponses(map[string]any{"model": "grok-4", "input": "hello", "external_web_access": true})
	if other["external_web_access"] != true {
		t.Fatal("external_web_access should remain available to other models")
	}
}

func TestPrepareResponsesStripsUnsupportedComposerExternalWebAccess(t *testing.T) {
	out := PrepareResponses(map[string]any{
		"model": "composer", "input": "hello",
		"external_web_access": true, "externalWebAccess": true,
	})
	if _, ok := out["external_web_access"]; ok {
		t.Fatal("external_web_access was forwarded to composer")
	}
	if _, ok := out["externalWebAccess"]; ok {
		t.Fatal("externalWebAccess was forwarded to composer")
	}
}

func TestPrepareResponsesCanonicalizesReasoningEffort(t *testing.T) {
	out := PrepareResponses(map[string]any{
		"model": "grok-4.5", "input": "hello", "reasoning_effort": "xhigh",
	})
	if _, ok := out["reasoning_effort"]; ok {
		t.Fatalf("legacy reasoning_effort leaked: %#v", out)
	}
	reasoning, ok := out["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %#v", out["reasoning"])
	}
}

func TestPrepareResponsesFallsBackUnknownReasoningEffort(t *testing.T) {
	out := PrepareResponses(map[string]any{
		"model": "grok-4.5", "input": "hello", "reasoning_effort": "vendor-adaptive",
	})
	reasoning, ok := out["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "medium" {
		t.Fatalf("reasoning = %#v", out["reasoning"])
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

func mustPrepareChat(t *testing.T, body map[string]any) PreparedChat {
	t.Helper()
	prepared, err := PrepareChat(body)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}
