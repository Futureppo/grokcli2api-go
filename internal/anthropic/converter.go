package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/openai"
)

const DefaultVersion = "2023-06-01"

type Prepared struct {
	Body     map[string]any
	Warnings []string
}

func Validate(body map[string]any) error {
	model, ok := body["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required and must be a string")
	}
	max, ok := number(body["max_tokens"])
	if !ok || max <= 0 {
		return fmt.Errorf("max_tokens is required and must be greater than zero")
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return fmt.Errorf("messages is required and must be a non-empty array")
	}
	if _, exists := body["top_k"]; exists {
		return fmt.Errorf("top_k cannot be represented by the Grok Responses API")
	}
	return nil
}

func Prepare(body map[string]any) (Prepared, error) {
	if err := Validate(body); err != nil {
		return Prepared{}, err
	}
	input, err := convertMessages(body["messages"].([]any))
	if err != nil {
		return Prepared{}, err
	}
	out := map[string]any{
		"model":             openai.UpstreamModel(stringValue(body["model"])),
		"input":             input,
		"max_output_tokens": body["max_tokens"],
		"stream":            boolValue(body["stream"]),
		"store":             false,
	}
	if system, ok := body["system"]; ok {
		instructions, err := convertSystem(system)
		if err != nil {
			return Prepared{}, err
		}
		out["instructions"] = instructions
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value, ok := body[key]; ok {
			out[key] = value
		}
	}
	if stops, ok := body["stop_sequences"]; ok {
		out["stop"] = stops
	}
	if tools, ok := body["tools"].([]any); ok {
		converted, err := convertTools(tools)
		if err != nil {
			return Prepared{}, err
		}
		out["tools"] = converted
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		converted, err := convertToolChoice(choice)
		if err != nil {
			return Prepared{}, err
		}
		out["tool_choice"] = converted
	}
	if metadata, ok := body["metadata"].(map[string]any); ok {
		if userID, ok := metadata["user_id"].(string); ok && userID != "" {
			out["user"] = userID
		}
	}
	if thinking, ok := body["thinking"].(map[string]any); ok && stringValue(thinking["type"]) == "enabled" {
		effort := "medium"
		if budget, ok := number(thinking["budget_tokens"]); ok {
			switch {
			case budget <= 2048:
				effort = "low"
			case budget > 10000:
				effort = "high"
			}
		}
		out["reasoning"] = map[string]any{"effort": effort, "summary": "detailed"}
		out["include"] = []any{"reasoning.encrypted_content"}
	}
	if outputConfig, ok := body["output_config"].(map[string]any); ok {
		if format, ok := outputConfig["format"]; ok {
			out["text"] = map[string]any{"format": format}
		}
	}
	if servers, ok := body["mcp_servers"].([]any); ok {
		mcpTools, err := convertMCPServers(servers)
		if err != nil {
			return Prepared{}, err
		}
		existing, _ := out["tools"].([]any)
		out["tools"] = append(existing, mcpTools...)
	}
	if err := normalizeUpstreamTools(out); err != nil {
		return Prepared{}, err
	}
	warnings := []string{}
	for _, key := range []string{"service_tier", "context_management", "container"} {
		if _, ok := body[key]; ok {
			warnings = append(warnings, key)
		}
	}
	known := map[string]bool{
		"model": true, "max_tokens": true, "messages": true, "system": true, "stream": true,
		"temperature": true, "top_p": true, "top_k": true, "stop_sequences": true,
		"tools": true, "tool_choice": true, "metadata": true, "thinking": true,
		"output_config": true, "mcp_servers": true, "service_tier": true,
		"context_management": true, "container": true,
	}
	for key := range body {
		if !known[key] {
			warnings = append(warnings, key)
		}
	}
	return Prepared{Body: out, Warnings: warnings}, nil
}

