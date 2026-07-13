package openai

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/grok"
)

// RequestError describes a client-side Responses validation failure. Param is
// an exact JSON path so SDK users can locate the rejected value.
type RequestError struct {
	Message string
	Param   string
	Code    string
}

func (e *RequestError) Error() string { return e.Message }

func invalidRequest(param, message string) error {
	return &RequestError{Message: message, Param: param, Code: "invalid_value"}
}

func unsupportedRequest(param, message string) error {
	return &RequestError{Message: message, Param: param, Code: "unsupported_parameter"}
}

func ValidateRequest(body map[string]any) error { return ValidateChatRequest(body) }

func ValidateChatRequest(body map[string]any) error {
	if err := validateModel(body); err != nil {
		return err
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return fmt.Errorf("messages is required and must be a non-empty array")
	}
	return nil
}

func ValidateResponsesRequest(body map[string]any, native bool) error {
	if err := validateModel(body); err != nil {
		return err
	}
	if native {
		if _, ok := body["input"]; !ok {
			if _, legacy := body["messages"]; !legacy {
				return fmt.Errorf("input is required")
			}
		}
		return nil
	}
	allowed := map[string]bool{
		"include": true, "input": true, "instructions": true,
		"max_completion_tokens": true, "max_output_tokens": true, "max_tokens": true, "max_tool_calls": true,
		"messages": true, "metadata": true, "model": true, "parallel_tool_calls": true,
		"previous_response_id": true, "prompt": true, "prompt_cache_key": true, "prompt_cache_retention": true,
		"reasoning": true, "reasoning_effort": true, "response_format": true, "safety_identifier": true,
		"service_tier": true, "store": true, "stream": true, "stream_options": true,
		"temperature": true, "text": true, "tool_choice": true, "tools": true,
		"top_logprobs": true, "top_p": true, "truncation": true, "user": true,
	}
	for key := range body {
		if allowed[key] {
			continue
		}
		switch key {
		case "background", "conversation", "context_management", "prompt_cache_options":
			return unsupportedRequest(key, key+" is not supported by this proxy")
		default:
			return unsupportedRequest(key, "unsupported parameter: "+key)
		}
	}
	input, ok := body["input"]
	if !ok {
		input, ok = body["messages"]
	}
	if !ok || input == nil {
		return invalidRequest("input", "input is required and must be a string or array")
	}
	switch input.(type) {
	case string, []any:
	default:
		return invalidRequest("input", "input is required and must be a string or array")
	}
	if err := validateResponsesInput(input, String(body, "previous_response_id", "") != ""); err != nil {
		return err
	}
	if value, exists := body["previous_response_id"]; exists {
		if id, ok := value.(string); !ok || strings.TrimSpace(id) == "" {
			return invalidRequest("previous_response_id", "previous_response_id must be a non-empty string")
		}
	}
	if value, exists := body["instructions"]; exists {
		if _, ok := value.(string); !ok {
			return invalidRequest("instructions", "instructions must be a string")
		}
	}
	for _, key := range []string{"metadata", "reasoning", "stream_options", "text"} {
		if value, exists := body[key]; exists {
			if _, ok := value.(map[string]any); !ok {
				return invalidRequest(key, key+" must be an object")
			}
		}
	}
	for _, key := range []string{"prompt_cache_key", "prompt_cache_retention", "safety_identifier", "service_tier", "truncation", "user"} {
		if value, exists := body[key]; exists {
			if _, ok := value.(string); !ok {
				return invalidRequest(key, key+" must be a string")
			}
		}
	}
	if value, exists := body["include"]; exists {
		if _, ok := value.([]any); !ok {
			return invalidRequest("include", "include must be an array")
		}
	}
	if value, exists := body["tool_choice"]; exists {
		switch value.(type) {
		case string, map[string]any:
		default:
			return invalidRequest("tool_choice", "tool_choice must be a string or object")
		}
	}
	for _, key := range []string{"stream", "store", "parallel_tool_calls", "background"} {
		if value, exists := body[key]; exists {
			if _, ok := value.(bool); !ok {
				return invalidRequest(key, key+" must be a boolean")
			}
		}
	}
	for _, key := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens", "max_tool_calls"} {
		if value, exists := body[key]; exists {
			number, ok := chatNumber(value)
			if !ok || !finiteNumber(number) || number <= 0 || math.Trunc(number) != number {
				return invalidRequest(key, key+" must be a positive integer")
			}
		}
	}
	for key, bounds := range map[string][2]float64{"temperature": {0, 2}, "top_p": {0, 1}} {
		if value, exists := body[key]; exists {
			number, ok := chatNumber(value)
			if !ok || !finiteNumber(number) || number < bounds[0] || number > bounds[1] {
				return invalidRequest(key, fmt.Sprintf("%s must be a number between %s and %s", key, formatNumber(bounds[0]), formatNumber(bounds[1])))
			}
		}
	}
	if value, exists := body["top_logprobs"]; exists {
		number, ok := chatNumber(value)
		if !ok || !finiteNumber(number) || number < 0 || number > 20 || math.Trunc(number) != number {
			return invalidRequest("top_logprobs", "top_logprobs must be an integer between 0 and 20")
		}
	}
	if err := validateResponsesTools(body["tools"], "tools"); err != nil {
		return err
	}
	if err := validateResponsesTextFormat(body); err != nil {
		return err
	}
	return nil
}

