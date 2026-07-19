package inference

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

func (p *RequestPlan) renderChat(model string, descriptor modelcatalog.ModelDescriptor) (map[string]any, error) {
	common := p.canonical()
	messages := renderChatMessages(common.messages)
	if len(messages) == 0 {
		return nil, noInput(p.protocol)
	}
	out := map[string]any{"model": model, "messages": messages, "stream": p.stream}
	if p.stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if err := copySampling(out, common, "max_tokens"); err != nil {
		return nil, err
	}
	if common.user != "" {
		out["user"] = common.user
	}
	if len(common.tools) > 0 {
		tools := make([]any, 0, len(common.tools))
		for _, tool := range common.tools {
			function := map[string]any{"name": tool.name, "parameters": cloneMap(tool.parameters)}
			if tool.description != "" {
				function["description"] = tool.description
			}
			// strict is intentionally not synthesized or forwarded: CLI chat
			// backends do not consistently accept it.
			tools = append(tools, map[string]any{"type": "function", "function": function})
		}
		out["tools"] = tools
		if choice := chatToolChoice(p.body, common.tools, common.toolAliases); choice != nil {
			out["tool_choice"] = choice
		}
	}
	if p.protocol == ProtocolChatCompletions && descriptor.SupportsBackendSearch {
		search, err := cleanSearchParameters(p.body["search_parameters"])
		if err != nil {
			return nil, err
		}
		if len(search) > 0 {
			out["search_parameters"] = search
		}
	}
	return out, nil
}

func renderChatMessages(messages []canonicalMessage) []any {
	var out []any
	knownCalls := map[string]bool{}
	for _, message := range messages {
		if message.role == "tool" {
			for _, block := range message.blocks {
				if block.kind != blockToolResult || block.id == "" || !knownCalls[block.id] {
					continue
				}
				out = append(out, map[string]any{"role": "tool", "tool_call_id": block.id, "content": outputString(block.output, block.isError)})
			}
			continue
		}
		clean := map[string]any{"role": message.role}
		if message.name != "" {
			clean["name"] = message.name
		}
		if message.role == "assistant" && message.modelID != "" {
			clean["model_id"] = message.modelID
		}
		var parts []any
		var calls []any
		for _, block := range message.blocks {
			switch block.kind {
			case blockText:
				parts = append(parts, map[string]any{"type": "text", "text": block.text})
			case blockImage:
				if message.role == "user" && block.url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": block.url}})
				}
			case blockToolCall:
				if message.role != "assistant" || block.id == "" || block.name == "" {
					continue
				}
				knownCalls[block.id] = true
				calls = append(calls, map[string]any{"id": block.id, "type": "function", "function": map[string]any{"name": block.name, "arguments": block.arguments}})
			}
		}
		if len(parts) == 1 {
			if part, _ := parts[0].(map[string]any); part["type"] == "text" {
				clean["content"] = part["text"]
			} else {
				clean["content"] = parts
			}
		} else if len(parts) > 0 {
			clean["content"] = parts
		}
		if len(calls) > 0 {
			clean["tool_calls"] = calls
		}
		if clean["content"] != nil || len(calls) > 0 {
			out = append(out, clean)
		}
	}
	return out
}

func chatToolChoice(body map[string]any, tools []canonicalTool, aliases map[string]ToolAlias) any {
	choice := parseToolChoice(body)
	if choice.mode != "" {
		return choice.mode
	}
	name := toolChoiceWireName(choice, aliases)
	if name == "" {
		return nil
	}
	for _, tool := range tools {
		if tool.name == name {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return nil
}

func toolChoiceWireName(choice toolChoiceSpec, aliases map[string]ToolAlias) string {
	if choice.name == "" {
		return ""
	}
	for wire, alias := range aliases {
		kindMatches := choice.kind == "tool" || choice.kind == alias.Kind || choice.kind == "function" && alias.Kind == "function"
		if kindMatches && alias.Name == choice.name && alias.Namespace == choice.namespace {
			return wire
		}
	}
	return choice.name
}

type toolChoiceSpec struct {
	mode      string
	kind      string
	name      string
	namespace string
}

// parseToolChoice reduces the three public tool-choice dialects to the common
// subset supported by all backends. Anthropic's any/tool spellings are mapped
// to required/named here; renderers then encode that intent for their wire.
func parseToolChoice(body map[string]any) toolChoiceSpec {
	raw := body["tool_choice"]
	if raw == nil {
		raw = body["function_call"]
	}
	if value, ok := raw.(string); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "none", "auto", "required":
			return toolChoiceSpec{mode: strings.ToLower(strings.TrimSpace(value))}
		}
		return toolChoiceSpec{}
	}
	choice, _ := raw.(map[string]any)
	kind := strings.ToLower(trimmedString(choice["type"]))
	switch kind {
	case "none", "auto":
		return toolChoiceSpec{mode: kind}
	case "any", "required":
		return toolChoiceSpec{mode: "required"}
	case "tool", "function", "custom":
		function, _ := choice["function"].(map[string]any)
		name := trimmedString(function["name"])
		if name == "" {
			name = trimmedString(choice["name"])
		}
		if name != "" {
			return toolChoiceSpec{kind: kind, name: name, namespace: trimmedString(choice["namespace"])}
		}
	}
	return toolChoiceSpec{}
}