func convertMessages(messages []any) ([]any, error) {
	var out []any
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages entries must be objects")
		}
		role := stringValue(message["role"])
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("unsupported Anthropic message role %q", role)
		}
		content := message["content"]
		if text, ok := content.(string); ok {
			out = append(out, map[string]any{"type": "message", "role": role, "content": text})
			continue
		}
		blocks, ok := content.([]any)
		if !ok {
			return nil, fmt.Errorf("message content must be a string or array")
		}
		var regular []any
		flush := func() {
			if len(regular) > 0 {
				out = append(out, map[string]any{"type": "message", "role": role, "content": regular})
				regular = nil
			}
		}
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content blocks must be objects")
			}
			switch stringValue(block["type"]) {
			case "text":
				regular = append(regular, map[string]any{"type": "input_text", "text": stringValue(block["text"])})
			case "image":
				image, err := convertImage(block)
				if err != nil {
					return nil, err
				}
				regular = append(regular, image)
			case "document":
				file, err := convertDocument(block)
				if err != nil {
					return nil, err
				}
				regular = append(regular, file)
			case "tool_use":
				flush()
				arguments, err := json.Marshal(block["input"])
				if err != nil {
					return nil, err
				}
				out = append(out, map[string]any{"type": "function_call", "call_id": block["id"], "name": block["name"], "arguments": string(arguments)})
			case "tool_result":
				flush()
				out = append(out, map[string]any{"type": "function_call_output", "call_id": block["tool_use_id"], "output": toolOutput(block["content"])})
			case "thinking":
				flush()
				item := map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": stringValue(block["thinking"])}}}
				if signature := stringValue(block["signature"]); signature != "" {
					item["encrypted_content"] = signature
				}
				out = append(out, item)
			case "redacted_thinking":
				flush()
				out = append(out, map[string]any{"type": "reasoning", "encrypted_content": block["data"]})
			default:
				return nil, fmt.Errorf("unsupported Anthropic content block type %q", stringValue(block["type"]))
			}
		}
		flush()
	}
	return out, nil
}

func convertSystem(value any) (any, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	blocks, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("system must be a string or text block array")
	}
	var text []string
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || stringValue(block["type"]) != "text" {
			return nil, fmt.Errorf("system only supports text blocks")
		}
		text = append(text, stringValue(block["text"]))
	}
	return strings.Join(text, "\n"), nil
}

func convertImage(block map[string]any) (map[string]any, error) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("image source is required")
	}
	var url string
	switch stringValue(source["type"]) {
	case "url":
		url = stringValue(source["url"])
	case "base64":
		url = "data:" + stringValue(source["media_type"]) + ";base64," + stringValue(source["data"])
	default:
		return nil, fmt.Errorf("unsupported image source type %q", stringValue(source["type"]))
	}
	return map[string]any{"type": "input_image", "image_url": url, "detail": "auto"}, nil
}

func convertDocument(block map[string]any) (map[string]any, error) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("document source is required")
	}
	file := map[string]any{"type": "input_file"}
	if title := stringValue(block["title"]); title != "" {
		file["filename"] = title
	}
	switch stringValue(source["type"]) {
	case "url":
		file["file_url"] = source["url"]
	case "base64":
		file["file_data"] = "data:" + stringValue(source["media_type"]) + ";base64," + stringValue(source["data"])
	case "text":
		return map[string]any{"type": "input_text", "text": source["data"]}, nil
	default:
		return nil, fmt.Errorf("unsupported document source type %q", stringValue(source["type"]))
	}
	return file, nil
}

