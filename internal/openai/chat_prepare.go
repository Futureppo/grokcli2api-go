package openai

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ChatChange describes a compatibility rewrite without retaining request
// values, so callers can safely expose it through diagnostic logs.
type ChatChange struct {
	Path   string
	Action string
	Reason string
}

type PreparedChat struct {
	Body    map[string]any
	Changes []ChatChange
}

type chatPreparer struct {
	body    map[string]any
	out     map[string]any
	handled map[string]bool
	changes []ChatChange
}

// PrepareChat rebuilds an OpenAI Chat Completions request using only fields
// accepted by the Grok CLI chat proxy. It never mutates body.
func PrepareChat(body map[string]any) (PreparedChat, error) {
	if err := ValidateChatRequest(body); err != nil {
		return PreparedChat{}, err
	}
	p := &chatPreparer{body: body, out: make(map[string]any), handled: make(map[string]bool)}
	if err := p.prepare(); err != nil {
		return PreparedChat{}, err
	}
	sort.SliceStable(p.changes, func(i, j int) bool {
		if p.changes[i].Path != p.changes[j].Path {
			return p.changes[i].Path < p.changes[j].Path
		}
		return p.changes[i].Action < p.changes[j].Action
	})
	return PreparedChat{Body: p.out, Changes: p.changes}, nil
}

func (p *chatPreparer) prepare() error {
	model := strings.TrimSpace(p.body["model"].(string))
	p.out["model"] = model
	p.handled["model"] = true
	if model != p.body["model"].(string) {
		p.change("model", "rewritten", "leading and trailing whitespace was removed")
	}

	messages, err := p.prepareMessages(p.body["messages"].([]any))
	if err != nil {
		return err
	}
	p.out["messages"] = messages
	p.handled["messages"] = true

	stream := false
	if raw, ok := p.body["stream"]; ok {
		var valid bool
		stream, valid = raw.(bool)
		if !valid {
			return fmt.Errorf("stream must be a boolean")
		}
	}
	p.out["stream"] = stream
	p.handled["stream"] = true

	if err := p.copyNumber("temperature", 0, 2); err != nil {
		return err
	}
	if err := p.copyNumber("top_p", 0, 1); err != nil {
		return err
	}
	if err := p.copyAliasedNumber("presence_penalty", "presencePenalty", -2, 2); err != nil {
		return err
	}
	if err := p.copyAliasedNumber("frequency_penalty", "frequencyPenalty", -2, 2); err != nil {
		return err
	}
	if err := p.prepareMaxTokens(); err != nil {
		return err
	}
	if err := p.prepareStop(); err != nil {
		return err
	}
	toolNames, err := p.prepareTools()
	if err != nil {
		return err
	}
	if err := p.prepareToolChoice(toolNames); err != nil {
		return err
	}
	if err := p.prepareReasoningEffort(); err != nil {
		return err
	}
	if err := p.prepareResponseFormat(); err != nil {
		return err
	}
	if err := p.prepareSearchParameters(); err != nil {
		return err
	}
	if err := p.prepareUserID(); err != nil {
		return err
	}

	for _, key := range []string{"prompt_cache_key", "previous_response_id"} {
		if _, ok := p.body[key]; ok {
			p.handled[key] = true
			p.change(key, "removed", "local routing field is not accepted by the upstream chat API")
		}
	}

	if IsStrictCompatibilityModel(model) {
		for _, key := range []string{"presence_penalty", "frequency_penalty", "stop"} {
			if _, ok := p.out[key]; ok {
				delete(p.out, key)
				p.change(key, "removed", "model family rejects this optional parameter")
			}
		}
	}

	keys := make([]string, 0, len(p.body))
	for key := range p.body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !p.handled[key] {
			p.change(key, "removed", "field is not in the upstream chat request whitelist")
		}
	}
	return nil
}

func (p *chatPreparer) copyNumber(key string, min, max float64) error {
	raw, ok := p.body[key]
	p.handled[key] = true
	if !ok {
		return nil
	}
	value, valid := chatNumber(raw)
	if !valid || !finiteNumber(value) || value < min || value > max {
		return fmt.Errorf("%s must be a number between %s and %s", key, formatNumber(min), formatNumber(max))
	}
	p.out[key] = raw
	return nil
}

