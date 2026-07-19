package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Futureppo/grokcli2api-go/internal/anthropic"
	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/inference"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

func TestBackendStreamNativeChatAddsAssistantRole(t *testing.T) {
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolChatCompletions, UpstreamBackend: modelcatalog.BackendChatCompletions,
	}, "grok-public")
	events, err := adapter.Handle(grok.SSEEvent{Data: []byte(`{"id":"chat-1","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	payload := decodeBackendStreamPayload(t, events[0])
	choices, _ := payload["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if delta["role"] != "assistant" || delta["content"] != "hello" {
		t.Fatalf("delta = %#v", delta)
	}
	if payload["model"] != "grok-public" {
		t.Fatalf("model = %#v", payload["model"])
	}
}

func TestBackendStreamChatTerminalSemantics(t *testing.T) {
	t.Run("explicit done", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolChatCompletions, UpstreamBackend: modelcatalog.BackendChatCompletions,
		}, "grok-public")
		events, err := adapter.Handle(grok.SSEEvent{Data: []byte("[DONE]")})
		if err != nil || len(events) != 0 || !adapter.Terminal() || !adapter.Success() {
			t.Fatalf("events=%#v terminal=%t success=%t err=%v", events, adapter.Terminal(), adapter.Success(), err)
		}
	})

	t.Run("finish reason then EOF", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolChatCompletions, UpstreamBackend: modelcatalog.BackendChatCompletions,
		}, "grok-public")
		_, err := adapter.Handle(grok.SSEEvent{Data: []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)})
		if err != nil {
			t.Fatal(err)
		}
		events := adapter.Finish()
		if !adapter.Terminal() || !adapter.Success() || len(events) != 1 || string(events[0].Data) != "[DONE]" {
			t.Fatalf("events=%#v terminal=%t success=%t", events, adapter.Terminal(), adapter.Success())
		}
	})

	t.Run("premature EOF", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolChatCompletions, UpstreamBackend: modelcatalog.BackendChatCompletions,
		}, "grok-public")
		_, err := adapter.Handle(grok.SSEEvent{Data: []byte(`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`)})
		if err != nil {
			t.Fatal(err)
		}
		events := adapter.Finish()
		if !adapter.Terminal() || adapter.Success() || len(events) != 1 || string(events[0].Data) == "[DONE]" {
			t.Fatalf("events=%#v terminal=%t success=%t", events, adapter.Terminal(), adapter.Success())
		}
		if !strings.Contains(string(events[0].Data), "upstream_stream_incomplete") {
			t.Fatalf("error event = %s", events[0].Data)
		}
	})

	t.Run("upstream error", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolChatCompletions, UpstreamBackend: modelcatalog.BackendChatCompletions,
		}, "grok-public")
		events, err := adapter.Handle(grok.SSEEvent{Data: []byte(`{"error":{"message":"boom"}}`)})
		if err != nil || !adapter.Terminal() || adapter.Success() || len(events) != 1 {
			t.Fatalf("events=%#v terminal=%t success=%t err=%v", events, adapter.Terminal(), adapter.Success(), err)
		}
		if !strings.Contains(string(events[0].Data), "boom") {
			t.Fatalf("error event = %s", events[0].Data)
		}
	})
}

func TestBackendStreamResponsesTerminalSemantics(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		success bool
	}{
		{name: "completed", kind: "response.completed", success: true},
		{name: "incomplete", kind: "response.incomplete", success: true},
		{name: "failed", kind: "response.failed"},
		{name: "error", kind: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := newBackendStreamAdapter(inference.ResponseAdapter{
				ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendResponses,
			}, "grok-public")
			payload := map[string]any{"type": test.kind}
			if test.kind == "error" {
				payload["error"] = map[string]any{"message": "boom"}
			} else {
				payload["response"] = map[string]any{"id": "resp_1", "status": strings.TrimPrefix(test.kind, "response.")}
			}
			events, err := adapter.Handle(grok.SSEEvent{Event: test.kind, Data: mustJSON(payload)})
			if err != nil || len(events) != 1 || !adapter.Terminal() || adapter.Success() != test.success {
				t.Fatalf("events=%#v terminal=%t success=%t err=%v", events, adapter.Terminal(), adapter.Success(), err)
			}
			if events[0].Event != test.kind {
				t.Fatalf("event = %q, want %q", events[0].Event, test.kind)
			}
		})
	}

	t.Run("done is not terminal", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendResponses,
		}, "grok-public")
		events, err := adapter.Handle(grok.SSEEvent{Data: []byte("[DONE]")})
		if err != nil || len(events) != 0 || adapter.Terminal() {
			t.Fatalf("events=%#v terminal=%t err=%v", events, adapter.Terminal(), err)
		}
		events = adapter.Finish()
		if adapter.Success() || len(events) != 1 || events[0].Event != "error" || strings.Contains(string(events[0].Data), "[DONE]") {
			t.Fatalf("events=%#v success=%t", events, adapter.Success())
		}
	})
}

func TestBackendStreamMessagesTerminalSemantics(t *testing.T) {
	t.Run("message stop", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolMessages, UpstreamBackend: modelcatalog.BackendMessages,
		}, "grok-public")
		events, err := adapter.Handle(grok.SSEEvent{Event: "message_stop", Data: []byte(`{"type":"message_stop"}`)})
		if err != nil || len(events) != 1 || !adapter.Terminal() || !adapter.Success() {
			t.Fatalf("events=%#v terminal=%t success=%t err=%v", events, adapter.Terminal(), adapter.Success(), err)
		}
	})

	t.Run("error", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolMessages, UpstreamBackend: modelcatalog.BackendMessages,
		}, "grok-public")
		events, err := adapter.Handle(grok.SSEEvent{Event: "error", Data: []byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`)})
		if err != nil || len(events) != 1 || !adapter.Terminal() || adapter.Success() || events[0].Event != "error" {
			t.Fatalf("events=%#v terminal=%t success=%t err=%v", events, adapter.Terminal(), adapter.Success(), err)
		}
	})

	t.Run("premature EOF", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolMessages, UpstreamBackend: modelcatalog.BackendMessages,
		}, "grok-public")
		_, err := adapter.Handle(grok.SSEEvent{Event: "message_start", Data: []byte(`{"type":"message_start","message":{"id":"msg_1"}}`)})
		if err != nil {
			t.Fatal(err)
		}
		events := adapter.Finish()
		if len(events) != 1 || events[0].Event != "error" || adapter.Success() {
			t.Fatalf("events=%#v success=%t", events, adapter.Success())
		}
		payload := decodeBackendStreamPayload(t, events[0])
		if payload["type"] != "error" {
			t.Fatalf("payload = %#v", payload)
		}
	})
}

