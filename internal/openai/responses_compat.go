package openai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const maxToolAliasLength = 128

type toolKind uint8

const (
	functionTool toolKind = iota
	customTool
	toolSearchTool
)

type toolIdentity struct {
	Kind      toolKind
	Name      string
	Namespace string
	Execution string
}

func (t toolIdentity) key() string {
	return fmt.Sprintf("%d\x00%s\x00%s", t.Kind, t.Namespace, t.Name)
}

// ResponsesCompatibility contains only request-local mappings. It is safe to
// create one per request and intentionally has no process-global state.
type ResponsesCompatibility struct {
	aliases         map[string]toolIdentity
	originalAliases map[string]string
	streamCalls     map[string]*streamToolCall
}

type streamToolCall struct {
	identity  toolIdentity
	arguments strings.Builder
}

type toolSource struct {
	value any
	force bool
}

// StreamEvent is a translated Responses API SSE event.
type StreamEvent struct {
	Event string
	Data  []byte
}

// PrepareCompatibleResponses maps current OpenAI/Codex Responses request
// shapes to the subset understood by the Grok CLI upstream.
func PrepareCompatibleResponses(body map[string]any) (map[string]any, *ResponsesCompatibility, error) {
	out := PrepareResponses(body)
	compat := &ResponsesCompatibility{
		aliases:         make(map[string]toolIdentity),
		originalAliases: make(map[string]string),
		streamCalls:     make(map[string]*streamToolCall),
	}

	sources := make([]toolSource, 0)
	if tools, ok := out["tools"].([]any); ok {
		for _, tool := range tools {
			sources = append(sources, toolSource{value: tool})
		}
	}

	if input, ok := out["input"].([]any); ok {
		rewritten := make([]any, 0, len(input))
		for index, raw := range input {
			item, extra, err := compat.normalizeInputItem(raw, index)
			if err != nil {
				return nil, nil, err
			}
			sources = append(sources, extra...)
			if item != nil {
				rewritten = append(rewritten, item)
			}
		}
		out["input"] = rewritten
	}

	tools, err := compat.normalizeToolSources(sources)
	if err != nil {
		return nil, nil, err
	}
	if len(tools) > 0 || out["tools"] != nil {
		out["tools"] = tools
	}
	compat.normalizeToolChoice(out)
	return out, compat, nil
}

func (c *ResponsesCompatibility) normalizeInputItem(raw any, index int) (any, []toolSource, error) {
	item, ok := raw.(map[string]any)
	if !ok {
		return raw, nil, nil
	}
	kind := String(item, "type", "")
	if kind == "" {
		return item, nil, nil
	}
	switch kind {
	case "message", "reasoning", "function_call_output", "web_search_call", "compaction", "context_compaction":
		return item, nil, nil
	case "function_call":
		out := clone(item)
		identity := toolIdentity{Kind: functionTool, Name: String(item, "name", ""), Namespace: String(item, "namespace", "")}
		if identity.Name == "" {
			return nil, nil, fmt.Errorf("input[%d] function_call is missing name", index)
		}
		alias := c.alias(identity)
		out["name"] = alias
		delete(out, "namespace")
		return out, nil, nil
	case "additional_tools":
		return nil, toolSources(item["tools"], true), nil
	case "tool_search_call":
		return nil, nil, nil
	case "tool_search_output":
		message := map[string]any{
			"type": "message", "role": "developer",
			"content": []any{map[string]any{"type": "input_text", "text": "Tool search completed; the selected tools are now available."}},
		}
		return message, toolSources(item["tools"], true), nil
	case "custom_tool_call":
		out := clone(item)
		identity := toolIdentity{Kind: customTool, Name: String(item, "name", ""), Namespace: String(item, "namespace", "")}
		if identity.Name == "" {
			return nil, nil, fmt.Errorf("input[%d] custom_tool_call is missing name", index)
		}
		out["type"] = "function_call"
		out["name"] = c.alias(identity)
		out["arguments"] = encodeCustomInput(item["input"])
		delete(out, "namespace")
		delete(out, "input")
		return out, nil, nil
	case "custom_tool_call_output":
		out := clone(item)
		out["type"] = "function_call_output"
		delete(out, "name")
		return out, nil, nil
	case "agent_message":
		content, ok := agentMessageContent(item["content"])
		if !ok {
			return nil, nil, fmt.Errorf("input[%d] agent_message contains encrypted or invalid content", index)
		}
		label := strings.TrimSpace(String(item, "author", "agent") + " -> " + String(item, "recipient", "recipient"))
		return map[string]any{
			"type": "message", "role": "developer",
			"content": []any{map[string]any{"type": "input_text", "text": "Agent message (" + label + "):\n" + content}},
		}, nil, nil
	case "local_shell_call":
		action, err := json.Marshal(item["action"])
		if err != nil {
			return nil, nil, fmt.Errorf("input[%d] local_shell_call action is invalid: %w", index, err)
		}
		return map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "Local shell call (" + String(item, "status", "unknown") + "): " + string(action)}},
		}, nil, nil
	case "mcp_tool_call_output":
		payload, err := json.Marshal(item["output"])
		if err != nil {
			return nil, nil, fmt.Errorf("input[%d] mcp_tool_call_output is invalid: %w", index, err)
		}
		return map[string]any{
			"type": "message", "role": "developer",
			"content": []any{map[string]any{"type": "input_text", "text": "MCP tool output for call " + String(item, "call_id", "unknown") + ": " + string(payload)}},
		}, nil, nil
	case "compaction_trigger":
		return nil, nil, fmt.Errorf("input[%d] type compaction_trigger is not supported by the upstream", index)
	default:
		return nil, nil, fmt.Errorf("unsupported input item type at input[%d]: %s", index, kind)
	}
}