func (p *chatPreparer) copyAliasedNumber(key, alias string, min, max float64) error {
	p.handled[key], p.handled[alias] = true, true
	raw, ok := p.body[key]
	if !ok {
		raw, ok = p.body[alias]
		if ok {
			p.change(alias, "rewritten", "mapped to "+key)
		}
	} else if _, aliasExists := p.body[alias]; aliasExists {
		p.change(alias, "removed", "canonical "+key+" takes precedence")
	}
	if !ok {
		return nil
	}
	value, valid := chatNumber(raw)
	if !valid || !finiteNumber(value) || value < min || value > max {
		return fmt.Errorf("%s must be a number between %s and %s", key, formatNumber(min), formatNumber(max))
	}
	p.out[key] = raw
	return nil
}

func (p *chatPreparer) prepareMaxTokens() error {
	aliases := []string{"max_completion_tokens", "maxTokens"}
	p.handled["max_tokens"] = true
	for _, alias := range aliases {
		p.handled[alias] = true
	}
	raw, ok := p.body["max_tokens"]
	if ok {
		for _, alias := range aliases {
			if _, exists := p.body[alias]; exists {
				p.change(alias, "removed", "canonical max_tokens takes precedence")
			}
		}
	} else {
		for _, alias := range aliases {
			if value, exists := p.body[alias]; exists {
				raw, ok = value, true
				p.change(alias, "rewritten", "mapped to max_tokens")
				break
			}
		}
	}
	if !ok {
		return nil
	}
	value, valid := chatNumber(raw)
	if !valid || !finiteNumber(value) || value <= 0 || math.Trunc(value) != value {
		return fmt.Errorf("max_tokens must be a positive integer")
	}
	p.out["max_tokens"] = raw
	return nil
}

func (p *chatPreparer) prepareStop() error {
	p.handled["stop"], p.handled["stop_sequences"] = true, true
	raw, ok := p.body["stop"]
	if ok {
		if _, exists := p.body["stop_sequences"]; exists {
			p.change("stop_sequences", "removed", "canonical stop takes precedence")
		}
	} else if value, exists := p.body["stop_sequences"]; exists {
		raw, ok = value, true
		p.change("stop_sequences", "rewritten", "mapped to stop")
	}
	if !ok {
		return nil
	}
	if err := validateStringOrStringArray(raw, "stop"); err != nil {
		return err
	}
	p.out["stop"] = cloneJSONValue(raw)
	return nil
}

func (p *chatPreparer) prepareReasoningEffort() error {
	p.handled["reasoning_effort"], p.handled["reasoning"] = true, true
	raw, ok := p.body["reasoning_effort"]
	if ok {
		if _, exists := p.body["reasoning"]; exists {
			p.change("reasoning", "removed", "canonical reasoning_effort takes precedence")
		}
	} else if reasoning, exists := p.body["reasoning"]; exists {
		object, valid := reasoning.(map[string]any)
		if !valid {
			return fmt.Errorf("reasoning must be an object")
		}
		if effort, exists := object["effort"]; exists {
			raw, ok = effort, true
			p.change("reasoning.effort", "rewritten", "mapped to reasoning_effort")
		} else {
			p.change("reasoning", "removed", "reasoning.effort is absent")
		}
		p.recordUnknownObjectFields(object, map[string]bool{"effort": true}, "reasoning")
	}
	if !ok {
		return nil
	}
	effort, valid := raw.(string)
	if !valid {
		return fmt.Errorf("reasoning_effort must be a string")
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "adaptive" {
		effort = "medium"
		p.change("reasoning_effort", "rewritten", "adaptive is normalized to medium")
	}
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		p.out["reasoning_effort"] = effort
		return nil
	default:
		return fmt.Errorf("reasoning_effort must be one of none, minimal, low, medium, high, xhigh")
	}
}