func TestBackendStreamThreeByThreeSuccessContract(t *testing.T) {
	upstreams := map[modelcatalog.Backend][]grok.SSEEvent{
		modelcatalog.BackendChatCompletions: {
			{Data: []byte(`{"id":"chat_1","model":"wire-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"matrix-ok"},"finish_reason":null}]}`)},
			{Data: []byte(`{"id":"chat_1","model":"wire-chat","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)},
			{Data: []byte("[DONE]")},
		},
		modelcatalog.BackendResponses: {
			{Event: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"wire-responses","output":[]}}`)},
			{Event: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_1","content_index":0,"delta":"matrix-ok"}`)},
			{Event: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"wire-responses","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"matrix-ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)},
		},
		modelcatalog.BackendMessages: {
			{Event: "message_start", Data: []byte(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"wire-messages","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)},
			{Event: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)},
			{Event: "content_block_delta", Data: []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"matrix-ok"}}`)},
			{Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0}`)},
			{Event: "message_delta", Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`)},
			{Event: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
		},
	}
	protocols := []inference.Protocol{
		inference.ProtocolChatCompletions,
		inference.ProtocolResponses,
		inference.ProtocolMessages,
	}
	for backend, inputs := range upstreams {
		for _, protocol := range protocols {
			t.Run(string(backend)+"/to/"+string(protocol), func(t *testing.T) {
				adapter := newBackendStreamAdapter(inference.ResponseAdapter{
					ClientProtocol: protocol, UpstreamBackend: backend,
				}, "public-model")
				var output []grok.SSEEvent
				for _, input := range inputs {
					events, err := adapter.Handle(input)
					if err != nil {
						t.Fatal(err)
					}
					output = append(output, events...)
				}
				if !adapter.Terminal() || !adapter.Success() {
					t.Fatalf("terminal=%t success=%t output=%#v", adapter.Terminal(), adapter.Success(), output)
				}
				var encoded strings.Builder
				for _, event := range output {
					encoded.WriteString(event.Event)
					encoded.Write(event.Data)
				}
				if !strings.Contains(encoded.String(), "matrix-ok") {
					t.Fatalf("stream lost text: %s", encoded.String())
				}
				switch protocol {
				case inference.ProtocolChatCompletions:
					if !strings.Contains(encoded.String(), `"choices"`) {
						t.Fatalf("chat chunks missing: %s", encoded.String())
					}
				case inference.ProtocolResponses:
					if !strings.Contains(encoded.String(), "response.completed") {
						t.Fatalf("Responses terminal missing: %s", encoded.String())
					}
				case inference.ProtocolMessages:
					if !strings.Contains(encoded.String(), "message_stop") {
						t.Fatalf("Messages terminal missing: %s", encoded.String())
					}
				}
			})
		}
	}
}

func TestBackendStreamResponsesToMessagesAppliesResponseOptions(t *testing.T) {
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolMessages, UpstreamBackend: modelcatalog.BackendResponses,
	}, "grok-public", anthropic.ResponseOptions{StopSequences: []string{"STOP"}})

	inputs := []grok.SSEEvent{
		{Event: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_1"}}`)},
		{Event: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","item":{"id":"reasoning_1","type":"reasoning"}}`)},
		{Event: "response.reasoning_summary_text.delta", Data: []byte(`{"type":"response.reasoning_summary_text.delta","item_id":"reasoning_1","delta":"hidden"}`)},
		{Event: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","item_id":"msg_1","delta":"ABCST"}`)},
		{Event: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","item_id":"msg_1","delta":"OPXYZ"}`)},
		{Event: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_1"}}`)},
	}
	var encoded strings.Builder
	for _, input := range inputs {
		events, err := adapter.Handle(input)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			encoded.WriteString(event.Event)
			encoded.Write(event.Data)
		}
	}
	text := encoded.String()
	if !adapter.Terminal() || !adapter.Success() {
		t.Fatalf("terminal=%t success=%t", adapter.Terminal(), adapter.Success())
	}
	if strings.Contains(text, "hidden") || strings.Contains(text, "XYZ") {
		t.Fatalf("filtered content leaked: %s", text)
	}
	for _, expected := range []string{"ABC", "stop_sequence", "STOP", "message_stop", "msg_resp_1"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("%q missing: %s", expected, text)
		}
	}
}