func toolSources(raw any, force bool) []toolSource {
	tools, _ := raw.([]any)
	out := make([]toolSource, 0, len(tools))
	for _, tool := range tools {
		out = append(out, toolSource{value: tool, force: force})
	}
	return out
}

func (c *ResponsesCompatibility) normalizeToolSources(sources []toolSource) ([]any, error) {
	clientSearch := false
	for _, source := range sources {
		tool, _ := source.value.(map[string]any)
		if String(tool, "type", "") == "tool_search" && strings.EqualFold(String(tool, "execution", "client"), "client") {
			clientSearch = true
			break
		}
	}

	normalized := make([]any, 0, len(sources))
	positions := make(map[string]int)
	deferredDescriptions := make([]string, 0)
	searchTools := make([]map[string]any, 0)
	add := func(tool map[string]any) {
		name := String(tool, "name", "")
		if name == "" {
			name = String(tool, "server_label", "")
		}
		if name == "" {
			normalized = append(normalized, tool)
			return
		}
		key := String(tool, "type", "") + "\x00" + name
		if index, exists := positions[key]; exists {
			normalized[index] = tool
			return
		}
		positions[key] = len(normalized)
		normalized = append(normalized, tool)
	}

	var visit func(map[string]any, string, bool) error
	visit = func(tool map[string]any, namespace string, force bool) error {
		kind := String(tool, "type", "")
		switch kind {
		case "function":
			name := String(tool, "name", "")
			if name == "" {
				return fmt.Errorf("function tool is missing name")
			}
			if deferred, _ := tool["defer_loading"].(bool); deferred && clientSearch && !force {
				deferredDescriptions = append(deferredDescriptions, describeDeferred(namespace, name, String(tool, "description", "")))
				return nil
			}
			out := clone(tool)
			identity := toolIdentity{Kind: functionTool, Name: name, Namespace: namespace}
			out["name"] = c.alias(identity)
			delete(out, "defer_loading")
			add(out)
			return nil
		case "namespace":
			name := String(tool, "name", "")
			if name == "" {
				return fmt.Errorf("namespace tool is missing name")
			}
			deferredDescriptions = append(deferredDescriptions, describeDeferred("", name, String(tool, "description", "")))
			children, ok := tool["tools"].([]any)
			if !ok {
				return fmt.Errorf("namespace %q tools must be an array", name)
			}
			for _, rawChild := range children {
				child, ok := rawChild.(map[string]any)
				if !ok {
					return fmt.Errorf("namespace %q contains a non-object tool", name)
				}
				if String(child, "type", "") != "function" {
					return fmt.Errorf("namespace %q contains unsupported tool type %q", name, String(child, "type", ""))
				}
				if err := visit(child, name, force); err != nil {
					return err
				}
			}
			return nil
		case "tool_search":
			if strings.EqualFold(String(tool, "execution", "client"), "client") {
				searchTools = append(searchTools, tool)
				return nil
			}
			// Hosted tool search cannot be executed by this proxy. Because
			// clientSearch is false, deferred definitions are eagerly exposed.
			return nil
		case "custom":
			name := String(tool, "name", "")
			if name == "" {
				return fmt.Errorf("custom tool is missing name")
			}
			identity := toolIdentity{Kind: customTool, Name: name, Namespace: namespace}
			description := String(tool, "description", "")
			if description != "" {
				description += "\n"
			}
			description += "Provide the custom tool input in the input string field."
			add(map[string]any{
				"type": "function", "name": c.alias(identity), "description": description,
				"parameters": map[string]any{
					"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}},
					"required": []any{"input"}, "additionalProperties": false,
				},
			})
			return nil
		case "web_search", "x_search", "collections_search", "file_search", "code_execution", "code_interpreter", "mcp", "shell":
			add(clone(tool))
			return nil
		default:
			return fmt.Errorf("unsupported tool type: %s", kind)
		}
	}

	for _, source := range sources {
		tool, ok := source.value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools entries must be objects")
		}
		if err := visit(tool, "", source.force); err != nil {
			return nil, err
		}
	}
	for _, search := range searchTools {
		identity := toolIdentity{Kind: toolSearchTool, Name: "tool_search", Execution: "client"}
		description := String(search, "description", "Search for tools needed to continue the task.")
		if len(deferredDescriptions) > 0 {
			description += "\nAvailable deferred tool groups:\n- " + strings.Join(deferredDescriptions, "\n- ")
			if len(description) > 16<<10 {
				description = description[:16<<10]
			}
		}
		parameters, ok := search["parameters"].(map[string]any)
		if !ok {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
		}
		add(map[string]any{"type": "function", "name": c.alias(identity), "description": description, "parameters": parameters})
	}
	return normalized, nil
}