func requestedParallelToolCalls(body map[string]any) (bool, bool) {
	if value, exists := body["parallel_tool_calls"]; exists {
		parallel, ok := value.(bool)
		return parallel, ok
	}
	choice, _ := body["tool_choice"].(map[string]any)
	if value, exists := choice["disable_parallel_tool_use"]; exists {
		disabled, ok := value.(bool)
		return !disabled, ok
	}
	return false, false
}

func (p *RequestPlan) renderResponses(model string, descriptor modelcatalog.ModelDescriptor) (map[string]any, bool, map[string]ToolAlias, error) {
	if p.protocol == ProtocolResponses {
		out, aliases, err := p.renderNativeResponses(model, descriptor)
		if err != nil {
			return nil, false, nil, err
		}
		preserves := responsesBodyPreservesState(out)
		return out, preserves, aliases, nil
	}
	common := p.canonical()
	input := renderResponsesInput(common.messages)
	if len(input) == 0 {
		return nil, false, nil, noInput(p.protocol)
	}
	out := map[string]any{"model": model, "input": input, "stream": p.stream, "store": false}
	if err := copySampling(out, common, "max_output_tokens"); err != nil {
		return nil, false, nil, err
	}
	if common.user != "" {
		out["safety_identifier"] = common.user
	}
	if common.format != nil {
		if format := responsesFormat(common.format); format != nil {
			out["text"] = map[string]any{"format": format}
		}
	}
	if len(common.tools) > 0 {
		tools := renderResponsesTools(common.tools)
		out["tools"] = tools
		if choice := responsesToolChoice(p.body, tools, nil); choice != nil {
			out["tool_choice"] = choice
		}
		if parallel, ok := requestedParallelToolCalls(p.body); ok {
			out["parallel_tool_calls"] = parallel
		}
	}
	if p.stream && descriptor.StreamToolCalls {
		out["stream_tool_calls"] = true
	}
	if p.protocol == ProtocolMessages {
		if err := applyMessagesThinkingToResponses(out, p.body["thinking"]); err != nil {
			return nil, false, nil, err
		}
	}
	return out, responsesBodyPreservesState(out), nil, nil
}

func responsesBodyPreservesState(body map[string]any) bool {
	if _, ok := body["previous_response_id"]; ok {
		return true
	}
	input, _ := body["input"].([]any)
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if trimmedString(item["type"]) == "reasoning" && trimmedString(item["encrypted_content"]) != "" {
			return true
		}
	}
	return false
}

func renderResponsesInput(messages []canonicalMessage) []any {
	var out []any
	knownCalls := map[string]bool{}
	for _, message := range messages {
		var content []any
		flushContent := func() {
			if len(content) == 0 {
				return
			}
			out = append(out, map[string]any{"type": "message", "role": message.role, "content": content})
			content = nil
		}
		for _, block := range message.blocks {
			switch block.kind {
			case blockText:
				kind := "input_text"
				if message.role == "assistant" {
					kind = "output_text"
				}
				content = append(content, map[string]any{"type": kind, "text": block.text})
			case blockImage:
				if message.role == "user" && block.url != "" {
					content = append(content, map[string]any{"type": "input_image", "image_url": block.url})
				}
			case blockToolCall:
				if message.role != "assistant" || block.id == "" || block.name == "" {
					continue
				}
				flushContent()
				knownCalls[block.id] = true
				out = append(out, map[string]any{"type": "function_call", "call_id": block.id, "name": block.name, "arguments": block.arguments})
			case blockToolResult:
				if block.id == "" || !knownCalls[block.id] {
					continue
				}
				flushContent()
				out = append(out, map[string]any{"type": "function_call_output", "call_id": block.id, "output": outputString(block.output, block.isError)})
			case blockReasoning:
				if message.role != "assistant" || block.data == "" {
					continue
				}
				flushContent()
				item := map[string]any{"type": "reasoning", "encrypted_content": block.data}
				if block.text != "" {
					item["summary"] = []any{map[string]any{"type": "summary_text", "text": block.text}}
				}
				out = append(out, item)
			}
		}
		flushContent()
	}
	return out
}