func (p *chatPreparer) prepareUserID() error {
	for _, key := range []string{"user_id", "user", "metadata"} {
		p.handled[key] = true
	}
	metadataRaw, metadataExists := p.body["metadata"]
	metadata, metadataIsObject := metadataRaw.(map[string]any)
	metadataUser, metadataHasUser := any(nil), false
	if metadataExists {
		if metadataIsObject {
			metadataUser, metadataHasUser = metadata["user_id"]
			p.recordUnknownObjectFields(metadata, map[string]bool{"user_id": true}, "metadata")
			if !metadataHasUser {
				p.change("metadata", "removed", "metadata does not contain a usable upstream user_id field")
			}
		} else {
			p.change("metadata", "removed", "metadata is not an object and is not accepted by the upstream chat API")
		}
	}
	var raw any
	var source string
	found := false
	if value, ok := p.body["user_id"]; ok {
		raw, source, found = value, "user_id", true
		if _, exists := p.body["user"]; exists {
			p.change("user", "removed", "canonical user_id takes precedence")
		}
		if metadataHasUser {
			p.change("metadata.user_id", "removed", "canonical user_id takes precedence")
		}
	} else if value, ok := p.body["user"]; ok {
		raw, source, found = value, "user", true
		p.change("user", "rewritten", "mapped to user_id")
		if metadataHasUser {
			p.change("metadata.user_id", "removed", "user takes precedence")
		}
	} else if metadataHasUser {
		raw, source, found = metadataUser, "metadata.user_id", true
		p.change(source, "rewritten", "mapped to user_id")
	}
	if !found {
		return nil
	}
	value, ok := raw.(string)
	if !ok || strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must be a non-empty string", source)
	}
	p.out["user_id"] = strings.TrimSpace(value)
	return nil
}

func (p *chatPreparer) prepareMessages(messages []any) ([]any, error) {
	out := make([]any, 0, len(messages))
	knownCalls := make(map[string]bool)
	answeredCalls := make(map[string]bool)
	for index, raw := range messages {
		path := fmt.Sprintf("messages[%d]", index)
		message, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object", path)
		}
		role, ok := message["role"].(string)
		if !ok || strings.TrimSpace(role) == "" {
			return nil, fmt.Errorf("%s.role must be a non-empty string", path)
		}
		role = strings.ToLower(strings.TrimSpace(role))
		if role == "developer" {
			role = "system"
			p.change(path+".role", "rewritten", "developer role is mapped to system")
		}
		switch role {
		case "system", "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("%s.role %q is not supported", path, role)
		}
		if role != "tool" {
			for callID := range knownCalls {
				if !answeredCalls[callID] {
					return nil, fmt.Errorf("%s cannot follow unresolved tool call %q", path, callID)
				}
			}
		}
		clean := map[string]any{"role": role}
		if name, exists := message["name"]; exists {
			value, valid := name.(string)
			if !valid || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("%s.name must be a non-empty string", path)
			}
			clean["name"] = strings.TrimSpace(value)
		}
		content, contentExists := message["content"]
		if contentExists && content != nil {
			prepared, err := p.prepareMessageContent(content, path+".content", role == "user")
			if err != nil {
				return nil, err
			}
			clean["content"] = prepared
		}
		if reasoning, exists := message["reasoning_content"]; exists {
			if role != "assistant" {
				return nil, fmt.Errorf("%s.reasoning_content is only valid for assistant messages", path)
			}
			value, valid := reasoning.(string)
			if !valid {
				return nil, fmt.Errorf("%s.reasoning_content must be a string", path)
			}
			clean["reasoning_content"] = value
		}
		if calls, exists := message["tool_calls"]; exists {
			if role != "assistant" {
				return nil, fmt.Errorf("%s.tool_calls is only valid for assistant messages", path)
			}
			prepared, ids, err := p.prepareMessageToolCalls(calls, path+".tool_calls")
			if err != nil {
				return nil, err
			}
			for _, id := range ids {
				if knownCalls[id] {
					return nil, fmt.Errorf("%s contains duplicate tool call id %q", path, id)
				}
				knownCalls[id] = true
			}
			clean["tool_calls"] = prepared
		}
		if role == "tool" {
			id, valid := message["tool_call_id"].(string)
			id = strings.TrimSpace(id)
			if !valid || id == "" {
				return nil, fmt.Errorf("%s.tool_call_id must be a non-empty string", path)
			}
			if !knownCalls[id] {
				return nil, fmt.Errorf("%s.tool_call_id %q does not reference an earlier assistant tool call", path, id)
			}
			if answeredCalls[id] {
				return nil, fmt.Errorf("%s.tool_call_id %q already has a tool result", path, id)
			}
			answeredCalls[id] = true
			clean["tool_call_id"] = id
		} else if _, exists := message["tool_call_id"]; exists {
			return nil, fmt.Errorf("%s.tool_call_id is only valid for tool messages", path)
		}
		_, hasContent := clean["content"]
		if role != "assistant" && !hasContent {
			return nil, fmt.Errorf("%s.content is required", path)
		}
		if role == "assistant" {
			_, hasCalls := clean["tool_calls"]
			if !hasCalls && !hasContent {
				return nil, fmt.Errorf("%s must contain content or tool_calls", path)
			}
		}
		allowed := map[string]bool{"role": true, "content": true, "name": true, "reasoning_content": true, "tool_calls": true, "tool_call_id": true}
		p.recordUnknownObjectFields(message, allowed, path)
		out = append(out, clean)
	}
	for callID := range knownCalls {
		if !answeredCalls[callID] {
			return nil, fmt.Errorf("messages end with unresolved tool call %q", callID)
		}
	}
	return out, nil
}