func TestBackendStreamResponsesToMessagesFailureIsError(t *testing.T) {
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolMessages, UpstreamBackend: modelcatalog.BackendResponses,
	}, "grok-public")
	events, err := adapter.Handle(grok.SSEEvent{Event: "response.failed", Data: []byte(`{"type":"response.failed","response":{"error":{"message":"boom"}}}`)})
	if err != nil || len(events) != 1 || events[0].Event != "error" || !adapter.Terminal() || adapter.Success() {
		t.Fatalf("events=%#v terminal=%t success=%t err=%v", events, adapter.Terminal(), adapter.Success(), err)
	}
	if !strings.Contains(string(events[0].Data), "boom") {
		t.Fatalf("error = %s", events[0].Data)
	}
}

func TestBackendStreamMessagesBlockStopOnlyCompletesTools(t *testing.T) {
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendMessages,
	}, "grok-public")
	if _, err := adapter.Handle(grok.SSEEvent{Event: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`)}); err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Handle(grok.SSEEvent{Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0}`)})
	if err != nil || len(events) != 0 {
		t.Fatalf("text stop events=%#v err=%v", events, err)
	}
	if _, err := adapter.Handle(grok.SSEEvent{Event: "content_block_start", Data: []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{}}}`)}); err != nil {
		t.Fatal(err)
	}
	events, err = adapter.Handle(grok.SSEEvent{Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":1}`)})
	if err != nil || len(events) == 0 {
		t.Fatalf("tool stop events=%#v err=%v", events, err)
	}
	foundDone := false
	for _, event := range events {
		if event.Event == "response.function_call_arguments.done" {
			foundDone = true
		}
	}
	if !foundDone {
		t.Fatalf("tool completion missing: %#v", events)
	}
}

