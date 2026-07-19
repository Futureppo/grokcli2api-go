package inference

import (
	"encoding/json"
	"fmt"
	"strings"
)

type blockKind uint8

const (
	blockText blockKind = iota + 1
	blockImage
	blockToolCall
	blockToolResult
	blockReasoning
)

type canonicalBlock struct {
	kind      blockKind
	text      string
	url       string
	mediaType string
	data      string
	id        string
	name      string
	arguments string
	input     any
	output    any
	isError   bool
}

type canonicalMessage struct {
	role    string
	name    string
	modelID string
	blocks  []canonicalBlock
}

type canonicalTool struct {
	name        string
	description string
	parameters  map[string]any
}

type canonicalRequest struct {
	messages    []canonicalMessage
	tools       []canonicalTool
	toolAliases map[string]ToolAlias
	maxTokens   any
	temperature any
	topP        any
	user        string
	format      any
}

func (p *RequestPlan) canonical() canonicalRequest {
	switch p.protocol {
	case ProtocolResponses:
		return canonicalFromResponses(p.body)
	case ProtocolMessages:
		return canonicalFromMessages(p.body)
	default:
		return canonicalFromChat(p.body)
	}
}

func canonicalFromChat(body map[string]any) canonicalRequest {
	request := canonicalRequest{
		maxTokens:   first(body, "max_tokens", "max_completion_tokens", "maxTokens"),
		temperature: body["temperature"], topP: body["top_p"],
		format: body["response_format"],
	}
	request.user = firstString(body, "user", "user_id")
	if request.user == "" {
		if metadata, ok := body["metadata"].(map[string]any); ok {
			request.user = trimmedString(metadata["user_id"])
		}
	}
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(trimmedString(message["role"]))
		if role == "developer" {
			role = "system"
		}
		if role != "system" && role != "user" && role != "assistant" && role != "tool" {
			continue
		}
		converted := canonicalMessage{role: role, name: trimmedString(message["name"])}
		if role == "assistant" {
			converted.modelID = trimmedString(message["model_id"])
		}
		if role == "tool" {
			if id := trimmedString(message["tool_call_id"]); id != "" {
				if output, ok := portableOutput(message["content"]); ok {
					converted.blocks = append(converted.blocks, canonicalBlock{kind: blockToolResult, id: id, output: output})
				}
			}
		} else {
			converted.blocks = append(converted.blocks, chatContent(message["content"], role)...)
		}
		if role == "assistant" {
			calls, _ := message["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				if kind := trimmedString(call["type"]); kind != "" && kind != "function" {
					continue
				}
				function, _ := call["function"].(map[string]any)
				id, name := trimmedString(call["id"]), trimmedString(function["name"])
				arguments, ok := function["arguments"].(string)
				if id == "" || name == "" || !ok {
					continue
				}
				converted.blocks = append(converted.blocks, canonicalBlock{kind: blockToolCall, id: id, name: name, arguments: arguments})
			}
		}
		if len(converted.blocks) > 0 {
			request.messages = append(request.messages, converted)
		}
	}
	request.tools = chatTools(body)
	return request
}

func chatContent(raw any, role string) []canonicalBlock {
	if text, ok := raw.(string); ok {
		return []canonicalBlock{{kind: blockText, text: text}}
	}
	parts, _ := raw.([]any)
	out := make([]canonicalBlock, 0, len(parts))
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(trimmedString(part["type"])) {
		case "text", "input_text", "output_text":
			if text, ok := part["text"].(string); ok {
				out = append(out, canonicalBlock{kind: blockText, text: text})
			}
		case "image_url", "input_image", "image":
			if role != "user" {
				continue
			}
			if image, ok := canonicalImage(part); ok {
				out = append(out, image)
			}
		}
	}
	return out
}