func (p *chatPreparer) prepareMessageContent(raw any, path string, allowImages bool) (any, error) {
	if text, ok := raw.(string); ok {
		return text, nil
	}
	parts, ok := raw.([]any)
	if !ok || len(parts) == 0 {
		return nil, fmt.Errorf("%s must be a string or non-empty array", path)
	}
	out := make([]any, 0, len(parts))
	for index, rawPart := range parts {
		partPath := fmt.Sprintf("%s[%d]", path, index)
		part, ok := rawPart.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object", partPath)
		}
		kind, _ := part["type"].(string)
		switch kind {
		case "text":
			text, valid := part["text"].(string)
			if !valid {
				return nil, fmt.Errorf("%s.text must be a string", partPath)
			}
			out = append(out, map[string]any{"type": "text", "text": text})
			p.recordUnknownObjectFields(part, map[string]bool{"type": true, "text": true}, partPath)
		case "image_url":
			if !allowImages {
				return nil, fmt.Errorf("%s.type image_url is only valid for user messages", partPath)
			}
			image, err := p.prepareImageURL(part["image_url"], partPath+".image_url")
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": image})
			p.recordUnknownObjectFields(part, map[string]bool{"type": true, "image_url": true}, partPath)
		case "image":
			if !allowImages {
				return nil, fmt.Errorf("%s.type image is only valid for user messages", partPath)
			}
			image, err := p.prepareAnthropicImage(part, partPath)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": image})
			p.recordUnknownObjectFields(part, map[string]bool{"type": true, "source": true}, partPath)
		default:
			return nil, fmt.Errorf("%s.type %q is not supported", partPath, kind)
		}
	}
	return out, nil
}

func (p *chatPreparer) prepareAnthropicImage(block map[string]any, path string) (map[string]any, error) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s.source must be an object", path)
	}
	var url string
	switch sourceType, _ := source["type"].(string); sourceType {
	case "url":
		value, ok := source["url"].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s.source.url must be a non-empty string", path)
		}
		url = value
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if strings.TrimSpace(mediaType) == "" || strings.TrimSpace(data) == "" {
			return nil, fmt.Errorf("%s.source media_type and data are required for base64 images", path)
		}
		url = "data:" + mediaType + ";base64," + data
	default:
		return nil, fmt.Errorf("%s.source.type %q is not supported", path, sourceType)
	}
	p.recordUnknownObjectFields(source, map[string]bool{"type": true, "url": true, "media_type": true, "data": true}, path+".source")
	p.change(path, "rewritten", "Anthropic image block is normalized to OpenAI image_url content")
	return map[string]any{"url": url}, nil
}