func TestBackendStreamResponsesToChatUsesCompleteToolArguments(t *testing.T) {
	for _, test := range []struct {
		name      string
		withDelta bool
	}{
		{name: "arguments only on done"},
		{name: "done does not duplicate deltas", withDelta: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := newBackendStreamAdapter(inference.ResponseAdapter{
				ClientProtocol: inference.ProtocolChatCompletions, UpstreamBackend: modelcatalog.BackendResponses,
			}, "grok-public")
			inputs := []grok.SSEEvent{{Event: "response.output_item.added", Data: mustJSON(map[string]any{
				"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": ""},
			})}}
			if test.withDelta {
				inputs = append(inputs, grok.SSEEvent{Event: "response.function_call_arguments.delta", Data: mustJSON(map[string]any{
					"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": "fc_1", "delta": `{"city":"Paris"}`,
				})})
			}
			inputs = append(inputs, grok.SSEEvent{Event: "response.output_item.done", Data: mustJSON(map[string]any{
				"type": "response.output_item.done", "output_index": 0,
				"item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": `{"city":"Paris"}`},
			})})

			var arguments strings.Builder
			for _, input := range inputs {
				events, err := adapter.Handle(input)
				if err != nil {
					t.Fatal(err)
				}
				appendChatToolArguments(t, &arguments, events)
			}
			if arguments.String() != `{"city":"Paris"}` {
				t.Fatalf("concatenated arguments = %q", arguments.String())
			}
		})
	}
}

func TestBackendStreamToolCallsUseProtocolTerminalReasons(t *testing.T) {
	t.Run("responses to chat", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolChatCompletions, UpstreamBackend: modelcatalog.BackendResponses,
		}, "grok-public")
		inputs := []grok.SSEEvent{
			{Event: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`)},
			{Event: "response.completed", Data: []byte(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`)},
		}
		var terminal map[string]any
		for _, input := range inputs {
			events, err := adapter.Handle(input)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				payload := decodeBackendStreamPayload(t, event)
				choices, _ := payload["choices"].([]any)
				if len(choices) > 0 && choices[0].(map[string]any)["finish_reason"] != nil {
					terminal = choices[0].(map[string]any)
				}
			}
		}
		if terminal == nil || terminal["finish_reason"] != "tool_calls" {
			t.Fatalf("terminal=%#v", terminal)
		}
	})

	t.Run("chat to messages", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolMessages, UpstreamBackend: modelcatalog.BackendChatCompletions,
		}, "grok-public")
		inputs := []grok.SSEEvent{
			{Data: []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`)},
			{Data: []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
			{Data: []byte("[DONE]")},
		}
		stop := ""
		for _, input := range inputs {
			events, err := adapter.Handle(input)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Event != "message_delta" {
					continue
				}
				payload := decodeBackendStreamPayload(t, event)
				delta, _ := payload["delta"].(map[string]any)
				stop = stringAt(delta, "stop_reason")
			}
		}
		if stop != "tool_use" {
			t.Fatalf("stop_reason=%q", stop)
		}
	})

	t.Run("messages to chat", func(t *testing.T) {
		adapter := newBackendStreamAdapter(inference.ResponseAdapter{
			ClientProtocol: inference.ProtocolChatCompletions, UpstreamBackend: modelcatalog.BackendMessages,
		}, "grok-public")
		inputs := []grok.SSEEvent{
			{Event: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{}}}`)},
			{Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0}`)},
			{Event: "message_delta", Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`)},
			{Event: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
		}
		stop := ""
		for _, input := range inputs {
			events, err := adapter.Handle(input)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				payload := decodeBackendStreamPayload(t, event)
				choices, _ := payload["choices"].([]any)
				if len(choices) > 0 {
					stop = stringAt(choices[0].(map[string]any), "finish_reason")
				}
			}
		}
		if stop != "tool_calls" {
			t.Fatalf("finish_reason=%q", stop)
		}
	})
}