func applyMessagesThinkingToResponses(out map[string]any, raw any) error {
	thinking, err := cleanThinking(raw)
	if err != nil {
		return err
	}
	if thinking == nil {
		return nil
	}
	config := thinking.(map[string]any)
	switch trimmedString(config["type"]) {
	case "disabled":
		return nil
	case "adaptive":
		if trimmedString(config["display"]) == "summarized" {
			out["reasoning"] = map[string]any{"summary": "detailed"}
		}
	case "enabled":
		budget, _ := jsonNumber(config["budget_tokens"])
		effort := "medium"
		switch {
		case budget <= 2048:
			effort = "low"
		case budget > 10000:
			effort = "high"
		}
		out["reasoning"] = map[string]any{"effort": effort, "summary": "detailed"}
	}
	out["include"] = []any{"reasoning.encrypted_content"}
	return nil
}

func renderResponsesTools(tools []canonicalTool) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		item := map[string]any{"type": "function", "name": tool.name, "parameters": cloneMap(tool.parameters)}
		if tool.description != "" {
			item["description"] = tool.description
		}
		out = append(out, item)
	}
	return out
}

func (p *RequestPlan) renderNativeResponses(model string, descriptor modelcatalog.ModelDescriptor) (map[string]any, map[string]ToolAlias, error) {
	out := map[string]any{"model": model, "stream": p.stream}
	tools, aliases := cleanNativeResponsesTools(responseToolSources(p.body), descriptor.SupportsBackendSearch)
	rawInput := p.body["input"]
	if rawInput == nil {
		rawInput = p.body["messages"]
	}
	aliases = ensureResponseInputToolAliases(rawInput, aliases)
	_, allowStatefulOutputs := p.body["previous_response_id"].(string)
	input := rewriteNativeResponseInputAliases(cleanNativeResponsesInput(rawInput, allowStatefulOutputs), aliases)
	switch input := input.(type) {
	case string:
		out["input"] = input
	case []any:
		if len(input) == 0 {
			return nil, nil, noInput(p.protocol)
		}
		out["input"] = input
	default:
		return nil, nil, noInput(p.protocol)
	}
	if text, ok := p.body["instructions"].(string); ok {
		out["instructions"] = text
	} else if p.body["instructions"] != nil {
		return nil, nil, requestError("instructions", "instructions must be a string")
	}

	for _, key := range []string{"max_output_tokens", "max_tool_calls", "top_logprobs"} {
		if value, exists := p.body[key]; exists {
			if err := positiveInteger(value, key, key != "top_logprobs"); err != nil {
				return nil, nil, err
			}
			out[key] = value
		}
	}
	if _, exists := out["max_output_tokens"]; !exists {
		for _, alias := range []string{"max_completion_tokens", "max_tokens"} {
			if value, exists := p.body[alias]; exists {
				if err := positiveInteger(value, alias, true); err != nil {
					return nil, nil, err
				}
				out["max_output_tokens"] = value
				break
			}
		}
	}
	if err := copyOptionalNumber(out, p.body, "temperature", 0, 2); err != nil {
		return nil, nil, err
	}
	if err := copyOptionalNumber(out, p.body, "top_p", 0, 1); err != nil {
		return nil, nil, err
	}
	for _, key := range []string{"parallel_tool_calls", "background"} {
		if value, exists := p.body[key]; exists {
			if _, ok := value.(bool); !ok {
				return nil, nil, requestError(key, key+" must be a boolean")
			}
			out[key] = value
		}
	}
	if value, exists := p.body["store"]; exists {
		if _, ok := value.(bool); !ok {
			return nil, nil, requestError("store", "store must be a boolean")
		}
		out["store"] = value
	} else {
		out["store"] = !p.nativeCLI
	}
	for _, key := range []string{"metadata", "prompt"} {
		if object, ok := p.body[key].(map[string]any); ok {
			out[key] = cloneMap(object)
		} else if p.body[key] != nil {
			// These schemas evolve independently. Values that cannot be copied
			// without guessing are silently removed.
		}
	}
	if conversation, ok := p.body["conversation"].(string); ok && strings.TrimSpace(conversation) != "" {
		// Responses accepts a stable conversation ID as well as the expanded
		// object form. Preserve the opaque ID byte-for-byte on the native wire.
		out["conversation"] = conversation
	} else if conversation, ok := p.body["conversation"].(map[string]any); ok {
		out["conversation"] = cloneMap(conversation)
	}
	for _, key := range []string{"previous_response_id", "prompt_cache_key", "prompt_cache_retention", "safety_identifier", "service_tier", "truncation"} {
		if value, exists := p.body[key]; exists {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, nil, requestError(key, key+" must be a non-empty string")
			}
			out[key] = text
		}
	}
	if include := cleanStringList(p.body["include"]); len(include) > 0 {
		out["include"] = include
	}
	if reasoning, ok := p.body["reasoning"].(map[string]any); ok {
		clean := map[string]any{}
		for _, key := range []string{"summary", "generate_summary"} {
			if value, ok := reasoning[key].(string); ok {
				clean[key] = value
			}
		}
		if len(clean) > 0 {
			out["reasoning"] = clean
		}
	}
	if text, ok := p.body["text"].(map[string]any); ok {
		if format := responsesFormat(text["format"]); format != nil {
			out["text"] = map[string]any{"format": format}
		}
	} else if format := responsesFormat(p.body["response_format"]); format != nil {
		out["text"] = map[string]any{"format": format}
	}
	if len(tools) > 0 {
		out["tools"] = tools
		if choice := responsesToolChoice(p.body, tools, aliases); choice != nil {
			out["tool_choice"] = choice
		}
	}
	if p.stream && descriptor.StreamToolCalls {
		out["stream_tool_calls"] = true
	} else if value, ok := p.body["stream_tool_calls"].(bool); ok && value && descriptor.StreamToolCalls {
		out["stream_tool_calls"] = true
	}
	return out, aliases, nil
}

