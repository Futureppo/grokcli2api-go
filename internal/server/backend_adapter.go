package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/anthropic"
	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/inference"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
	"github.com/Futureppo/grokcli2api-go/internal/openai"
)

type canonicalToolCall struct {
	ID        string
	Name      string
	Kind      string
	Namespace string
	Execution string
	Arguments any
}

type canonicalUsage struct {
	Input, Output, Cached, Reasoning int64
}

type canonicalResult struct {
	ID         string
	Text       string
	Reasoning  string
	Tools      []canonicalToolCall
	Usage      canonicalUsage
	StopReason string
	Status     string
}

func adaptBackendResponse(adapter inference.ResponseAdapter, payload map[string]any, publicModel string, anthropicOptions anthropic.ResponseOptions) map[string]any {
	// A Responses server can report an application-level failure in an HTTP
	// 2xx response. Cross-protocol adapters must not turn that payload into an
	// empty, successful Chat completion or Anthropic message.
	if adapter.UpstreamBackend == modelcatalog.BackendResponses && adapter.ClientProtocol != inference.ProtocolResponses && responsesPayloadFailed(payload) {
		message := responsesPayloadFailureMessage(payload)
		if adapter.ClientProtocol == inference.ProtocolMessages {
			return anthropic.Error(message, "api_error")
		}
		return openai.Error(message, "upstream_error", "upstream_response_failed")
	}

	// Keep native responses on their native normalization paths so additive
	// fields introduced by a newer upstream are not needlessly erased.
	switch {
	case adapter.ClientProtocol == inference.ProtocolChatCompletions && adapter.UpstreamBackend == modelcatalog.BackendChatCompletions:
		return openai.NormalizeChat(payload, publicModel, false)
	case adapter.ClientProtocol == inference.ProtocolResponses && adapter.UpstreamBackend == modelcatalog.BackendResponses:
		if adapter.NativeCLI {
			return restoreResponsesPayloadAliases(payload, adapter.ToolAliases)
		}
		return restoreResponsesPayloadAliases(openai.NormalizeResponse(payload, publicModel), adapter.ToolAliases)
	case adapter.ClientProtocol == inference.ProtocolMessages && adapter.UpstreamBackend == modelcatalog.BackendResponses:
		return anthropic.NormalizeResponseWithOptions(payload, publicModel, anthropicOptions)
	case adapter.ClientProtocol == inference.ProtocolMessages && adapter.UpstreamBackend == modelcatalog.BackendMessages:
		return normalizeNativeMessagesResponse(payload, publicModel)
	}

	canonical := decodeCanonicalResponse(adapter.UpstreamBackend, payload)
	canonical = restoreCanonicalToolAliases(canonical, adapter.ToolAliases)
	switch adapter.ClientProtocol {
	case inference.ProtocolChatCompletions:
		return canonicalChatResponse(canonical, publicModel)
	case inference.ProtocolMessages:
		return canonicalMessagesResponse(canonical, publicModel, anthropicOptions)
	default:
		return canonicalResponsesResponse(canonical, publicModel)
	}
}

func responsesPayloadFailed(payload map[string]any) bool {
	if strings.EqualFold(strings.TrimSpace(stringAt(payload, "status")), "failed") {
		return true
	}
	response, _ := payload["response"].(map[string]any)
	return strings.EqualFold(strings.TrimSpace(stringAt(response, "status")), "failed")
}

func responsesPayloadFailureMessage(payload map[string]any) string {
	failure := payload
	if response, ok := payload["response"].(map[string]any); ok && responsesPayloadFailed(response) {
		failure = response
	}
	if raw, exists := failure["error"]; exists {
		switch value := raw.(type) {
		case string:
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		case map[string]any:
			if message := firstNonEmptyString(value, "message", "code"); message != "" {
				return message
			}
		}
	}
	return "upstream response failed"
}

func restoreCanonicalToolAliases(result canonicalResult, aliases map[string]inference.ToolAlias) canonicalResult {
	if len(aliases) == 0 {
		return result
	}
	for index := range result.Tools {
		if alias, ok := aliases[result.Tools[index].Name]; ok {
			if alias.Name != "" {
				result.Tools[index].Name = alias.Name
			}
			result.Tools[index].Kind = alias.Kind
			result.Tools[index].Namespace = alias.Namespace
			result.Tools[index].Execution = alias.Execution
		}
	}
	return result
}

func restoreResponsesPayloadAliases(payload map[string]any, aliases map[string]inference.ToolAlias) map[string]any {
	if len(aliases) == 0 {
		return payload
	}
	out := cloneJSONMap(payload)
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case []any:
			for _, item := range value {
				visit(item)
			}
		case map[string]any:
			if stringAt(value, "type") == "function_call" {
				restoreResponsesCallAlias(value, aliases)
			}
			for _, item := range value {
				visit(item)
			}
		}
	}
	visit(out)
	return out
}