func TestBackendStreamMessagesToolInputReachesChat(t *testing.T) {
	for _, test := range []struct {
		name   string
		inputs []grok.SSEEvent
	}{
		{
			name: "complete input on block start",
			inputs: []grok.SSEEvent{
				{Event: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{"city":"Paris"}}}`)},
				{Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0}`)},
			},
		},
		{
			name: "empty start input does not prefix streamed input",
			inputs: []grok.SSEEvent{
				{Event: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{}}}`)},
				{Event: "content_block_delta", Data: []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Paris\"}"}}`)},
				{Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0}`)},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := newBackendStreamAdapter(inference.ResponseAdapter{
				ClientProtocol: inference.ProtocolChatCompletions, UpstreamBackend: modelcatalog.BackendMessages,
			}, "grok-public")
			var arguments strings.Builder
			for _, input := range test.inputs {
				events, err := adapter.Handle(input)
				if err != nil {
					t.Fatal(err)
				}
				appendChatToolArguments(t, &arguments, events)
			}
			if arguments.String() != `{"city":"Paris"}` {
				t.Fatalf("concatenated arguments = %q", arguments.String())
			}
		})
	}
}

func TestBackendStreamMessagesStartToolInputReachesResponses(t *testing.T) {
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendMessages,
	}, "grok-public")
	inputs := []grok.SSEEvent{
		{Event: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{"city":"Paris"}}}`)},
		{Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0}`)},
	}
	found := ""
	for _, input := range inputs {
		events, err := adapter.Handle(input)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Event == "response.function_call_arguments.done" {
				found = stringAt(decodeBackendStreamPayload(t, event), "arguments")
			}
		}
	}
	if found != `{"city":"Paris"}` {
		t.Fatalf("done arguments = %q", found)
	}
}

func appendChatToolArguments(t *testing.T, target *strings.Builder, events []grok.SSEEvent) {
	t.Helper()
	for _, event := range events {
		payload := decodeBackendStreamPayload(t, event)
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			target.WriteString(stringAt(function, "arguments"))
		}
	}
}

func TestBackendStreamChatToMessagesFiltersReasoningByThinkingOption(t *testing.T) {
	input := grok.SSEEvent{Data: []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"hidden"}}]}`)}
	for _, test := range []struct {
		name    string
		enabled bool
		want    bool
	}{
		{name: "disabled by default"},
		{name: "enabled", enabled: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := newBackendStreamAdapter(inference.ResponseAdapter{
				ClientProtocol: inference.ProtocolMessages, UpstreamBackend: modelcatalog.BackendChatCompletions,
			}, "grok-public", anthropic.ResponseOptions{ThinkingEnabled: test.enabled})
			events, err := adapter.Handle(input)
			if err != nil {
				t.Fatal(err)
			}
			encoded := ""
			for _, event := range events {
				encoded += string(event.Data)
			}
			if strings.Contains(encoded, "hidden") != test.want || strings.Contains(encoded, "thinking_delta") != test.want {
				t.Fatalf("events=%#v", events)
			}
		})
	}
}