func validateResponsesInput(raw any, hasPrevious bool) error {
	input, ok := raw.([]any)
	if !ok {
		return nil
	}
	calls := make(map[string]int)
	outputs := make(map[string]int)
	for index, rawItem := range input {
		path := fmt.Sprintf("input[%d]", index)
		item, ok := rawItem.(map[string]any)
		if !ok {
			return invalidRequest(path, path+" must be an object")
		}
		kind := String(item, "type", "")
		if kind == "" && item["role"] != nil {
			kind = "message"
		}
		switch kind {
		case "message":
			if err := validateResponsesMessage(item, path); err != nil {
				return err
			}
		case "function_call", "custom_tool_call":
			callID := strings.TrimSpace(String(item, "call_id", ""))
			if callID == "" {
				return invalidRequest(path+".call_id", path+".call_id is required")
			}
			if _, exists := calls[callID]; exists {
				return invalidRequest(path+".call_id", "duplicate tool call_id: "+callID)
			}
			calls[callID] = index
			if strings.TrimSpace(String(item, "name", "")) == "" {
				return invalidRequest(path+".name", path+".name is required")
			}
		case "function_call_output", "custom_tool_call_output":
			callID := strings.TrimSpace(String(item, "call_id", ""))
			if callID == "" {
				return invalidRequest(path+".call_id", path+".call_id is required")
			}
			if _, exists := outputs[callID]; exists {
				return invalidRequest(path+".call_id", "duplicate tool output for call_id: "+callID)
			}
			outputs[callID] = index
			if _, exists := item["output"]; !exists {
				return invalidRequest(path+".output", path+".output is required")
			}
		case "reasoning", "compaction", "context_compaction", "compaction_trigger",
			"item_reference", "additional_tools", "tool_search_call", "tool_search_output",
			"agent_message", "local_shell_call", "local_shell_call_output", "mcp_tool_call_output",
			"web_search_call", "x_search_call", "web_search", "x_search", "computer_call", "computer_call_output",
			"image_generation_call", "code_interpreter_call", "file_search_call", "mcp_call", "mcp_list_tools",
			"mcp_approval_request", "mcp_approval_response":
			// Known portable or safely-cleaned Codex history items.
		default:
			return unsupportedRequest(path+".type", "unsupported input item type: "+kind)
		}
	}
	if !hasPrevious {
		for callID, index := range outputs {
			if _, ok := calls[callID]; !ok {
				return invalidRequest(fmt.Sprintf("input[%d].call_id", index), "tool output has no matching call_id: "+callID)
			}
		}
	}
	return nil
}

