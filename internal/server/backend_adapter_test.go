package server

import (
	"encoding/json"
	"testing"

	"github.com/Futureppo/grokcli2api-go/internal/anthropic"
	"github.com/Futureppo/grokcli2api-go/internal/inference"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

func TestBackendAdapterThreeByThreeNonStreamingContract(t *testing.T) {
	upstreams := map[modelcatalog.Backend]map[string]any{
		modelcatalog.BackendChatCompletions: {
			"id": "chatcmpl_upstream", "object": "chat.completion", "model": "wire-chat",
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "matrix-ok"},
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		},
		modelcatalog.BackendResponses: {
			"id": "resp_upstream", "object": "response", "status": "completed", "model": "wire-responses",
			"output": []any{map[string]any{
				"id": "msg_upstream", "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "matrix-ok", "annotations": []any{}}},
			}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
		},
		modelcatalog.BackendMessages: {
			"id": "msg_upstream", "type": "message", "role": "assistant", "model": "wire-messages",
			"content":     []any{map[string]any{"type": "text", "text": "matrix-ok"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		},
	}
	protocols := []inference.Protocol{
		inference.ProtocolChatCompletions,
		inference.ProtocolResponses,
		inference.ProtocolMessages,
	}
	for backend, payload := range upstreams {
		for _, protocol := range protocols {
			t.Run(string(backend)+"/to/"+string(protocol), func(t *testing.T) {
				response := adaptBackendResponse(inference.ResponseAdapter{
					ClientProtocol: protocol, UpstreamBackend: backend,
				}, payload, "public-model", anthropic.ResponseOptions{})
				switch protocol {
				case inference.ProtocolChatCompletions:
					choices, _ := response["choices"].([]any)
					if len(choices) != 1 || response["object"] != "chat.completion" {
						t.Fatalf("chat response = %#v", response)
					}
				case inference.ProtocolResponses:
					output, _ := response["output"].([]any)
					if len(output) == 0 || response["status"] != "completed" {
						t.Fatalf("responses response = %#v", response)
					}
				case inference.ProtocolMessages:
					content, _ := response["content"].([]any)
					if len(content) == 0 || response["type"] != "message" {
						t.Fatalf("messages response = %#v", response)
					}
				}
				encoded, err := json.Marshal(response)
				if err != nil || !json.Valid(encoded) || !containsJSONText(encoded, "matrix-ok") {
					t.Fatalf("response lost text: %s (err=%v)", encoded, err)
				}
			})
		}
	}
}

func containsJSONText(encoded []byte, text string) bool {
	var value any
	if json.Unmarshal(encoded, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(current any) bool {
		switch current := current.(type) {
		case string:
			return current == text
		case []any:
			for _, item := range current {
				if visit(item) {
					return true
				}
			}
		case map[string]any:
			for _, item := range current {
				if visit(item) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func TestBackendAdapterNativeResponsesRestoresAllToolAliases(t *testing.T) {
	adapter := inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendResponses,
		ToolAliases: map[string]inference.ToolAlias{
			"calendar__create":        {Kind: "function", Name: "create", Namespace: "calendar__"},
			"grokcli2api_custom_code": {Kind: "custom", Name: "code", Namespace: "shell__"},
			"grokcli2api_tool_search": {Kind: "tool_search", Name: "tool_search", Execution: "client"},
		},
	}
	payload := map[string]any{
		"id": "resp_1", "object": "response", "status": "completed", "output": []any{
			map[string]any{"type": "function_call", "call_id": "call-1", "name": "calendar__create", "arguments": `{"title":"demo"}`},
			map[string]any{"type": "function_call", "call_id": "call-2", "name": "grokcli2api_custom_code", "arguments": `{"input":"print(1)"}`},
			map[string]any{"type": "function_call", "call_id": "call-3", "name": "grokcli2api_tool_search", "arguments": `{"goal":"calendar"}`},
		},
	}
	response := adaptBackendResponse(adapter, payload, "grok-public", anthropic.ResponseOptions{})
	output, _ := response["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("output = %#v", output)
	}
	namespaced := output[0].(map[string]any)
	if namespaced["type"] != "function_call" || namespaced["name"] != "create" || namespaced["namespace"] != "calendar__" {
		t.Fatalf("namespaced = %#v", namespaced)
	}
	custom := output[1].(map[string]any)
	if custom["type"] != "custom_tool_call" || custom["name"] != "code" || custom["namespace"] != "shell__" || custom["input"] != "print(1)" {
		t.Fatalf("custom = %#v", custom)
	}
	if _, exists := custom["arguments"]; exists {
		t.Fatalf("custom arguments leaked: %#v", custom)
	}
	search := output[2].(map[string]any)
	arguments, _ := search["arguments"].(map[string]any)
	if search["type"] != "tool_search_call" || search["execution"] != "client" || arguments["goal"] != "calendar" {
		t.Fatalf("search = %#v", search)
	}
	if _, exists := search["name"]; exists {
		t.Fatalf("search wire alias leaked: %#v", search)
	}
}

func TestBackendAdapterRestoresExtendedResponsesToolsAfterBackendSwitch(t *testing.T) {
	adapter := inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendChatCompletions,
		ToolAliases: map[string]inference.ToolAlias{
			"calendar__create": {Kind: "function", Name: "create", Namespace: "calendar__"},
			"shell":            {Kind: "custom", Name: "shell"},
		},
	}
	payload := map[string]any{
		"id": "chatcmpl_1", "choices": []any{map[string]any{
			"finish_reason": "tool_calls", "message": map[string]any{"tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "calendar__create", "arguments": `{}`}},
				map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "shell", "arguments": `{"input":"echo hi"}`}},
			}},
		}},
	}
	response := adaptBackendResponse(adapter, payload, "grok-public", anthropic.ResponseOptions{})
	output := response["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%#v", output)
	}
	namespaced := output[0].(map[string]any)
	custom := output[1].(map[string]any)
	if namespaced["name"] != "create" || namespaced["namespace"] != "calendar__" ||
		custom["type"] != "custom_tool_call" || custom["name"] != "shell" || custom["input"] != "echo hi" {
		t.Fatalf("output=%#v", output)
	}
}

func TestAliasedCustomInputPreservesEmptyString(t *testing.T) {
	if got := aliasedCustomInput(""); got != "" {
		t.Fatalf("aliasedCustomInput(\"\") = %#v", got)
	}
}

func TestModelContextWindowStopReasonMappings(t *testing.T) {
	result := canonicalResult{StopReason: "model_context_window_exceeded"}
	if got := chatStopReason(result); got != "length" {
		t.Fatalf("chat stop reason = %q", got)
	}
	if got := messagesStopReason(result); got != "model_context_window_exceeded" {
		t.Fatalf("messages stop reason = %q", got)
	}
	response := canonicalResponsesResponse(result, "grok-public")
	details, _ := response["incomplete_details"].(map[string]any)
	if response["status"] != "incomplete" || details["reason"] != "model_context_window_exceeded" {
		t.Fatalf("Responses terminal = %#v", response)
	}
}

func TestBackendAdapterResponsesFailureUsesClientErrorEnvelope(t *testing.T) {
	payload := map[string]any{
		"id": "resp_failed", "object": "response", "status": "failed",
		"error": map[string]any{"code": "server_error", "message": "generation failed"},
	}
	tests := []struct {
		name       string
		protocol   inference.Protocol
		wantType   string
		wantErrKey string
	}{
		{name: "chat", protocol: inference.ProtocolChatCompletions, wantType: "upstream_error", wantErrKey: "error"},
		{name: "messages", protocol: inference.ProtocolMessages, wantType: "api_error", wantErrKey: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := adaptBackendResponse(inference.ResponseAdapter{
				ClientProtocol: test.protocol, UpstreamBackend: modelcatalog.BackendResponses,
			}, payload, "grok-public", anthropic.ResponseOptions{})
			if test.protocol == inference.ProtocolMessages && response["type"] != "error" {
				t.Fatalf("response type = %#v; response=%#v", response["type"], response)
			}
			failure, _ := response[test.wantErrKey].(map[string]any)
			if failure["type"] != test.wantType || failure["message"] != "generation failed" {
				t.Fatalf("failure = %#v; response=%#v", failure, response)
			}
			if _, exists := response["choices"]; exists {
				t.Fatalf("failed Responses payload became a Chat success: %#v", response)
			}
			if _, exists := response["content"]; exists {
				t.Fatalf("failed Responses payload became a Messages success: %#v", response)
			}
		})
	}
}

func TestBackendAdapterNativeResponsesPreservesFailedStatus(t *testing.T) {
	payload := map[string]any{"id": "resp_failed", "status": "failed", "error": map[string]any{"message": "generation failed"}}
	response := adaptBackendResponse(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendResponses,
	}, payload, "grok-public", anthropic.ResponseOptions{})
	if response["status"] != "failed" {
		t.Fatalf("response = %#v", response)
	}
}