func restoreResponsesCallAlias(call map[string]any, aliases map[string]inference.ToolAlias) (inference.ToolAlias, bool) {
	alias, ok := aliases[stringAt(call, "name")]
	if !ok {
		return inference.ToolAlias{}, false
	}
	switch alias.Kind {
	case "custom":
		call["type"] = "custom_tool_call"
		call["name"] = alias.Name
		call["input"] = aliasedCustomInput(call["arguments"])
		delete(call, "arguments")
	case "tool_search":
		call["type"] = "tool_search_call"
		call["execution"] = alias.Execution
		call["arguments"] = parseArguments(call["arguments"])
		delete(call, "name")
		delete(call, "namespace")
	default:
		if alias.Name != "" {
			call["name"] = alias.Name
		}
		if alias.Namespace != "" {
			call["namespace"] = alias.Namespace
		} else {
			delete(call, "namespace")
		}
	}
	if alias.Kind == "custom" {
		if alias.Namespace != "" {
			call["namespace"] = alias.Namespace
		} else {
			delete(call, "namespace")
		}
	}
	return alias, true
}

func aliasedCustomInput(value any) any {
	if text, ok := value.(string); ok {
		if text == "" {
			return ""
		}
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			if object, ok := decoded.(map[string]any); ok {
				if input, exists := object["input"]; exists {
					return input
				}
			}
			return decoded
		}
		return text
	}
	decoded := parseArguments(value)
	if object, ok := decoded.(map[string]any); ok {
		if input, exists := object["input"]; exists {
			return input
		}
	}
	return decoded
}

func decodeCanonicalResponse(backend modelcatalog.Backend, payload map[string]any) canonicalResult {
	switch backend {
	case modelcatalog.BackendResponses:
		return decodeResponsesResult(payload)
	case modelcatalog.BackendMessages:
		return decodeMessagesResult(payload)
	default:
		return decodeChatResult(payload)
	}
}

func decodeChatResult(payload map[string]any) canonicalResult {
	result := canonicalResult{ID: stringAt(payload, "id"), Status: "completed"}
	choices, _ := payload["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		result.StopReason = stringAt(choice, "finish_reason")
		message, _ := choice["message"].(map[string]any)
		result.Text = flattenText(message["content"])
		result.Reasoning = firstNonEmptyString(message, "reasoning_content", "reasoning", "thinking")
		calls, _ := message["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			name := firstNonEmptyString(function, "name")
			if name == "" {
				continue
			}
			result.Tools = append(result.Tools, canonicalToolCall{
				ID: firstNonEmptyString(call, "id", "call_id"), Name: name,
				Arguments: parseArguments(function["arguments"]),
			})
		}
	}
	usage, _ := payload["usage"].(map[string]any)
	result.Usage = canonicalUsage{
		Input: intAt(usage, "prompt_tokens", "input_tokens"), Output: intAt(usage, "completion_tokens", "output_tokens"),
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		result.Usage.Cached = intAt(details, "cached_tokens")
	}
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		result.Usage.Reasoning = intAt(details, "reasoning_tokens")
	}
	return result
}

func decodeResponsesResult(payload map[string]any) canonicalResult {
	result := canonicalResult{
		ID: stringAt(payload, "id"), Status: firstNonEmptyString(payload, "status"),
	}
	if result.Status == "" {
		result.Status = "completed"
	}
	if incomplete, ok := payload["incomplete_details"].(map[string]any); ok {
		result.StopReason = stringAt(incomplete, "reason")
	}
	output, _ := payload["output"].([]any)
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		switch stringAt(item, "type") {
		case "message":
			result.Text += flattenText(item["content"])
		case "reasoning":
			result.Reasoning += flattenReasoning(item)
		case "function_call", "custom_tool_call":
			name := firstNonEmptyString(item, "name")
			if name != "" {
				result.Tools = append(result.Tools, canonicalToolCall{
					ID: firstNonEmptyString(item, "call_id", "id"), Name: name,
					Arguments: parseArguments(first(item, "arguments", "input")),
				})
			}
		}
	}
	usage, _ := payload["usage"].(map[string]any)
	result.Usage = canonicalUsage{Input: intAt(usage, "input_tokens"), Output: intAt(usage, "output_tokens")}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		result.Usage.Cached = intAt(details, "cached_tokens")
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		result.Usage.Reasoning = intAt(details, "reasoning_tokens")
	}
	return result
}