func (p *chatPreparer) prepareImageURL(raw any, path string) (map[string]any, error) {
	if value, ok := raw.(string); ok && strings.TrimSpace(value) != "" {
		p.change(path, "rewritten", "string image URL is normalized to an object")
		return map[string]any{"url": value}, nil
	}
	image, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object with a URL", path)
	}
	url, ok := image["url"].(string)
	if !ok || strings.TrimSpace(url) == "" {
		return nil, fmt.Errorf("%s.url must be a non-empty string", path)
	}
	out := map[string]any{"url": url}
	if detail, exists := image["detail"]; exists {
		value, valid := detail.(string)
		if !valid || (value != "auto" && value != "low" && value != "high") {
			return nil, fmt.Errorf("%s.detail must be one of auto, low, high", path)
		}
		out["detail"] = value
	}
	p.recordUnknownObjectFields(image, map[string]bool{"url": true, "detail": true}, path)
	return out, nil
}

func (p *chatPreparer) prepareMessageToolCalls(raw any, path string) ([]any, []string, error) {
	calls, ok := raw.([]any)
	if !ok || len(calls) == 0 {
		return nil, nil, fmt.Errorf("%s must be a non-empty array", path)
	}
	out := make([]any, 0, len(calls))
	ids := make([]string, 0, len(calls))
	for index, rawCall := range calls {
		callPath := fmt.Sprintf("%s[%d]", path, index)
		call, ok := rawCall.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s must be an object", callPath)
		}
		id, _ := call["id"].(string)
		if strings.TrimSpace(id) == "" {
			return nil, nil, fmt.Errorf("%s.id must be a non-empty string", callPath)
		}
		kind, _ := call["type"].(string)
		if kind != "function" {
			return nil, nil, fmt.Errorf("%s.type must be function", callPath)
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s.function must be an object", callPath)
		}
		name, _ := function["name"].(string)
		arguments, argumentsOK := function["arguments"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, nil, fmt.Errorf("%s.function.name must be a non-empty string", callPath)
		}
		if !argumentsOK {
			return nil, nil, fmt.Errorf("%s.function.arguments must be a JSON string", callPath)
		}
		p.recordUnknownObjectFields(call, map[string]bool{"id": true, "type": true, "function": true}, callPath)
		p.recordUnknownObjectFields(function, map[string]bool{"name": true, "arguments": true}, callPath+".function")
		out = append(out, map[string]any{"id": strings.TrimSpace(id), "type": "function", "function": map[string]any{"name": strings.TrimSpace(name), "arguments": arguments}})
		ids = append(ids, strings.TrimSpace(id))
	}
	return out, ids, nil
}

func (p *chatPreparer) prepareTools() (map[string]bool, error) {
	p.handled["tools"], p.handled["functions"] = true, true
	raw, ok := p.body["tools"]
	legacy := false
	if ok {
		if _, exists := p.body["functions"]; exists {
			p.change("functions", "removed", "canonical tools takes precedence")
		}
	} else if value, exists := p.body["functions"]; exists {
		raw, ok, legacy = value, true, true
		p.change("functions", "rewritten", "mapped to tools")
	}
	names := make(map[string]bool)
	if !ok {
		return names, nil
	}
	items, valid := raw.([]any)
	if !valid {
		return nil, fmt.Errorf("tools must be an array")
	}
	if len(items) == 0 {
		path := "tools"
		if legacy {
			path = "functions"
		}
		p.change(path, "removed", "empty tool list is equivalent to an omitted optional field")
		return names, nil
	}
	out := make([]any, 0, len(items))
	for index, rawItem := range items {
		path := fmt.Sprintf("tools[%d]", index)
		functionPath := path + ".function"
		item, valid := rawItem.(map[string]any)
		if !valid {
			return nil, fmt.Errorf("%s must be an object", path)
		}
		var function map[string]any
		if legacy {
			function = item
			path = fmt.Sprintf("functions[%d]", index)
			functionPath = path
		} else {
			kind, _ := item["type"].(string)
			if kind != "function" {
				return nil, fmt.Errorf("%s.type must be function", path)
			}
			function, valid = item["function"].(map[string]any)
			if !valid {
				return nil, fmt.Errorf("%s.function must be an object", path)
			}
			p.recordUnknownObjectFields(item, map[string]bool{"type": true, "function": true}, path)
		}
		name, _ := function["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%s.name must be a non-empty string", functionPath)
		}
		if names[name] {
			return nil, fmt.Errorf("%s.name %q is duplicated", functionPath, name)
		}
		parameters, valid := function["parameters"].(map[string]any)
		if !valid {
			return nil, fmt.Errorf("%s.parameters must be an object", functionPath)
		}
		cleanFunction := map[string]any{"name": name, "parameters": cloneJSONValue(parameters)}
		if description, exists := function["description"]; exists {
			value, valid := description.(string)
			if !valid {
				return nil, fmt.Errorf("%s.description must be a string", functionPath)
			}
			cleanFunction["description"] = value
		}
		if strict, exists := function["strict"]; exists {
			value, valid := strict.(bool)
			if !valid {
				return nil, fmt.Errorf("%s.strict must be a boolean", functionPath)
			}
			cleanFunction["strict"] = value
		}
		p.recordUnknownObjectFields(function, map[string]bool{"name": true, "description": true, "parameters": true, "strict": true}, functionPath)
		out = append(out, map[string]any{"type": "function", "function": cleanFunction})
		names[name] = true
	}
	p.out["tools"] = out
	return names, nil
}