func TestBackendStreamChatToResponsesCompletesOutputItems(t *testing.T) {
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendChatCompletions,
	}, "grok-public")
	inputs := []grok.SSEEvent{
		{Data: []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`)},
		{Data: []byte(`{"choices":[{"index":0,"delta":{"content":"hello"}}]}`)},
		{Data: []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}}]}`)},
		{Data: []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
		{Data: []byte("[DONE]")},
	}
	var events []grok.SSEEvent
	for _, input := range inputs {
		out, err := adapter.Handle(input)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, out...)
	}
	if !adapter.Terminal() || !adapter.Success() {
		t.Fatalf("terminal=%t success=%t", adapter.Terminal(), adapter.Success())
	}
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Event]++
	}
	for _, kind := range []string{
		"response.output_text.done", "response.reasoning_summary_text.done",
		"response.function_call_arguments.done", "response.output_item.done", "response.completed",
	} {
		if counts[kind] == 0 {
			t.Fatalf("%s missing; counts=%#v", kind, counts)
		}
	}
	if counts["response.output_item.done"] != 3 {
		t.Fatalf("output_item.done count=%d events=%#v", counts["response.output_item.done"], events)
	}
	completed := decodeBackendStreamPayload(t, events[len(events)-1])
	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("terminal output=%#v", output)
	}
	encoded, _ := json.Marshal(output)
	for _, expected := range []string{"think", "hello", "lookup", `\"q\":\"x\"`} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("%q missing from terminal output: %s", expected, encoded)
		}
	}
}

func TestBackendStreamChatBuffersFragmentedToolStart(t *testing.T) {
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendChatCompletions,
	}, "grok-public")
	inputs := []grok.SSEEvent{
		{Data: []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"arguments":"{\"city\":"}}]}}]}`)},
		{Data: []byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"lookup","arguments":"\"Paris\"}"}}]}}]}`)},
		{Data: []byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
		{Data: []byte("[DONE]")},
	}
	var events []grok.SSEEvent
	for _, input := range inputs {
		out, err := adapter.Handle(input)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, out...)
	}
	added, arguments := 0, ""
	for _, event := range events {
		payload := decodeBackendStreamPayload(t, event)
		switch event.Event {
		case "response.output_item.added":
			item, _ := payload["item"].(map[string]any)
			if item["type"] == "function_call" {
				added++
				if item["call_id"] != "call_1" || item["name"] != "lookup" {
					t.Fatalf("added item=%#v", item)
				}
			}
		case "response.function_call_arguments.done":
			arguments = stringAt(payload, "arguments")
		}
	}
	if added != 1 || arguments != `{"city":"Paris"}` {
		t.Fatalf("added=%d arguments=%q events=%#v", added, arguments, events)
	}
}

func TestBackendStreamMessagesToResponsesCompletesTextAtTerminal(t *testing.T) {
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendMessages,
	}, "grok-public")
	inputs := []grok.SSEEvent{
		{Event: "message_start", Data: []byte(`{"type":"message_start","message":{"usage":{"input_tokens":2}}}`)},
		{Event: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"he"}}`)},
		{Event: "content_block_delta", Data: []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"llo"}}`)},
		{Event: "content_block_stop", Data: []byte(`{"type":"content_block_stop","index":0}`)},
		{Event: "message_delta", Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)},
		{Event: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
	}
	var events []grok.SSEEvent
	for _, input := range inputs {
		out, err := adapter.Handle(input)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, out...)
	}
	if !adapter.Success() || len(events) == 0 || events[len(events)-1].Event != "response.completed" {
		t.Fatalf("events=%#v success=%t", events, adapter.Success())
	}
	completed := decodeBackendStreamPayload(t, events[len(events)-1])
	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	encoded, _ := json.Marshal(output)
	if !strings.Contains(string(encoded), "hello") {
		t.Fatalf("terminal output=%s", encoded)
	}
}