func describeDeferred(namespace, name, description string) string {
	full := namespace + name
	if namespace != "" {
		full = namespace + "/" + name
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return full
	}
	if len(description) > 240 {
		description = description[:240]
	}
	return full + ": " + description
}

func (c *ResponsesCompatibility) alias(identity toolIdentity) string {
	key := identity.key()
	if alias, ok := c.originalAliases[key]; ok {
		return alias
	}
	base := identity.Namespace + identity.Name
	if identity.Kind == toolSearchTool {
		base = "grokcli2api_tool_search"
	}
	if base == "" {
		base = "grokcli2api_tool"
	}
	alias := base
	if len(alias) > maxToolAliasLength {
		alias = alias[:maxToolAliasLength-11] + "__" + shortHash(key)
	}
	if existing, collision := c.aliases[alias]; collision && existing.key() != key {
		limit := maxToolAliasLength - 11
		if len(base) < limit {
			limit = len(base)
		}
		alias = base[:limit] + "__" + shortHash(key)
	}
	c.aliases[alias] = identity
	c.originalAliases[key] = alias
	return alias
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:9]
}

func (c *ResponsesCompatibility) normalizeToolChoice(body map[string]any) {
	choice, ok := body["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	name := String(choice, "name", "")
	if function, ok := choice["function"].(map[string]any); ok {
		name = String(function, "name", name)
		if name != "" {
			identity := toolIdentity{Kind: functionTool, Name: name, Namespace: String(function, "namespace", "")}
			function["name"] = c.alias(identity)
			delete(function, "namespace")
		}
		return
	}
	if name != "" {
		kind := functionTool
		switch String(choice, "type", "") {
		case "custom":
			kind = customTool
			choice["type"] = "function"
		case "tool_search":
			kind = toolSearchTool
			choice["type"] = "function"
		}
		identity := toolIdentity{Kind: kind, Name: name, Namespace: String(choice, "namespace", ""), Execution: "client"}
		choice["name"] = c.alias(identity)
		delete(choice, "namespace")
	}
}

func encodeCustomInput(value any) string {
	encoded, _ := json.Marshal(map[string]any{"input": value})
	return string(encoded)
}

func decodeCustomInput(value any) string {
	text, _ := value.(string)
	var wrapper map[string]any
	if json.Unmarshal([]byte(text), &wrapper) == nil {
		if input, ok := wrapper["input"].(string); ok {
			return input
		}
	}
	return text
}

func decodeArguments(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) == nil {
		return decoded
	}
	return map[string]any{"input": text}
}

func agentMessageContent(raw any) (string, bool) {
	items, ok := raw.([]any)
	if !ok {
		return "", false
	}
	parts := make([]string, 0, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || String(item, "type", "") != "input_text" {
			return "", false
		}
		parts = append(parts, String(item, "text", ""))
	}
	return strings.Join(parts, "\n"), true
}

// NormalizeResponse restores namespaced, custom, and tool-search calls after
// applying the ordinary OpenAI Responses envelope defaults.
func (c *ResponsesCompatibility) NormalizeResponse(raw map[string]any, fallbackModel string) map[string]any {
	out := NormalizeResponse(raw, fallbackModel)
	if c == nil {
		return out
	}
	return c.rewriteValue(out).(map[string]any)
}

func (c *ResponsesCompatibility) rewriteValue(value any) any {
	switch typed := value.(type) {
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = c.rewriteValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = c.rewriteValue(item)
		}
		if String(out, "type", "") == "function_call" {
			c.rewriteCall(out)
		}
		return out
	default:
		return value
	}
}