func convertTools(tools []any) ([]any, error) {
	out := make([]any, 0, len(tools))
	for index, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools[%d] must be an object", index)
		}
		kind := stringValue(tool["type"])
		if strings.HasPrefix(kind, "web_search_") {
			converted := map[string]any{"type": "web_search"}
			for _, key := range []string{"max_uses", "allowed_domains", "blocked_domains", "user_location"} {
				if value, ok := tool[key]; ok {
					converted[key] = value
				}
			}
			out = append(out, converted)
			continue
		}
		if name := stringValue(tool["name"]); name != "" {
			parameters, ok := tool["input_schema"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tools[%d] %q input_schema must be an object", index, name)
			}
			converted := map[string]any{"type": "function", "name": name, "parameters": parameters}
			if description, ok := tool["description"]; ok {
				converted["description"] = description
			}
			if strict, ok := tool["strict"]; ok {
				converted["strict"] = strict
			}
			out = append(out, converted)
			continue
		}
		if kind == "" {
			return nil, fmt.Errorf("tools[%d] is missing type and name", index)
		}
		return nil, fmt.Errorf("unsupported Anthropic tool type %q at tools[%d]", kind, index)
	}
	return out, nil
}

// normalizeUpstreamTools is the final guard on the Anthropic request path.
// Every tool sent to the Grok Responses endpoint must carry a discriminator;
// named tools without one are ordinary function tools in the Anthropic API.
func normalizeUpstreamTools(body map[string]any) error {
	raw, exists := body["tools"]
	if !exists {
		return nil
	}
	tools, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("tools must be an array")
	}
	for index, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			return fmt.Errorf("tools[%d] must be an object", index)
		}
		kind := strings.TrimSpace(stringValue(tool["type"]))
		if kind == "" {
			if strings.TrimSpace(stringValue(tool["name"])) == "" {
				return fmt.Errorf("tools[%d] is missing type and name", index)
			}
			tool["type"] = "function"
			kind = "function"
		}
		switch kind {
		case "function":
			if strings.TrimSpace(stringValue(tool["name"])) == "" {
				return fmt.Errorf("tools[%d] function is missing name", index)
			}
			if _, ok := tool["parameters"].(map[string]any); !ok {
				return fmt.Errorf("tools[%d] function parameters must be an object", index)
			}
		case "web_search", "mcp":
			// These hosted tools have their own required fields and already come
			// from the dedicated converters above.
		default:
			return fmt.Errorf("tools[%d] has unsupported upstream type %q", index, kind)
		}
	}
	return nil
}

func convertToolChoice(choice map[string]any) (any, error) {
	switch stringValue(choice["type"]) {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "none":
		return "none", nil
	case "tool":
		return map[string]any{"type": "function", "name": choice["name"]}, nil
	default:
		return nil, fmt.Errorf("unsupported tool_choice type %q", stringValue(choice["type"]))
	}
}

func convertMCPServers(servers []any) ([]any, error) {
	out := make([]any, 0, len(servers))
	for _, raw := range servers {
		server, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mcp_servers entries must be objects")
		}
		converted := map[string]any{"type": "mcp", "server_label": server["name"], "server_url": server["url"]}
		if token, ok := server["authorization_token"]; ok {
			converted["authorization"] = token
		}
		out = append(out, converted)
	}
	return out, nil
}

func NormalizeResponse(raw map[string]any, fallbackModel string) map[string]any {
	content := responseContent(raw)
	stopReason := "end_turn"
	for _, rawBlock := range content {
		if block, ok := rawBlock.(map[string]any); ok && (block["type"] == "tool_use" || block["type"] == "server_tool_use") {
			stopReason = "tool_use"
			break
		}
	}
	if details, ok := raw["incomplete_details"].(map[string]any); ok && stringValue(details["reason"]) == "max_output_tokens" {
		stopReason = "max_tokens"
	}
	if sequence := stringValue(raw["stop_sequence"]); sequence != "" {
		stopReason = "stop_sequence"
	}
	id := stringValue(raw["id"])
	if id == "" {
		id = "msg_" + grok.NewID()
	}
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": fallbackModel,
		"content": content, "stop_reason": stopReason, "stop_sequence": raw["stop_sequence"],
		"usage": normalizeUsage(raw["usage"]),
	}
}