func decodeMessagesResult(payload map[string]any) canonicalResult {
	result := canonicalResult{
		ID: stringAt(payload, "id"), StopReason: stringAt(payload, "stop_reason"), Status: "completed",
	}
	content, _ := payload["content"].([]any)
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		switch stringAt(block, "type") {
		case "text":
			result.Text += stringAt(block, "text")
		case "thinking":
			result.Reasoning += firstNonEmptyString(block, "thinking", "text")
		case "tool_use":
			name := stringAt(block, "name")
			if name != "" {
				result.Tools = append(result.Tools, canonicalToolCall{
					ID: stringAt(block, "id"), Name: name, Arguments: first(block, "input"),
				})
			}
		}
	}
	usage, _ := payload["usage"].(map[string]any)
	result.Usage = canonicalUsage{
		Input: intAt(usage, "input_tokens"), Output: intAt(usage, "output_tokens"),
		Cached: intAt(usage, "cache_read_input_tokens"),
	}
	return result
}

func canonicalChatResponse(result canonicalResult, model string) map[string]any {
	id := result.ID
	if !strings.HasPrefix(id, "chatcmpl-") {
		id = "chatcmpl-" + stablePublicID(id)
	}
	message := map[string]any{"role": "assistant", "content": nullableText(result.Text)}
	if result.Reasoning != "" {
		message["reasoning_content"] = result.Reasoning
	}
	if len(result.Tools) > 0 {
		calls := make([]any, 0, len(result.Tools))
		for _, tool := range result.Tools {
			calls = append(calls, map[string]any{
				"id": normalizedToolID(tool.ID), "type": "function",
				"function": map[string]any{"name": tool.Name, "arguments": argumentsString(tool.Arguments)},
			})
		}
		message["tool_calls"] = calls
	}
	finish := chatStopReason(result)
	return map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   chatUsage(result.Usage),
	}
}

func canonicalResponsesResponse(result canonicalResult, model string) map[string]any {
	id := result.ID
	if !strings.HasPrefix(id, "resp_") {
		id = "resp_" + stablePublicID(id)
	}
	output := make([]any, 0, len(result.Tools)+2)
	if result.Reasoning != "" {
		output = append(output, map[string]any{
			"id": "rs_" + stablePublicID(id+"reasoning"), "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": result.Reasoning}},
		})
	}
	if result.Text != "" || len(result.Tools) == 0 {
		output = append(output, map[string]any{
			"id": "msg_" + stablePublicID(id+"message"), "type": "message", "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": result.Text, "annotations": []any{}}},
		})
	}
	for _, tool := range result.Tools {
		item := map[string]any{
			"id": "fc_" + stablePublicID(tool.ID), "type": "function_call", "status": "completed",
			"call_id": normalizedToolID(tool.ID), "name": tool.Name, "arguments": argumentsString(tool.Arguments),
		}
		if tool.Namespace != "" {
			item["namespace"] = tool.Namespace
		}
		if tool.Kind == "custom" {
			item["type"] = "custom_tool_call"
			delete(item, "arguments")
			input := tool.Arguments
			if object, ok := input.(map[string]any); ok && object["input"] != nil {
				input = object["input"]
			}
			item["input"] = input
		} else if tool.Kind == "tool_search" {
			item["type"] = "tool_search_call"
			item["execution"] = tool.Execution
			item["arguments"] = normalizeArguments(tool.Arguments)
			delete(item, "name")
			delete(item, "namespace")
		}
		output = append(output, item)
	}
	status := result.Status
	if status == "" {
		status = "completed"
	}
	response := map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status,
		"model": model, "output": output, "usage": responsesUsage(result.Usage),
	}
	if status == "incomplete" || result.StopReason == "max_tokens" || result.StopReason == "max_output_tokens" || result.StopReason == "model_context_window_exceeded" {
		response["status"] = "incomplete"
		reason := "max_output_tokens"
		if result.StopReason == "model_context_window_exceeded" {
			reason = result.StopReason
		}
		response["incomplete_details"] = map[string]any{"reason": reason}
	}
	return response
}

func canonicalMessagesResponse(result canonicalResult, model string, options anthropic.ResponseOptions) map[string]any {
	content := make([]any, 0, len(result.Tools)+2)
	if options.ThinkingEnabled && result.Reasoning != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": result.Reasoning})
	}
	if result.Text != "" || len(result.Tools) == 0 {
		content = append(content, map[string]any{"type": "text", "text": result.Text})
	}
	for _, tool := range result.Tools {
		content = append(content, map[string]any{
			"type": "tool_use", "id": normalizedToolID(tool.ID), "name": tool.Name,
			"input": normalizeArguments(tool.Arguments),
		})
	}
	stopReason := messagesStopReason(result)
	return map[string]any{
		"id": "msg_" + stablePublicID(result.ID), "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stopReason, "stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens": result.Usage.Input, "output_tokens": result.Usage.Output,
			"cache_read_input_tokens": result.Usage.Cached,
		},
	}
}