func chatTools(body map[string]any) []canonicalTool {
	rawTools, ok := body["tools"].([]any)
	legacy := false
	if !ok {
		rawTools, ok = body["functions"].([]any)
		legacy = ok
	}
	if !ok {
		return nil
	}
	var out []canonicalTool
	seen := make(map[string]bool)
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function := tool
		if !legacy {
			if kind := trimmedString(tool["type"]); kind != "function" {
				continue
			}
			function, _ = tool["function"].(map[string]any)
		}
		name := trimmedString(function["name"])
		parameters, valid := function["parameters"].(map[string]any)
		if name == "" || !valid || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, canonicalTool{name: name, description: stringValue(function["description"]), parameters: cloneMap(parameters)})
	}
	return out
}

func canonicalFromResponses(body map[string]any) canonicalRequest {
	request := canonicalRequest{
		maxTokens:   first(body, "max_output_tokens", "max_completion_tokens", "max_tokens"),
		temperature: body["temperature"], topP: body["top_p"],
	}
	request.tools, request.toolAliases = responsesTools(responseToolSources(body))
	request.user = firstString(body, "safety_identifier", "user")
	if text, ok := body["text"].(map[string]any); ok {
		request.format = text["format"]
	}
	if request.format == nil {
		request.format = body["response_format"]
	}
	if instructions, ok := body["instructions"].(string); ok {
		request.messages = append(request.messages, canonicalMessage{role: "system", blocks: []canonicalBlock{{kind: blockText, text: instructions}}})
	}
	rawInput, exists := body["input"]
	if !exists {
		rawInput = body["messages"]
	}
	request.toolAliases = ensureResponseInputToolAliases(rawInput, request.toolAliases)
	if text, ok := rawInput.(string); ok {
		request.messages = append(request.messages, canonicalMessage{role: "user", blocks: []canonicalBlock{{kind: blockText, text: text}}})
	} else if input, ok := rawInput.([]any); ok {
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind := strings.ToLower(trimmedString(item["type"]))
			if kind == "" && item["role"] != nil {
				kind = "message"
			}
			switch kind {
			case "message":
				role := strings.ToLower(trimmedString(item["role"]))
				if role == "developer" {
					role = "system"
				}
				if role != "system" && role != "user" && role != "assistant" {
					continue
				}
				message := canonicalMessage{role: role, blocks: responsesContent(item["content"], role)}
				if role == "assistant" {
					message.modelID = trimmedString(item["model_id"])
				}
				if len(message.blocks) > 0 {
					request.messages = append(request.messages, message)
				}
			case "function_call", "custom_tool_call":
				id, name := trimmedString(first(item, "call_id", "id")), trimmedString(item["name"])
				if id == "" || name == "" {
					continue
				}
				wireName, identity := responseToolWireIdentity(kind, name, trimmedString(item["namespace"]), request.toolAliases)
				if wireName != "" {
					name = wireName
				}
				args := stringifyFunctionArguments(item["arguments"])
				input := cloneValue(item["input"])
				if kind == "custom_tool_call" {
					input = map[string]any{"input": input}
					args = jsonString(input)
				} else if identity.Kind == "custom" {
					// A cached/replayed custom call may already be represented as a
					// function call. Keep its portable wrapper intact.
					input = decodeObject(args)
				}
				request.messages = append(request.messages, canonicalMessage{role: "assistant", blocks: []canonicalBlock{{kind: blockToolCall, id: id, name: name, arguments: args, input: input}}})
			case "function_call_output", "custom_tool_call_output":
				id := trimmedString(item["call_id"])
				if id == "" {
					continue
				}
				if output, ok := portableOutput(item["output"]); ok {
					request.messages = append(request.messages, canonicalMessage{role: "tool", blocks: []canonicalBlock{{kind: blockToolResult, id: id, output: output}}})
				}
			case "tool_search_output":
				if clientToolExecution(item) {
					request.messages = append(request.messages, canonicalMessage{role: "system", blocks: []canonicalBlock{{kind: blockText, text: toolSearchCompletedText}}})
				}
			}
		}
	}
	return request
}