func (p *chatPreparer) prepareToolChoice(toolNames map[string]bool) error {
	p.handled["tool_choice"], p.handled["function_call"] = true, true
	raw, ok := p.body["tool_choice"]
	legacy := false
	if ok {
		if _, exists := p.body["function_call"]; exists {
			p.change("function_call", "removed", "canonical tool_choice takes precedence")
		}
	} else if value, exists := p.body["function_call"]; exists {
		raw, ok, legacy = value, true, true
		p.change("function_call", "rewritten", "mapped to tool_choice")
	}
	if !ok {
		return nil
	}
	if value, valid := raw.(string); valid {
		if value != "none" && value != "auto" && value != "required" {
			return fmt.Errorf("tool_choice must be one of none, auto, required or a function object")
		}
		if value == "required" && len(toolNames) == 0 {
			return fmt.Errorf("tool_choice required needs at least one tool")
		}
		p.out["tool_choice"] = value
		return nil
	}
	choice, valid := raw.(map[string]any)
	if !valid {
		return fmt.Errorf("tool_choice must be a string or object")
	}
	var name string
	if legacy {
		name, _ = choice["name"].(string)
		p.recordUnknownObjectFields(choice, map[string]bool{"name": true}, "function_call")
	} else {
		kind, _ := choice["type"].(string)
		if kind != "function" {
			return fmt.Errorf("tool_choice.type must be function")
		}
		function, ok := choice["function"].(map[string]any)
		if !ok {
			return fmt.Errorf("tool_choice.function must be an object")
		}
		name, _ = function["name"].(string)
		p.recordUnknownObjectFields(choice, map[string]bool{"type": true, "function": true}, "tool_choice")
		p.recordUnknownObjectFields(function, map[string]bool{"name": true}, "tool_choice.function")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("tool_choice.function.name must be a non-empty string")
	}
	if !toolNames[name] {
		return fmt.Errorf("tool_choice.function.name %q does not match a declared tool", name)
	}
	p.out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
	return nil
}