func responseContent(raw map[string]any) []any {
	var content []any
	if output, ok := raw["output"].([]any); ok {
		for _, rawItem := range output {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			switch stringValue(item["type"]) {
			case "message":
				if parts, ok := item["content"].([]any); ok {
					for _, rawPart := range parts {
						part, _ := rawPart.(map[string]any)
						switch stringValue(part["type"]) {
						case "output_text", "text":
							block := map[string]any{"type": "text", "text": stringValue(part["text"])}
							if citations, ok := part["citations"]; ok {
								block["citations"] = citations
							}
							content = append(content, block)
						case "refusal":
							content = append(content, map[string]any{"type": "text", "text": stringValue(part["refusal"])})
						}
					}
				}
			case "reasoning":
				text := reasoningText(item)
				if text != "" || item["encrypted_content"] != nil {
					block := map[string]any{"type": "thinking", "thinking": text}
					if signature := stringValue(item["encrypted_content"]); signature != "" {
						block["signature"] = signature
					}
					content = append(content, block)
				}
			case "function_call", "custom_tool_call":
				content = append(content, toolUseBlock(item))
			case "web_search_call", "file_search_call", "code_interpreter_call", "computer_call", "mcp_call":
				content = append(content, map[string]any{"type": "server_tool_use", "id": itemID(item), "name": item["type"], "input": item["action"]})
			}
		}
	}
	// Some Grok deployments may still answer the Responses path with a
	// chat-completion envelope. Preserve that content as a compatibility path.
	if len(content) == 0 {
		if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
			choice, _ := choices[0].(map[string]any)
			message, _ := choice["message"].(map[string]any)
			if text := stringValue(message["content"]); text != "" {
				content = append(content, map[string]any{"type": "text", "text": text})
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					content = append(content, toolUseBlock(map[string]any{"id": call["id"], "call_id": call["id"], "name": fn["name"], "arguments": fn["arguments"]}))
				}
			}
		}
	}
	return content
}

func toolUseBlock(item map[string]any) map[string]any {
	input := any(map[string]any{})
	if args := stringValue(item["arguments"]); args != "" {
		if json.Unmarshal([]byte(args), &input) != nil {
			input = map[string]any{"value": args}
		}
	} else if value, ok := item["input"]; ok {
		input = value
	}
	return map[string]any{"type": "tool_use", "id": itemID(item), "name": item["name"], "input": input}
}

func reasoningText(item map[string]any) string {
	var text []string
	for _, key := range []string{"summary", "content"} {
		if parts, ok := item[key].([]any); ok {
			for _, raw := range parts {
				part, _ := raw.(map[string]any)
				if value := stringValue(part["text"]); value != "" {
					text = append(text, value)
				}
			}
		}
	}
	return strings.Join(text, "\n")
}

func normalizeUsage(value any) map[string]any {
	usage, _ := value.(map[string]any)
	input := firstNumber(usage, "input_tokens", "prompt_tokens")
	output := firstNumber(usage, "output_tokens", "completion_tokens")
	out := map[string]any{"input_tokens": input, "output_tokens": output}
	if details, ok := usage["input_tokens_details"].(map[string]any); ok {
		if cached, ok := number(details["cached_tokens"]); ok {
			out["cache_read_input_tokens"] = cached
		}
	}
	return out
}

func Error(message, kind string) map[string]any {
	if kind == "" {
		kind = "invalid_request_error"
	}
	return map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}}
}

func itemID(item map[string]any) string {
	for _, key := range []string{"call_id", "id"} {
		if value := stringValue(item[key]); value != "" {
			return value
		}
	}
	return "toolu_" + grok.NewID()
}

func toolOutput(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	b, _ := json.Marshal(value)
	return string(b)
}

func boolValue(value any) bool     { result, _ := value.(bool); return result }
func stringValue(value any) string { result, _ := value.(string); return result }
func number(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	default:
		return 0, false
	}
}
func firstNumber(values map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if value, ok := number(values[key]); ok {
			return value
		}
	}
	return 0
}

// CreatedAt is kept in one place so stream and non-stream responses use the
// same timestamp precision.
func CreatedAt() int64 { return time.Now().Unix() }