func (p *RequestPlan) renderMessages(model string, descriptor modelcatalog.ModelDescriptor) (map[string]any, bool, error) {
	if p.protocol == ProtocolMessages {
		out, preserved, err := p.renderNativeMessages(model, descriptor)
		return out, preserved, err
	}
	common := p.canonical()
	messages, system := renderMessagesConversation(common.messages)
	if len(messages) == 0 {
		return nil, false, noInput(p.protocol)
	}
	maxTokens := common.maxTokens
	if maxTokens == nil {
		if descriptor.MaxCompletionTokens > 0 {
			maxTokens = descriptor.MaxCompletionTokens
		} else {
			maxTokens = 4096
		}
	}
	if err := positiveInteger(maxTokens, "max_tokens", true); err != nil {
		return nil, false, err
	}
	out := map[string]any{"model": model, "max_tokens": maxTokens, "messages": messages, "stream": p.stream}
	if len(system) == 1 {
		out["system"] = system[0].(map[string]any)["text"]
	} else if len(system) > 0 {
		out["system"] = system
	}
	if err := copyOptionalNumber(out, map[string]any{"temperature": common.temperature}, "temperature", 0, 1); err != nil {
		return nil, false, err
	}
	if err := copyOptionalNumber(out, map[string]any{"top_p": common.topP}, "top_p", 0, 1); err != nil {
		return nil, false, err
	}
	if common.user != "" {
		out["metadata"] = map[string]any{"user_id": common.user}
	}
	if normalized := responsesFormat(common.format); normalized != nil {
		if format := cleanMessagesFormat(normalized); format != nil {
			out["output_config"] = map[string]any{"format": format}
		}
	}
	if len(common.tools) > 0 {
		tools := make([]any, 0, len(common.tools))
		for _, tool := range common.tools {
			item := map[string]any{"name": tool.name, "input_schema": cloneMap(tool.parameters)}
			if tool.description != "" {
				item["description"] = tool.description
			}
			tools = append(tools, item)
		}
		out["tools"] = tools
		if choice := messagesToolChoice(p.body, tools, common.toolAliases); choice != nil {
			out["tool_choice"] = choice
		}
	}
	return out, false, nil
}