func (c *ResponsesCompatibility) rewriteCall(call map[string]any) (toolIdentity, bool) {
	identity, ok := c.aliases[String(call, "name", "")]
	if !ok {
		return toolIdentity{}, false
	}
	switch identity.Kind {
	case functionTool:
		call["name"] = identity.Name
		if identity.Namespace != "" {
			call["namespace"] = identity.Namespace
		} else {
			delete(call, "namespace")
		}
	case customTool:
		call["type"] = "custom_tool_call"
		call["name"] = identity.Name
		if identity.Namespace != "" {
			call["namespace"] = identity.Namespace
		} else {
			delete(call, "namespace")
		}
		call["input"] = decodeCustomInput(call["arguments"])
		delete(call, "arguments")
	case toolSearchTool:
		call["type"] = "tool_search_call"
		call["execution"] = identity.Execution
		call["arguments"] = decodeArguments(call["arguments"])
		delete(call, "name")
		delete(call, "namespace")
	}
	return identity, true
}

// TranslateStream converts one upstream Responses SSE event into zero or
// more client-compatible events. Custom/tool-search argument fragments are
// buffered until they can be decoded safely.
func (c *ResponsesCompatibility) TranslateStream(event string, data []byte) ([]StreamEvent, error) {
	if c == nil {
		return []StreamEvent{{Event: event, Data: data}}, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return []StreamEvent{{Event: event, Data: data}}, nil
	}
	kind := String(payload, "type", event)
	switch kind {
	case "response.output_item.added":
		if item, ok := payload["item"].(map[string]any); ok && String(item, "type", "") == "function_call" {
			alias := String(item, "name", "")
			if identity, found := c.aliases[alias]; found {
				state := &streamToolCall{identity: identity}
				for _, key := range []string{String(item, "id", ""), String(item, "call_id", "")} {
					if key != "" {
						c.streamCalls[key] = state
					}
				}
				c.rewriteCall(item)
			}
		}
	case "response.function_call_arguments.delta":
		identity, state, found := c.streamIdentity(payload)
		if found && (identity.Kind == customTool || identity.Kind == toolSearchTool) {
			state.arguments.WriteString(String(payload, "delta", ""))
			return nil, nil
		}
		c.rewriteEventName(payload)
	case "response.function_call_arguments.done":
		identity, state, found := c.streamIdentity(payload)
		if found && (identity.Kind == customTool || identity.Kind == toolSearchTool) {
			arguments := String(payload, "arguments", state.arguments.String())
			if arguments == "" {
				arguments = state.arguments.String()
			}
			if identity.Kind == toolSearchTool {
				return nil, nil
			}
			input := decodeCustomInput(arguments)
			delta := clone(payload)
			delta["type"] = "response.custom_tool_call_input.delta"
			delta["delta"] = input
			delete(delta, "arguments")
			done := clone(payload)
			done["type"] = "response.custom_tool_call_input.done"
			done["input"] = input
			delete(done, "arguments")
			return marshalStreamEvents(delta, done)
		}
		c.rewriteEventName(payload)
	case "response.output_item.done", "response.completed":
		payload = c.rewriteValue(payload).(map[string]any)
	default:
		payload = c.rewriteValue(payload).(map[string]any)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []StreamEvent{{Event: String(payload, "type", event), Data: encoded}}, nil
}

func (c *ResponsesCompatibility) streamIdentity(payload map[string]any) (toolIdentity, *streamToolCall, bool) {
	for _, key := range []string{String(payload, "item_id", ""), String(payload, "call_id", "")} {
		if state, ok := c.streamCalls[key]; ok {
			return state.identity, state, true
		}
	}
	if alias := String(payload, "name", ""); alias != "" {
		if identity, ok := c.aliases[alias]; ok {
			state := &streamToolCall{identity: identity}
			for _, key := range []string{String(payload, "item_id", ""), String(payload, "call_id", "")} {
				if key != "" {
					c.streamCalls[key] = state
				}
			}
			return identity, state, true
		}
	}
	return toolIdentity{}, nil, false
}

func (c *ResponsesCompatibility) rewriteEventName(payload map[string]any) {
	alias := String(payload, "name", "")
	identity, ok := c.aliases[alias]
	if !ok || identity.Kind != functionTool {
		return
	}
	payload["name"] = identity.Name
	if identity.Namespace != "" {
		payload["namespace"] = identity.Namespace
	}
}

func marshalStreamEvents(payloads ...map[string]any) ([]StreamEvent, error) {
	events := make([]StreamEvent, 0, len(payloads))
	for _, payload := range payloads {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		events = append(events, StreamEvent{Event: String(payload, "type", ""), Data: encoded})
	}
	return events, nil
}