func validateResponsesMessage(message map[string]any, path string) error {
	if role, ok := message["role"].(string); !ok || !map[string]bool{"user": true, "assistant": true, "system": true, "developer": true}[role] {
		return invalidRequest(path+".role", path+".role is invalid")
	}
	content, exists := message["content"]
	if !exists {
		return nil // Some Codex assistant history items contain only tool metadata.
	}
	if _, ok := content.(string); ok {
		return nil
	}
	parts, ok := content.([]any)
	if !ok {
		return invalidRequest(path+".content", path+".content must be a string or array")
	}
	for index, rawPart := range parts {
		partPath := fmt.Sprintf("%s.content[%d]", path, index)
		part, ok := rawPart.(map[string]any)
		if !ok {
			return invalidRequest(partPath, partPath+" must be an object")
		}
		switch String(part, "type", "") {
		case "input_text", "output_text":
			if _, ok := part["text"].(string); !ok {
				return invalidRequest(partPath+".text", partPath+".text must be a string")
			}
		case "input_image":
			if _, exists := part["file_id"]; exists {
				return unsupportedRequest(partPath+".file_id", "file_id is unsupported because this proxy does not implement /v1/files")
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || strings.TrimSpace(imageURL) == "" {
				return invalidRequest(partPath+".image_url", partPath+".image_url is required")
			}
			if err := validateImageURL(imageURL); err != nil {
				return invalidRequest(partPath+".image_url", err.Error())
			}
			if detail, exists := part["detail"]; exists {
				value, ok := detail.(string)
				if !ok || !map[string]bool{"auto": true, "low": true, "high": true}[value] {
					return invalidRequest(partPath+".detail", partPath+".detail must be auto, low, or high")
				}
			}
		case "input_file":
			return unsupportedRequest(partPath+".type", "input_file is unsupported because this proxy does not implement /v1/files")
		default:
			return unsupportedRequest(partPath+".type", "unsupported message content type: "+String(part, "type", ""))
		}
	}
	return nil
}

func validateImageURL(value string) error {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		comma := strings.IndexByte(value, ',')
		if comma < 0 {
			return fmt.Errorf("image_url must be a valid Base64 data URI")
		}
		metadata := strings.ToLower(value[5:comma])
		if !strings.HasPrefix(metadata, "image/") || !strings.HasSuffix(metadata, ";base64") {
			return fmt.Errorf("image_url must be an image Base64 data URI")
		}
		decoded, err := base64.StdEncoding.DecodeString(value[comma+1:])
		if err != nil || len(decoded) == 0 {
			return fmt.Errorf("image_url must contain valid Base64 image data")
		}
		return nil
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return fmt.Errorf("image_url must be an HTTPS URL or Base64 data URI")
	}
	return nil
}

func validateResponsesTools(raw any, path string) error {
	if raw == nil {
		return nil
	}
	tools, ok := raw.([]any)
	if !ok {
		return invalidRequest(path, path+" must be an array")
	}
	for index, rawTool := range tools {
		toolPath := fmt.Sprintf("%s[%d]", path, index)
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return invalidRequest(toolPath, toolPath+" must be an object")
		}
		kind := String(tool, "type", "")
		switch kind {
		case "function", "custom":
			if strings.TrimSpace(String(tool, "name", "")) == "" {
				return invalidRequest(toolPath+".name", toolPath+".name is required")
			}
			if parameters, exists := tool["parameters"]; exists {
				if _, ok := parameters.(map[string]any); !ok {
					return invalidRequest(toolPath+".parameters", toolPath+".parameters must be an object")
				}
			}
		case "namespace":
			if strings.TrimSpace(String(tool, "name", "")) == "" {
				return invalidRequest(toolPath+".name", toolPath+".name is required")
			}
			if _, exists := tool["tools"]; !exists {
				return invalidRequest(toolPath+".tools", toolPath+".tools is required")
			}
			if err := validateResponsesTools(tool["tools"], toolPath+".tools"); err != nil {
				return err
			}
		case "tool_search":
			if execution := String(tool, "execution", "client"); !strings.EqualFold(execution, "client") {
				return unsupportedRequest(toolPath+".execution", "only client tool_search execution is supported")
			}
			if parameters, exists := tool["parameters"]; exists {
				if _, ok := parameters.(map[string]any); !ok {
					return invalidRequest(toolPath+".parameters", toolPath+".parameters must be an object")
				}
			}
		case "web_search", "web_search_preview":
			// Web search is the hosted tool executed by the upstream.
		case "image_generation":
			return unsupportedRequest(toolPath+".type", "image_generation output is not supported by this proxy")
		default:
			return unsupportedRequest(toolPath+".type", "unsupported tool type: "+kind)
		}
	}
	return nil
}

func validateResponsesTextFormat(body map[string]any) error {
	var format any
	path := "text.format"
	if rawText, exists := body["text"]; exists {
		text, ok := rawText.(map[string]any)
		if !ok {
			return invalidRequest("text", "text must be an object")
		}
		format = text["format"]
	}
	if legacy, exists := body["response_format"]; exists {
		format, path = legacy, "response_format"
	}
	if format == nil {
		return nil
	}
	object, ok := format.(map[string]any)
	if !ok {
		return invalidRequest(path, path+" must be an object")
	}
	if String(object, "type", "") != "json_schema" {
		return nil
	}
	if path == "response_format" {
		object, ok = object["json_schema"].(map[string]any)
		if !ok {
			return invalidRequest(path+".json_schema", path+".json_schema must be an object")
		}
		path += ".json_schema"
	}
	if strings.TrimSpace(String(object, "name", "")) == "" {
		return invalidRequest(path+".name", path+".name is required")
	}
	if _, ok := object["schema"].(map[string]any); !ok {
		return invalidRequest(path+".schema", path+".schema must be an object")
	}
	return nil
}