func normalizeNativeMessagesResponse(payload map[string]any, model string) map[string]any {
	out := cloneJSONMap(payload)
	out["model"] = model
	if stringAt(out, "type") == "" {
		out["type"] = "message"
	}
	if stringAt(out, "role") == "" {
		out["role"] = "assistant"
	}
	if id := stringAt(out, "id"); id == "" {
		out["id"] = "msg_" + stablePublicID("")
	}
	return out
}

func responseOptionsFromMessages(body map[string]any) anthropic.ResponseOptions {
	options := anthropic.ResponseOptions{}
	if thinking, ok := body["thinking"].(map[string]any); ok {
		switch strings.ToLower(strings.TrimSpace(stringAt(thinking, "type"))) {
		case "enabled", "adaptive":
			options.ThinkingEnabled = true
		}
	}
	if raw, ok := body["stop_sequences"].([]any); ok {
		for _, value := range raw {
			if sequence, ok := value.(string); ok && sequence != "" {
				options.StopSequences = append(options.StopSequences, sequence)
			}
		}
	}
	return options
}

func chatStopReason(result canonicalResult) string {
	if len(result.Tools) > 0 {
		return "tool_calls"
	}
	switch result.StopReason {
	case "tool_calls", "tool_use":
		return "tool_calls"
	case "max_tokens", "max_output_tokens", "length", "model_context_window_exceeded":
		return "length"
	case "stop_sequence":
		return "stop"
	case "content_filter", "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

func messagesStopReason(result canonicalResult) string {
	if len(result.Tools) > 0 {
		return "tool_use"
	}
	switch result.StopReason {
	case "tool_calls", "tool_use":
		return "tool_use"
	case "max_tokens", "max_output_tokens", "length":
		return "max_tokens"
	case "stop_sequence":
		return "stop_sequence"
	case "pause_turn", "refusal", "model_context_window_exceeded":
		return result.StopReason
	default:
		return "end_turn"
	}
}

func chatUsage(usage canonicalUsage) map[string]any {
	return map[string]any{
		"prompt_tokens": usage.Input, "completion_tokens": usage.Output, "total_tokens": usage.Input + usage.Output,
		"prompt_tokens_details":     map[string]any{"cached_tokens": usage.Cached},
		"completion_tokens_details": map[string]any{"reasoning_tokens": usage.Reasoning},
	}
}

func responsesUsage(usage canonicalUsage) map[string]any {
	return map[string]any{
		"input_tokens": usage.Input, "output_tokens": usage.Output, "total_tokens": usage.Input + usage.Output,
		"input_tokens_details":  map[string]any{"cached_tokens": usage.Cached},
		"output_tokens_details": map[string]any{"reasoning_tokens": usage.Reasoning},
	}
}

func flattenReasoning(item map[string]any) string {
	if text := firstNonEmptyString(item, "text"); text != "" {
		return text
	}
	return flattenText(item["summary"])
}

func flattenText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any:
		var out strings.Builder
		for _, raw := range value {
			part, _ := raw.(map[string]any)
			switch stringAt(part, "type") {
			case "text", "output_text", "input_text", "summary_text":
				out.WriteString(firstNonEmptyString(part, "text", "content"))
			case "refusal":
				out.WriteString(firstNonEmptyString(part, "refusal", "text"))
			}
		}
		return out.String()
	case map[string]any:
		return firstNonEmptyString(value, "text", "content")
	default:
		return ""
	}
}

func parseArguments(value any) any {
	if text, ok := value.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return decoded
		}
		return map[string]any{"value": text}
	}
	if value == nil {
		return map[string]any{}
	}
	return value
}

func argumentsString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	b, err := json.Marshal(normalizeArguments(value))
	if err != nil {
		return "{}"
	}
	return string(b)
}

func normalizeArguments(value any) any {
	switch value := value.(type) {
	case map[string]any, []any:
		return value
	case nil:
		return map[string]any{}
	default:
		return map[string]any{"value": value}
	}
}

func normalizedToolID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "call_" + grok.NewID()
	}
	return value
}

func stablePublicID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return grok.NewID()
	}
	value = strings.TrimPrefix(value, "chatcmpl-")
	value = strings.TrimPrefix(value, "resp_")
	value = strings.TrimPrefix(value, "msg_")
	return value
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func first(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func firstNonEmptyString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringAt(values, key); value != "" {
			return value
		}
	}
	return ""
}

func stringAt(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func intAt(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := values[key].(type) {
		case float64:
			return int64(value)
		case float32:
			return int64(value)
		case int:
			return int64(value)
		case int64:
			return value
		case json.Number:
			result, _ := value.Int64()
			return result
		}
	}
	return 0
}

func cloneJSONMap(value map[string]any) map[string]any {
	b, _ := json.Marshal(value)
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return map[string]any{}
	}
	return out
}

func adapterDescription(adapter inference.ResponseAdapter) string {
	return fmt.Sprintf("%s<- %s", adapter.ClientProtocol, adapter.UpstreamBackend)
}