func (p *chatPreparer) prepareResponseFormat() error {
	p.handled["response_format"] = true
	raw, ok := p.body["response_format"]
	if !ok {
		return nil
	}
	format, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("response_format must be an object")
	}
	kind, _ := format["type"].(string)
	switch kind {
	case "text", "json_object":
		p.out["response_format"] = map[string]any{"type": kind}
		p.recordUnknownObjectFields(format, map[string]bool{"type": true}, "response_format")
		return nil
	case "json_schema":
		schema, ok := format["json_schema"].(map[string]any)
		if !ok {
			return fmt.Errorf("response_format.json_schema must be an object")
		}
		name, _ := schema["name"].(string)
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("response_format.json_schema.name must be a non-empty string")
		}
		definition, ok := schema["schema"].(map[string]any)
		if !ok {
			return fmt.Errorf("response_format.json_schema.schema must be an object")
		}
		clean := map[string]any{"name": strings.TrimSpace(name), "schema": cloneJSONValue(definition)}
		if description, exists := schema["description"]; exists {
			value, valid := description.(string)
			if !valid {
				return fmt.Errorf("response_format.json_schema.description must be a string")
			}
			clean["description"] = value
		}
		if strict, exists := schema["strict"]; exists {
			value, valid := strict.(bool)
			if !valid {
				return fmt.Errorf("response_format.json_schema.strict must be a boolean")
			}
			clean["strict"] = value
		}
		p.recordUnknownObjectFields(format, map[string]bool{"type": true, "json_schema": true}, "response_format")
		p.recordUnknownObjectFields(schema, map[string]bool{"name": true, "description": true, "schema": true, "strict": true}, "response_format.json_schema")
		p.out["response_format"] = map[string]any{"type": "json_schema", "json_schema": clean}
		return nil
	default:
		return fmt.Errorf("response_format.type must be one of text, json_object, json_schema")
	}
}

func (p *chatPreparer) prepareSearchParameters() error {
	p.handled["search_parameters"] = true
	raw, ok := p.body["search_parameters"]
	if !ok {
		return nil
	}
	parameters, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("search_parameters must be an object")
	}
	boolFields := map[string]bool{"reasoning_only": true, "no_visible_content": true, "web": true, "safe_search": true, "news": true, "rss": true, "links": true, "return_citations": true}
	arrayFields := map[string]bool{"included_x_handles": true, "x_handles": true, "excluded_x_handles": true, "excluded_websites": true, "allowed_websites": true}
	numberFields := map[string]bool{"post_favorite_count": true, "post_view_count": true, "max_search_results": true}
	dateFields := map[string]bool{"from_date": true, "to_date": true}
	allowed := make(map[string]bool)
	out := make(map[string]any)
	for key := range boolFields {
		allowed[key] = true
	}
	for key := range arrayFields {
		allowed[key] = true
	}
	for key := range numberFields {
		allowed[key] = true
	}
	for key := range dateFields {
		allowed[key] = true
	}
	for key, value := range parameters {
		path := "search_parameters." + key
		switch {
		case boolFields[key]:
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("%s must be a boolean", path)
			}
		case arrayFields[key]:
			if err := validateStringArray(value, path); err != nil {
				return err
			}
		case numberFields[key]:
			number, ok := chatNumber(value)
			if !ok || !finiteNumber(number) || number < 0 || math.Trunc(number) != number {
				return fmt.Errorf("%s must be a non-negative integer", path)
			}
		case dateFields[key]:
			if text, ok := value.(string); !ok || strings.TrimSpace(text) == "" {
				return fmt.Errorf("%s must be a non-empty string", path)
			}
		default:
			continue
		}
		out[key] = cloneJSONValue(value)
	}
	p.recordUnknownObjectFields(parameters, allowed, "search_parameters")
	if len(out) > 0 {
		p.out["search_parameters"] = out
	} else {
		p.change("search_parameters", "removed", "no supported search parameters remain")
	}
	return nil
}

func (p *chatPreparer) recordUnknownObjectFields(object map[string]any, allowed map[string]bool, base string) {
	keys := make([]string, 0, len(object))
	for key := range object {
		if !allowed[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		p.change(base+"."+key, "removed", "nested field is not accepted by the upstream chat API")
	}
}

func (p *chatPreparer) change(path, action, reason string) {
	p.changes = append(p.changes, ChatChange{Path: path, Action: action, Reason: reason})
}

func validateStringOrStringArray(value any, path string) error {
	if _, ok := value.(string); ok {
		return nil
	}
	return validateStringArray(value, path)
}

func validateStringArray(value any, path string) error {
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s must be a string array", path)
	}
	for index, item := range items {
		if _, ok := item.(string); !ok {
			return fmt.Errorf("%s[%d] must be a string", path, index)
		}
	}
	return nil
}

func chatNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	default:
		return 0, false
	}
}

func formatNumber(value float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", value), "0"), ".")
}

func finiteNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = cloneJSONValue(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = cloneJSONValue(item)
		}
		return out
	case json.Number:
		return typed
	default:
		return typed
	}
}