func validateModel(body map[string]any) error {
	model, ok := body["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return invalidRequest("model", "model is required and must be a string")
	}
	return nil
}

func IsStreaming(body map[string]any) bool { v, _ := body["stream"].(bool); return v }

func String(body map[string]any, key, fallback string) string {
	if value, ok := body[key].(string); ok && value != "" {
		return value
	}
	return fallback
}

// PrepareResponses accepts the current OpenAI Responses request shape while
// retaining compatibility with the old chat-shaped implementation.
func PrepareResponses(body map[string]any) map[string]any {
	out := clone(body)
	out["model"] = UpstreamModel(String(body, "model", ""))
	out["stream"] = IsStreaming(body)
	normalizeReasoning(out, true)
	if _, ok := out["input"]; !ok {
		if messages, legacy := out["messages"]; legacy {
			out["input"] = messages
			delete(out, "messages")
		}
	}
	if _, ok := out["max_output_tokens"]; !ok {
		for _, key := range []string{"max_completion_tokens", "max_tokens"} {
			if value, exists := out[key]; exists {
				out["max_output_tokens"] = value
				break
			}
		}
	}
	delete(out, "max_completion_tokens")
	delete(out, "max_tokens")
	if format, ok := out["response_format"]; ok {
		if _, exists := out["text"]; !exists {
			out["text"] = map[string]any{"format": responsesTextFormat(format)}
		}
		delete(out, "response_format")
	}
	if _, ok := out["store"]; !ok {
		out["store"] = true
	}
	if _, ok := out["safety_identifier"]; !ok {
		if user, exists := out["user"]; exists {
			out["safety_identifier"] = user
		}
	}
	return filterResponsesRequest(out)
}

// filterResponsesRequest keeps the OpenAI Responses fields implemented by the
// Grok CLI 0.2.99 upstream. Native Grok CLI requests bypass this function via
// PrepareNativeResponses and retain their extension fields unchanged.
func filterResponsesRequest(body map[string]any) map[string]any {
	allowed := map[string]bool{
		"background": true, "conversation": true, "include": true, "input": true,
		"instructions": true, "max_output_tokens": true, "max_tool_calls": true,
		"metadata": true, "model": true, "parallel_tool_calls": true,
		"previous_response_id": true, "prompt": true, "prompt_cache_key": true,
		"prompt_cache_retention": true, "reasoning": true, "safety_identifier": true,
		"service_tier": true, "store": true, "stream": true, "stream_options": true,
		"temperature": true, "text": true, "tool_choice": true, "tools": true,
		"top_logprobs": true, "top_p": true, "truncation": true,
	}
	out := make(map[string]any, len(allowed))
	for key, value := range body {
		if allowed[key] {
			out[key] = value
		}
	}
	return out
}

// IsStrictCompatibilityModel reports models whose Grok CLI endpoint rejects
// optional OpenAI sampling and stopping parameters. Composer model IDs are
// versioned, so match the family instead of a single alias.
func IsStrictCompatibilityModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "composer" || strings.HasPrefix(model, "grok-composer-") ||
		model == "grok-4.5" || strings.HasPrefix(model, "grok-4.5-")
}

func normalizeReasoning(body map[string]any, responses bool) {
	effort := ""
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		effort = normalizeReasoningEffort(String(reasoning, "effort", ""))
		if effort != "" {
			copy := clone(reasoning)
			copy["effort"] = effort
			body["reasoning"] = copy
		}
	}
	if effort == "" {
		effort = normalizeReasoningEffort(String(body, "reasoning_effort", ""))
	}
	if effort == "" {
		return
	}
	if responses {
		if _, ok := body["reasoning"]; !ok {
			body["reasoning"] = map[string]any{"effort": effort}
		}
		delete(body, "reasoning_effort")
		return
	}
	body["reasoning_effort"] = effort
}

func normalizeReasoningEffort(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "":
		return ""
	case "none", "minimal", "low":
		return "low"
	case "adaptive", "medium":
		return "medium"
	case "high", "xhigh":
		return "high"
	default:
		return "medium"
	}
}

func responsesTextFormat(value any) any {
	format, ok := value.(map[string]any)
	if !ok || format["type"] != "json_schema" {
		return value
	}
	legacy, ok := format["json_schema"].(map[string]any)
	if !ok {
		return value
	}
	out := map[string]any{"type": "json_schema"}
	for _, key := range []string{"name", "description", "schema", "strict"} {
		if field, exists := legacy[key]; exists {
			out[key] = field
		}
	}
	return out
}