func TestBackendStreamNativeEventAllowlists(t *testing.T) {
	if !knownNativeResponsesEvent("response.doom_loop_check") {
		t.Fatal("response.doom_loop_check must be preserved on the native Responses wire")
	}
	for _, test := range []struct {
		name     string
		protocol inference.Protocol
		backend  modelcatalog.Backend
		event    grok.SSEEvent
	}{
		{
			name: "responses", protocol: inference.ProtocolResponses, backend: modelcatalog.BackendResponses,
			event: grok.SSEEvent{Event: "response.future.delta", Data: []byte(`{"type":"response.future.delta","value":1}`)},
		},
		{
			name: "messages", protocol: inference.ProtocolMessages, backend: modelcatalog.BackendMessages,
			event: grok.SSEEvent{Event: "message_future", Data: []byte(`{"type":"message_future","value":1}`)},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ordinary := newBackendStreamAdapter(inference.ResponseAdapter{ClientProtocol: test.protocol, UpstreamBackend: test.backend}, "grok-public")
			events, err := ordinary.Handle(test.event)
			if err != nil || len(events) != 0 {
				t.Fatalf("ordinary events=%#v err=%v", events, err)
			}
			native := newBackendStreamAdapter(inference.ResponseAdapter{ClientProtocol: test.protocol, UpstreamBackend: test.backend, NativeCLI: true}, "grok-public")
			events, err = native.Handle(test.event)
			if err != nil || len(events) != 1 || events[0].Event != test.event.Event {
				t.Fatalf("native events=%#v err=%v", events, err)
			}
		})
	}
}

func TestBackendStreamMessagesToResponsesKeepsDeferredContextStop(t *testing.T) {
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendMessages,
	}, "grok-public")
	if _, err := adapter.Handle(grok.SSEEvent{Event: "message_delta", Data: mustJSON(map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "model_context_window_exceeded"},
		"usage": map[string]any{"output_tokens": 1},
	})}); err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Handle(grok.SSEEvent{Event: "message_stop", Data: mustJSON(map[string]any{"type": "message_stop"})})
	if err != nil || len(events) == 0 || !adapter.Success() {
		t.Fatalf("events=%#v success=%t err=%v", events, adapter.Success(), err)
	}
	terminal := decodeBackendStreamPayload(t, events[len(events)-1])
	response, _ := terminal["response"].(map[string]any)
	details, _ := response["incomplete_details"].(map[string]any)
	if terminal["type"] != "response.incomplete" || details["reason"] != "model_context_window_exceeded" {
		t.Fatalf("terminal=%#v", terminal)
	}
}