func renderMessagesConversation(messages []canonicalMessage) ([]any, []any) {
	var out, system []any
	knownCalls := map[string]bool{}
	for _, message := range messages {
		var content []any
		for _, block := range message.blocks {
			switch block.kind {
			case blockText:
				content = append(content, map[string]any{"type": "text", "text": block.text})
			case blockImage:
				if message.role != "user" || block.url == "" {
					continue
				}
				source := map[string]any{"type": "url", "url": block.url}
				if block.data != "" && block.mediaType != "" {
					source = map[string]any{"type": "base64", "media_type": block.mediaType, "data": block.data}
				}
				content = append(content, map[string]any{"type": "image", "source": source})
			case blockToolCall:
				if message.role == "assistant" && block.id != "" && block.name != "" {
					knownCalls[block.id] = true
					input := block.input
					if input == nil {
						input = decodeObject(block.arguments)
					}
					content = append(content, map[string]any{"type": "tool_use", "id": block.id, "name": block.name, "input": input})
				}
			case blockToolResult:
				if block.id != "" && knownCalls[block.id] {
					result := map[string]any{"type": "tool_result", "tool_use_id": block.id, "content": outputString(block.output, false)}
					if block.isError {
						result["is_error"] = true
					}
					content = append(content, result)
				}
			}
		}
		if len(content) == 0 {
			continue
		}
		if message.role == "system" {
			for _, part := range content {
				if object, _ := part.(map[string]any); object["type"] == "text" {
					system = append(system, object)
				}
			}
			continue
		}
		role := message.role
		if role == "tool" {
			role = "user"
		}
		if role != "user" && role != "assistant" {
			continue
		}
		out = append(out, map[string]any{"role": role, "content": content})
	}
	return out, system
}

func copySampling(out map[string]any, common canonicalRequest, maxKey string) error {
	if common.maxTokens != nil {
		if err := positiveInteger(common.maxTokens, maxKey, true); err != nil {
			return err
		}
		out[maxKey] = common.maxTokens
	}
	if common.temperature != nil {
		value, ok := jsonNumber(common.temperature)
		if !ok || !finite(value) || value < 0 || value > 2 {
			return requestError("temperature", "temperature must be a number between 0 and 2")
		}
		out["temperature"] = common.temperature
	}
	if common.topP != nil {
		value, ok := jsonNumber(common.topP)
		if !ok || !finite(value) || value < 0 || value > 1 {
			return requestError("top_p", "top_p must be a number between 0 and 1")
		}
		out["top_p"] = common.topP
	}
	return nil
}

func copyOptionalNumber(out, source map[string]any, key string, min, max float64) error {
	value, exists := source[key]
	if !exists || value == nil {
		return nil
	}
	number, ok := jsonNumber(value)
	if !ok || !finite(number) || number < min || number > max {
		return requestError(key, fmt.Sprintf("%s must be a number between %g and %g", key, min, max))
	}
	out[key] = value
	return nil
}

func positiveInteger(value any, path string, strictlyPositive bool) error {
	number, ok := jsonNumber(value)
	minimumOK := number >= 0
	if strictlyPositive {
		minimumOK = number > 0
	}
	if !ok || !finite(number) || !minimumOK || math.Trunc(number) != number {
		qualifier := "a non-negative integer"
		if strictlyPositive {
			qualifier = "a positive integer"
		}
		return requestError(path, path+" must be "+qualifier)
	}
	return nil
}

func outputString(value any, isError bool) string {
	var result string
	if text, ok := value.(string); ok {
		result = text
	} else if encoded, err := json.Marshal(value); err == nil {
		result = string(encoded)
	} else {
		result = fmt.Sprint(value)
	}
	if isError {
		return "Tool execution failed: " + result
	}
	return result
}

func decodeObject(value string) any {
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) == nil {
		return decoded
	}
	return map[string]any{"value": value}
}

func cleanStringList(raw any) []any {
	items, _ := raw.([]any)
	var out []any
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return out
}

func responsesFormat(raw any) any {
	format, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	kind := trimmedString(format["type"])
	switch kind {
	case "text", "json_object":
		return map[string]any{"type": kind}
	case "json_schema":
		if legacy, ok := format["json_schema"].(map[string]any); ok {
			format = legacy
		}
		name := trimmedString(format["name"])
		schema, ok := format["schema"].(map[string]any)
		if name == "" || !ok {
			return nil
		}
		out := map[string]any{"type": "json_schema", "name": name, "schema": cloneMap(schema)}
		if description, ok := format["description"].(string); ok {
			out["description"] = description
		}
		if strict, ok := format["strict"].(bool); ok {
			out["strict"] = strict
		}
		return out
	default:
		return nil
	}
}