func responsesContent(raw any, role string) []canonicalBlock {
	if text, ok := raw.(string); ok {
		return []canonicalBlock{{kind: blockText, text: text}}
	}
	parts, _ := raw.([]any)
	var out []canonicalBlock
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(trimmedString(part["type"])) {
		case "input_text", "output_text", "text":
			if text, ok := part["text"].(string); ok {
				out = append(out, canonicalBlock{kind: blockText, text: text})
			}
		case "input_image", "image_url", "image":
			if role != "user" {
				continue
			}
			if image, ok := canonicalImage(part); ok {
				out = append(out, image)
			}
		}
	}
	return out
}

func responsesTools(raw any) ([]canonicalTool, map[string]ToolAlias) {
	items, _ := raw.([]any)
	tools, aliases := cleanNativeResponsesTools(items, false)
	var out []canonicalTool
	seen := map[string]bool{}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(trimmedString(tool["type"])) != "function" {
			continue
		}
		name := trimmedString(tool["name"])
		parameters, ok := tool["parameters"].(map[string]any)
		if name == "" || !ok || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, canonicalTool{name: name, description: stringValue(tool["description"]), parameters: cloneMap(parameters)})
	}
	return out, aliases
}

func responseToolWireIdentity(kind, name, namespace string, aliases map[string]ToolAlias) (string, ToolAlias) {
	wantKind := "function"
	if kind == "custom_tool_call" {
		wantKind = "custom"
	}
	for wire, alias := range aliases {
		if alias.Name == name && alias.Namespace == namespace && alias.Kind == wantKind {
			return wire, alias
		}
	}
	if alias, ok := aliases[name]; ok {
		return name, alias
	}
	return "", ToolAlias{}
}

func canonicalFromMessages(body map[string]any) canonicalRequest {
	request := canonicalRequest{
		maxTokens: body["max_tokens"], temperature: body["temperature"], topP: body["top_p"],
	}
	if metadata, ok := body["metadata"].(map[string]any); ok {
		request.user = trimmedString(metadata["user_id"])
	}
	if output, ok := body["output_config"].(map[string]any); ok {
		request.format = output["format"]
	}
	if system := messageSystem(body["system"]); len(system) > 0 {
		request.messages = append(request.messages, canonicalMessage{role: "system", blocks: system})
	}
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(trimmedString(message["role"]))
		if role != "user" && role != "assistant" {
			continue
		}
		converted := canonicalMessage{role: role}
		if role == "assistant" {
			converted.modelID = trimmedString(message["model_id"])
		}
		if text, ok := message["content"].(string); ok {
			converted.blocks = append(converted.blocks, canonicalBlock{kind: blockText, text: text})
		} else if blocks, ok := message["content"].([]any); ok {
			for _, rawBlock := range blocks {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					continue
				}
				switch strings.ToLower(trimmedString(block["type"])) {
				case "text":
					if text, ok := block["text"].(string); ok {
						converted.blocks = append(converted.blocks, canonicalBlock{kind: blockText, text: text})
					}
				case "image":
					if role == "user" {
						if image, ok := canonicalImage(block); ok {
							converted.blocks = append(converted.blocks, image)
						}
					}
				case "document":
					source, _ := block["source"].(map[string]any)
					if trimmedString(source["type"]) == "text" {
						if text, ok := source["data"].(string); ok {
							converted.blocks = append(converted.blocks, canonicalBlock{kind: blockText, text: text})
						}
					}
				case "tool_use":
					if role != "assistant" {
						continue
					}
					id, name := trimmedString(block["id"]), trimmedString(block["name"])
					if id != "" && name != "" && block["input"] != nil {
						converted.blocks = append(converted.blocks, canonicalBlock{kind: blockToolCall, id: id, name: name, arguments: jsonString(block["input"]), input: cloneValue(block["input"])})
					}
				case "tool_result":
					if role != "user" {
						continue
					}
					id := trimmedString(block["tool_use_id"])
					if output, ok := portableOutput(block["content"]); id != "" && ok {
						converted.blocks = append(converted.blocks, canonicalBlock{kind: blockToolResult, id: id, output: output, isError: boolValue(block["is_error"])})
					}
				case "thinking":
					if role != "assistant" {
						continue
					}
					signature := trimmedString(block["signature"])
					if signature != "" {
						converted.blocks = append(converted.blocks, canonicalBlock{
							kind: blockReasoning, text: stringValue(block["thinking"]), data: signature,
						})
					}
				}
			}
		}
		if len(converted.blocks) > 0 {
			request.messages = append(request.messages, converted)
		}
	}
	request.tools = messageTools(body["tools"])
	return request
}