// PrepareNativeResponses changes only transport fields that the proxy requires. All
// Grok-specific fields and extension items remain untouched.
func PrepareNativeResponses(body map[string]any) map[string]any {
	out := clone(body)
	out["stream"] = IsStreaming(body)
	return out
}

func UpstreamModel(model string) string {
	return model
}

func Normalize(raw map[string]any, fallbackModel string, stream bool) map[string]any {
	return NormalizeChat(raw, fallbackModel, stream)
}

func NormalizeChat(raw map[string]any, fallbackModel string, stream bool) map[string]any {
	out := clone(raw)
	if id, _ := out["id"].(string); id == "" {
		out["id"] = "chatcmpl-" + grok.NewID()
	}
	if _, ok := out["object"]; !ok {
		if stream {
			out["object"] = "chat.completion.chunk"
		} else {
			out["object"] = "chat.completion"
		}
	}
	if _, ok := out["created"]; !ok {
		out["created"] = time.Now().Unix()
	}
	if model, _ := out["model"].(string); model == "" {
		out["model"] = fallbackModel
	}
	if _, ok := out["choices"]; !ok {
		out["choices"] = []any{}
	}
	return out
}

func NormalizeResponse(raw map[string]any, fallbackModel string) map[string]any {
	out := clone(raw)
	if id, _ := out["id"].(string); id == "" {
		out["id"] = "resp_" + grok.NewID()
	}
	if object, _ := out["object"].(string); object == "" {
		out["object"] = "response"
	}
	if _, ok := out["created_at"]; !ok {
		out["created_at"] = time.Now().Unix()
	}
	if fallbackModel != "" {
		out["model"] = fallbackModel
	}
	if _, ok := out["status"]; !ok {
		out["status"] = "completed"
	}
	if _, ok := out["output"]; !ok {
		out["output"] = []any{}
	}
	return normalizeResponseObject(out)
}

func normalizeResponseObject(body map[string]any) map[string]any {
	out := filterResponseObject(body)
	delete(out, "billing")
	if usage, ok := out["usage"].(map[string]any); ok {
		out["usage"] = sanitizeResponseUsage(usage)
	}
	return out
}

func sanitizeResponseUsage(usage map[string]any) map[string]any {
	out := make(map[string]any, 5)
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if value, exists := usage[key]; exists {
			out[key] = value
		}
	}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		clean := make(map[string]any, 1)
		if value, exists := details["cached_tokens"]; exists {
			clean["cached_tokens"] = value
		}
		out["input_tokens_details"] = clean
	}
	if details, ok := usage["output_tokens_details"].(map[string]any); ok {
		clean := make(map[string]any, 1)
		if value, exists := details["reasoning_tokens"]; exists {
			clean["reasoning_tokens"] = value
		}
		out["output_tokens_details"] = clean
	}
	return out
}

func filterResponseObject(body map[string]any) map[string]any {
	allowed := map[string]bool{
		"id": true, "object": true, "created_at": true, "status": true,
		"background": true, "completed_at": true, "conversation": true,
		"error": true, "incomplete_details": true, "instructions": true,
		"max_output_tokens": true, "max_tool_calls": true, "metadata": true, "model": true,
		"output": true, "parallel_tool_calls": true, "previous_response_id": true,
		"prompt": true, "prompt_cache_key": true, "prompt_cache_retention": true,
		"reasoning": true, "safety_identifier": true, "service_tier": true,
		"store": true, "temperature": true, "text": true, "tool_choice": true,
		"tools": true, "top_logprobs": true, "top_p": true, "truncation": true,
		"usage": true,
	}
	out := make(map[string]any, len(body))
	for key, value := range body {
		if allowed[key] {
			out[key] = value
		}
	}
	return out
}

// EnsureAssistantRole inserts the role into the first streamed choice only
// when the upstream did not send it, avoiding an extra synthetic chunk.
func EnsureAssistantRole(chunk map[string]any) bool {
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := delta["role"]; !exists {
		delta["role"] = "assistant"
	}
	return true
}

func EventType(event string, data map[string]any) string {
	if event != "" {
		return event
	}
	value, _ := data["type"].(string)
	return value
}

func LeadingChunk(model string) map[string]any {
	return map[string]any{
		"id": "", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	}
}

func Error(message, kind, code string) map[string]any {
	return ErrorWithParam(message, kind, code, "")
}

func ErrorWithParam(message, kind, code, param string) map[string]any {
	var value any
	if param != "" {
		value = param
	}
	return map[string]any{"error": map[string]any{"message": message, "type": kind, "param": value, "code": code}}
}

func ResponseStreamError(message, code string) map[string]any {
	return map[string]any{"type": "error", "code": code, "message": message, "param": nil}
}

func clone(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