func TestBackendStreamNativeResponsesRestoresToolAliases(t *testing.T) {
	aliases := map[string]inference.ToolAlias{
		"calendar__create":        {Kind: "function", Name: "create", Namespace: "calendar__"},
		"grokcli2api_custom_code": {Kind: "custom", Name: "code"},
		"grokcli2api_tool_search": {Kind: "tool_search", Name: "tool_search", Execution: "client"},
	}
	adapter := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendResponses, ToolAliases: aliases,
	}, "grok-public")

	namespaced := handleOneBackendStreamEvent(t, adapter, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"id": "item-ns", "type": "function_call", "call_id": "call-ns", "name": "calendar__create", "arguments": ""},
	})
	item := namespaced["item"].(map[string]any)
	if item["name"] != "create" || item["namespace"] != "calendar__" {
		t.Fatalf("namespaced item = %#v", item)
	}

	custom := handleOneBackendStreamEvent(t, adapter, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 1,
		"item": map[string]any{"id": "item-custom", "type": "function_call", "call_id": "call-custom", "name": "grokcli2api_custom_code", "arguments": ""},
	})
	item = custom["item"].(map[string]any)
	if item["type"] != "custom_tool_call" || item["name"] != "code" || item["input"] != "" {
		t.Fatalf("custom item = %#v", item)
	}
	if events, err := adapter.Handle(grok.SSEEvent{Event: "response.function_call_arguments.delta", Data: mustJSON(map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": "item-custom", "output_index": 1, "delta": `{"input":"hello"}`,
	})}); err != nil || len(events) != 0 {
		t.Fatalf("custom delta events=%#v err=%v", events, err)
	}
	events, err := adapter.Handle(grok.SSEEvent{Event: "response.function_call_arguments.done", Data: mustJSON(map[string]any{
		"type": "response.function_call_arguments.done", "item_id": "item-custom", "output_index": 1, "arguments": `{"input":"hello"}`,
	})})
	if err != nil || len(events) != 2 || events[0].Event != "response.custom_tool_call_input.delta" || events[1].Event != "response.custom_tool_call_input.done" {
		t.Fatalf("custom done events=%#v err=%v", events, err)
	}
	done := decodeBackendStreamPayload(t, events[1])
	if done["input"] != "hello" {
		t.Fatalf("custom done = %#v", done)
	}

	search := handleOneBackendStreamEvent(t, adapter, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 2,
		"item": map[string]any{"id": "item-search", "type": "function_call", "call_id": "call-search", "name": "grokcli2api_tool_search", "arguments": ""},
	})
	item = search["item"].(map[string]any)
	if item["type"] != "tool_search_call" || item["execution"] != "client" {
		t.Fatalf("search item = %#v", item)
	}
	for _, kind := range []string{"response.function_call_arguments.delta", "response.function_call_arguments.done"} {
		payload := map[string]any{"type": kind, "item_id": "item-search", "output_index": 2}
		if strings.HasSuffix(kind, ".delta") {
			payload["delta"] = `{"goal":"calendar"}`
		} else {
			payload["arguments"] = `{"goal":"calendar"}`
		}
		if events, err := adapter.Handle(grok.SSEEvent{Event: kind, Data: mustJSON(payload)}); err != nil || len(events) != 0 {
			t.Fatalf("%s events=%#v err=%v", kind, events, err)
		}
	}
	searchDone := handleOneBackendStreamEvent(t, adapter, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 2,
		"item": map[string]any{"id": "item-search", "type": "function_call", "call_id": "call-search", "name": "grokcli2api_tool_search", "arguments": `{"goal":"calendar"}`},
	})
	item = searchDone["item"].(map[string]any)
	arguments, _ := item["arguments"].(map[string]any)
	if item["type"] != "tool_search_call" || arguments["goal"] != "calendar" {
		t.Fatalf("search done item = %#v", item)
	}

	completed := handleOneBackendStreamEvent(t, adapter, "response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{"id": "resp_1", "status": "completed", "output": []any{
			map[string]any{"id": "item-ns", "type": "function_call", "call_id": "call-ns", "name": "calendar__create", "arguments": `{}`},
			map[string]any{"id": "item-custom", "type": "function_call", "call_id": "call-custom", "name": "grokcli2api_custom_code", "arguments": `{"input":"hello"}`},
			map[string]any{"id": "item-search", "type": "function_call", "call_id": "call-search", "name": "grokcli2api_tool_search", "arguments": `{"goal":"calendar"}`},
		}},
	})
	response := completed["response"].(map[string]any)
	output := response["output"].([]any)
	if output[0].(map[string]any)["namespace"] != "calendar__" || output[1].(map[string]any)["type"] != "custom_tool_call" || output[2].(map[string]any)["type"] != "tool_search_call" {
		t.Fatalf("completed output = %#v", output)
	}
}

func handleOneBackendStreamEvent(t *testing.T, adapter *backendStreamAdapter, event string, payload map[string]any) map[string]any {
	t.Helper()
	events, err := adapter.Handle(grok.SSEEvent{Event: event, Data: mustJSON(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("%s events = %#v", event, events)
	}
	return decodeBackendStreamPayload(t, events[0])
}

func decodeBackendStreamPayload(t *testing.T, event grok.SSEEvent) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatalf("decode %q: %v", event.Data, err)
	}
	return payload
}