func messageSystem(raw any) []canonicalBlock {
	if text, ok := raw.(string); ok {
		return []canonicalBlock{{kind: blockText, text: text}}
	}
	blocks, _ := raw.([]any)
	var out []canonicalBlock
	for _, rawBlock := range blocks {
		block, _ := rawBlock.(map[string]any)
		if trimmedString(block["type"]) == "text" {
			if text, ok := block["text"].(string); ok {
				out = append(out, canonicalBlock{kind: blockText, text: text})
			}
		}
	}
	return out
}

func messageTools(raw any) []canonicalTool {
	tools, _ := raw.([]any)
	var out []canonicalTool
	seen := map[string]bool{}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok || strings.HasPrefix(trimmedString(tool["type"]), "web_search_") {
			continue
		}
		name := trimmedString(tool["name"])
		parameters, ok := tool["input_schema"].(map[string]any)
		if name == "" || !ok || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, canonicalTool{name: name, description: stringValue(tool["description"]), parameters: cloneMap(parameters)})
	}
	return out
}

func canonicalImage(part map[string]any) (canonicalBlock, bool) {
	if raw := part["image_url"]; raw != nil {
		switch raw := raw.(type) {
		case string:
			if strings.TrimSpace(raw) != "" {
				return imageFromURL(raw)
			}
		case map[string]any:
			if url := trimmedString(raw["url"]); url != "" {
				return imageFromURL(url)
			}
		}
	}
	if url := trimmedString(part["url"]); url != "" {
		return imageFromURL(url)
	}
	source, _ := part["source"].(map[string]any)
	switch trimmedString(source["type"]) {
	case "url":
		if url := trimmedString(source["url"]); url != "" {
			return imageFromURL(url)
		}
	case "base64":
		mediaType, data := trimmedString(source["media_type"]), trimmedString(source["data"])
		if mediaType != "" && data != "" {
			return canonicalBlock{kind: blockImage, url: "data:" + mediaType + ";base64," + data, mediaType: mediaType, data: data}, true
		}
	}
	return canonicalBlock{}, false
}

func imageFromURL(url string) (canonicalBlock, bool) {
	if strings.HasPrefix(strings.ToLower(url), "data:") {
		comma := strings.IndexByte(url, ',')
		if comma <= 5 {
			return canonicalBlock{}, false
		}
		meta := url[5:comma]
		if !strings.HasSuffix(strings.ToLower(meta), ";base64") {
			return canonicalBlock{}, false
		}
		return canonicalBlock{kind: blockImage, url: url, mediaType: strings.TrimSuffix(meta, ";base64"), data: url[comma+1:]}, true
	}
	return canonicalBlock{kind: blockImage, url: url}, true
}

func portableOutput(raw any) (any, bool) {
	if raw == nil {
		return "", true
	}
	if text, ok := raw.(string); ok {
		return text, true
	}
	parts, ok := raw.([]any)
	if !ok {
		if _, err := json.Marshal(raw); err == nil {
			return cloneValue(raw), true
		}
		return nil, false
	}
	var text []string
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		kind := trimmedString(part["type"])
		if kind == "text" || kind == "input_text" || kind == "output_text" {
			if value, ok := part["text"].(string); ok {
				text = append(text, value)
			}
		}
	}
	if len(text) == 0 {
		return nil, false
	}
	return strings.Join(text, "\n"), true
}

func first(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, exists := values[key]; exists {
			return value
		}
	}
	return nil
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := trimmedString(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func stringValue(value any) string   { valueString, _ := value.(string); return valueString }
func trimmedString(value any) string { return strings.TrimSpace(stringValue(value)) }
func boolValue(value any) bool       { valueBool, _ := value.(bool); return valueBool }

func jsonString(value any) string {
	if value == nil {
		return "{}"
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}
