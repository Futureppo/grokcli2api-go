package openai

import (
	"errors"
	"strings"
	"testing"
)

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

func TestPrepareResponsesDefaultsStoreToTrue(t *testing.T) {
	defaulted := PrepareResponses(map[string]any{"model": "grok-4.5", "input": "hello"})
	if defaulted["store"] != true {
		t.Fatalf("default store = %#v, want true", defaulted["store"])
	}
	explicit := PrepareResponses(map[string]any{"model": "grok-4.5", "input": "hello", "store": false})
	if explicit["store"] != false {
		t.Fatalf("explicit store = %#v, want false", explicit["store"])
	}
}

func TestPrepareResponsesStripsFieldsOutsideUpstreamSchema(t *testing.T) {
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

	other := PrepareResponses(map[string]any{
		"model": "grok-4", "input": "hello", "external_web_access": true,
		"presence_penalty": float64(0.5), "stop": []any{"done"}, "unknown": true,
	})
	for _, key := range []string{"external_web_access", "presence_penalty", "stop", "unknown"} {
		if _, ok := other[key]; ok {
			t.Fatalf("%s should not be forwarded: %#v", key, other)
		}
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

func TestPrepareResponsesMapsUserToSafetyIdentifier(t *testing.T) {
	out := PrepareResponses(map[string]any{
		"model": "grok-4", "input": "hello", "user": "user-1",
	})
	if out["safety_identifier"] != "user-1" {
		t.Fatalf("safety_identifier = %#v", out["safety_identifier"])
	}
	if _, ok := out["user"]; ok {
		t.Fatalf("legacy user leaked upstream: %#v", out)
	}
}

func TestValidateResponsesRequestRejectsInvalidUpstreamOptions(t *testing.T) {
	tests := []struct {
		field string
		value any
	}{
		{field: "stream", value: "true"},
		{field: "max_output_tokens", value: float64(1.5)},
		{field: "max_tool_calls", value: float64(0)},
		{field: "temperature", value: float64(3)},
		{field: "top_p", value: float64(-1)},
		{field: "top_logprobs", value: float64(21)},
	}
	for _, test := range tests {
		t.Run(test.field, func(t *testing.T) {
			body := map[string]any{"model": "grok-4", "input": "hello", test.field: test.value}
			if err := ValidateResponsesRequest(body, false); err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	if err := ValidateResponsesRequest(map[string]any{
		"model": "grok-build", "input": "hello", "stream": "native-extension",
	}, true); err != nil {
		t.Fatalf("native request was over-validated: %v", err)
	}
}

func TestNormalizeResponseDoesNotCreateChatEnvelope(t *testing.T) {
	out := NormalizeResponse(map[string]any{"output": []any{map[string]any{"type": "message"}}, "grok_extension": true}, "grok-4")
	if out["object"] != "response" || out["model"] != "grok-4" {
		t.Fatalf("unexpected response: %#v", out)
	}
	if _, exists := out["choices"]; exists {
		t.Fatal("Responses object must not contain synthesized choices")
	}
	if _, exists := out["grok_extension"]; exists {
		t.Fatal("Grok-native response field leaked into the default OpenAI response")
	}
}

func TestNormalizeResponseUsesRequestedModelAndSanitizesUsage(t *testing.T) {
	out := NormalizeResponse(map[string]any{
		"model":   "grok-4.5-build-free",
		"billing": map[string]any{"cost_in_usd_ticks": float64(12)},
		"usage": map[string]any{
			"input_tokens": float64(10),
			"input_tokens_details": map[string]any{
				"cached_tokens": float64(3), "context_details": map[string]any{"x": true},
			},
			"output_tokens": float64(4),
			"output_tokens_details": map[string]any{
				"reasoning_tokens": float64(2), "num_server_side_tools_used": float64(1),
			},
			"total_tokens": float64(14), "cost_in_usd_ticks": float64(12), "num_sources_used": float64(2),
		},
	}, "grok-4.5")
	if out["model"] != "grok-4.5" {
		t.Fatalf("model = %#v", out["model"])
	}
	if _, ok := out["billing"]; ok {
		t.Fatalf("billing leaked: %#v", out)
	}
	usage := out["usage"].(map[string]any)
	if len(usage) != 5 || usage["cost_in_usd_ticks"] != nil || usage["num_sources_used"] != nil {
		t.Fatalf("usage = %#v", usage)
	}
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if len(inputDetails) != 1 || inputDetails["cached_tokens"] != float64(3) {
		t.Fatalf("input token details = %#v", inputDetails)
	}
	outputDetails := usage["output_tokens_details"].(map[string]any)
	if len(outputDetails) != 1 || outputDetails["reasoning_tokens"] != float64(2) {
		t.Fatalf("output token details = %#v", outputDetails)
	}
}

func TestValidateResponsesRequestReportsPreciseParameters(t *testing.T) {
	tests := []struct {
		name  string
		body  map[string]any
		param string
		code  string
	}{
		{name: "previous response", body: map[string]any{"model": "grok-4.5", "input": "x", "previous_response_id": 1}, param: "previous_response_id"},
		{name: "image url", body: map[string]any{"model": "grok-4.5", "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "x"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,%%%"}}}}}, param: "input[0].content[1].image_url"},
		{name: "image detail", body: map[string]any{"model": "grok-4.5", "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "https://example.com/a.png", "detail": "giant"}}}}}, param: "input[0].content[0].detail"},
		{name: "file id", body: map[string]any{"model": "grok-4.5", "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "file_id": "file_1"}}}}}, param: "input[0].content[0].file_id", code: "unsupported_parameter"},
		{name: "function parameters", body: map[string]any{"model": "grok-4.5", "input": "x", "tools": []any{map[string]any{"type": "function", "name": "f", "parameters": "bad"}}}, param: "tools[0].parameters"},
		{name: "orphan output", body: map[string]any{"model": "grok-4.5", "input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "x"}}}, param: "input[0].call_id"},
		{name: "unsupported conversation", body: map[string]any{"model": "grok-4.5", "input": "x", "conversation": "conv_1"}, param: "conversation", code: "unsupported_parameter"},
		{name: "unknown field", body: map[string]any{"model": "grok-4.5", "input": "x", "future_option": true}, param: "future_option", code: "unsupported_parameter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateResponsesRequest(test.body, false)
			var requestErr *RequestError
			if !errors.As(err, &requestErr) {
				t.Fatalf("error = %#v", err)
			}
			if requestErr.Param != test.param {
				t.Fatalf("param = %q, want %q", requestErr.Param, test.param)
			}
			if test.code != "" && requestErr.Code != test.code {
				t.Fatalf("code = %q, want %q", requestErr.Code, test.code)
			}
		})
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
