package inference

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"

	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

func cleanSearchParameters(raw any) (map[string]any, error) {
	if raw == nil {
		return nil, nil
	}
	input, ok := raw.(map[string]any)
	if !ok {
		return nil, requestError("search_parameters", "search_parameters must be an object")
	}
	out := map[string]any{}
	if value, exists := input["mode"]; exists {
		mode, ok := value.(string)
		mode = strings.ToLower(strings.TrimSpace(mode))
		if !ok || (mode != "off" && mode != "on" && mode != "auto") {
			return nil, requestError("search_parameters.mode", "search_parameters.mode must be off, on, or auto")
		}
		out["mode"] = mode
	}
	if sources, ok := input["sources"].([]any); ok {
		if clean := cleanSearchSources(sources); len(clean) > 0 {
			out["sources"] = clean
		}
	} else if input["sources"] != nil {
		return nil, requestError("search_parameters.sources", "search_parameters.sources must be an array")
	}
	// Convert the pre-0.2.102 flat source options when their meaning is
	// unambiguous. Unknown flat options simply disappear.
	if _, exists := out["sources"]; !exists {
		var sources []any
		x := copySourceFields("x", input, []string{"included_x_handles", "x_handles", "excluded_x_handles", "post_favorite_count", "post_view_count"})
		if len(x) > 1 {
			sources = append(sources, x)
		}
		web := copySourceFields("web", input, []string{"excluded_websites", "allowed_websites", "country", "safe_search"})
		if len(web) > 1 || boolValue(input["web"]) {
			sources = append(sources, web)
		}
		news := copySourceFields("news", input, []string{"excluded_websites", "country", "safe_search"})
		if boolValue(input["news"]) {
			sources = append(sources, news)
		}
		if boolValue(input["rss"]) {
			if links := cleanStrings(input["links"]); len(links) > 0 {
				sources = append(sources, map[string]any{"type": "rss", "links": links})
			}
		}
		if len(sources) > 0 {
			out["sources"] = sources
		}
	}
	for _, key := range []string{"from_date", "to_date"} {
		if value, ok := input[key].(string); ok && strings.TrimSpace(value) != "" {
			out[key] = value
		} else if input[key] != nil {
			return nil, requestError("search_parameters."+key, "search_parameters."+key+" must be a non-empty string")
		}
	}
	if value, exists := input["return_citations"]; exists {
		if _, ok := value.(bool); !ok {
			return nil, requestError("search_parameters.return_citations", "search_parameters.return_citations must be a boolean")
		}
		out["return_citations"] = value
	}
	if value, exists := input["max_search_results"]; exists {
		if err := positiveInteger(value, "search_parameters.max_search_results", false); err != nil {
			return nil, err
		}
		number, _ := jsonNumber(value)
		if number > math.MaxInt32 {
			return nil, requestError("search_parameters.max_search_results", "search_parameters.max_search_results must fit in a non-negative 32-bit integer")
		}
		out["max_search_results"] = value
	}
	return out, nil
}

func cleanSearchSources(sources []any) []any {
	var out []any
	for _, raw := range sources {
		source, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind := strings.ToLower(trimmedString(source["type"]))
		var fields []string
		switch kind {
		case "x":
			fields = []string{"included_x_handles", "x_handles", "excluded_x_handles", "post_favorite_count", "post_view_count"}
		case "web":
			fields = []string{"excluded_websites", "allowed_websites", "country", "safe_search"}
		case "news":
			fields = []string{"excluded_websites", "country", "safe_search"}
		case "rss":
			links := cleanStrings(source["links"])
			if len(links) > 0 {
				out = append(out, map[string]any{"type": "rss", "links": links})
			}
			continue
		default:
			continue
		}
		out = append(out, copySourceFields(kind, source, fields))
	}
	return out
}

func copySourceFields(kind string, source map[string]any, fields []string) map[string]any {
	out := map[string]any{"type": kind}
	for _, key := range fields {
		value := source[key]
		switch key {
		case "included_x_handles", "x_handles", "excluded_x_handles", "excluded_websites", "allowed_websites":
			if values := cleanStrings(value); len(values) > 0 {
				out[key] = values
			}
		case "post_favorite_count", "post_view_count":
			if number, ok := jsonNumber(value); ok && finite(number) && number >= 0 && number <= math.MaxInt32 && math.Trunc(number) == number {
				out[key] = value
			}
		case "safe_search":
			if _, ok := value.(bool); ok {
				out[key] = value
			}
		case "country":
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				out[key] = text
			}
		}
	}
	return out
}

func cleanStrings(raw any) []any {
	items, _ := raw.([]any)
	var out []any
	for _, rawItem := range items {
		if item, ok := rawItem.(string); ok && strings.TrimSpace(item) != "" {
			out = append(out, item)
		}
	}
	return out
}

func cleanNativeResponsesInput(raw any, allowStatefulOutputs bool) any {
	if text, ok := raw.(string); ok {
		return text
	}
	items, _ := raw.([]any)
	var out []any
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
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
			if role != "user" && role != "assistant" && role != "system" && role != "developer" {
				continue
			}
			content := cleanNativeResponsesContent(item["content"], role)
			if content == nil {
				continue
			}
			out = append(out, map[string]any{"type": "message", "role": role, "content": content})
		case "function_call", "custom_tool_call":
			callID, name := trimmedString(item["call_id"]), trimmedString(item["name"])
			if callID == "" || name == "" {
				continue
			}
			clean := map[string]any{"type": kind, "call_id": callID, "name": name}
			if namespace := trimmedString(item["namespace"]); namespace != "" {
				clean["namespace"] = namespace
			}
			if id := trimmedString(item["id"]); id != "" {
				clean["id"] = id
			}
			if kind == "function_call" {
				clean["arguments"] = stringifyFunctionArguments(item["arguments"])
			} else if item["input"] != nil {
				clean["input"] = cloneValue(item["input"])
			} else {
				continue
			}
			out = append(out, clean)
		case "tool_search_call":
			// Client-side tool search is represented to the upstream by the
			// newly loaded declarations plus the developer marker below. Keeping
			// the call without a matching ModelInput output leaves an unresolved
			// function turn and commonly causes another search loop.
			continue
		case "tool_search_output":
			if !clientToolExecution(item) {
				continue
			}
			out = append(out, map[string]any{
				"type": "message", "role": "developer",
				"content": []any{map[string]any{"type": "input_text", "text": toolSearchCompletedText}},
			})
		case "function_call_output", "custom_tool_call_output":
			callID := trimmedString(item["call_id"])
			if callID == "" || item["output"] == nil {
				continue
			}
			out = append(out, map[string]any{"type": kind, "call_id": callID, "output": flattenFunctionOutput(item["output"])})
		case "reasoning":
			clean := map[string]any{"type": "reasoning"}
			for _, key := range []string{"id", "encrypted_content", "status"} {
				if value, ok := item[key].(string); ok && value != "" {
					clean[key] = value
				}
			}
			if summary := cleanReasoningParts(item["summary"]); len(summary) > 0 {
				clean["summary"] = summary
			}
			if len(clean) > 1 {
				out = append(out, clean)
			}
		case "web_search_call", "x_search_call", "image_generation_call":
			clean := map[string]any{"type": kind}
			for _, key := range []string{"id", "call_id", "status", "name"} {
				if value, ok := item[key].(string); ok && value != "" {
					clean[key] = value
				}
			}
			for _, key := range []string{"action", "input", "result", "output"} {
				if value := item[key]; value != nil {
					clean[key] = cloneValue(value)
				}
			}
			out = append(out, clean)
		}
	}
	if allowStatefulOutputs || len(out) == 0 {
		return out
	}
	calls := make(map[string]struct{})
	for _, rawItem := range out {
		item, _ := rawItem.(map[string]any)
		switch trimmedString(item["type"]) {
		case "function_call", "custom_tool_call":
			if callID := trimmedString(item["call_id"]); callID != "" {
				calls[callID] = struct{}{}
			}
		}
	}
	filtered := make([]any, 0, len(out))
	for _, rawItem := range out {
		item, _ := rawItem.(map[string]any)
		switch trimmedString(item["type"]) {
		case "function_call_output", "custom_tool_call_output":
			if _, ok := calls[trimmedString(item["call_id"])]; !ok {
				continue
			}
		}
		filtered = append(filtered, rawItem)
	}
	return filtered
}

const toolSearchCompletedText = "Tool search completed; the selected tools are now available."

func stringifyFunctionArguments(value any) string {
	if text, ok := value.(string); ok {
		if strings.TrimSpace(text) == "" {
			return "{}"
		}
		return text
	}
	return jsonString(value)
}

func flattenFunctionOutput(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, rawPart := range value {
			if part, ok := rawPart.(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					parts = append(parts, text)
					continue
				}
			}
			parts = append(parts, jsonString(rawPart))
		}
		return strings.Join(parts, "\n")
	default:
		return jsonString(value)
	}
}

func rewriteNativeResponseInputAliases(raw any, aliases map[string]ToolAlias) any {
	input, ok := raw.([]any)
	if !ok || len(aliases) == 0 {
		return raw
	}
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		kind := trimmedString(item["type"])
		if kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		name, namespace := trimmedString(item["name"]), trimmedString(item["namespace"])
		wire := ""
		identity := ToolAlias{}
		for candidate, alias := range aliases {
			if alias.Name == name && alias.Namespace == namespace && (alias.Kind == "function" || alias.Kind == "custom") {
				wire, identity = candidate, alias
				break
			}
		}
		if wire == "" {
			if alias, exists := aliases[name]; exists {
				wire, identity = name, alias
			}
		}
		if wire == "" {
			continue
		}
		item["name"] = wire
		delete(item, "namespace")
		if identity.Kind == "custom" {
			inputValue := item["input"]
			if inputValue == nil {
				inputValue = item["arguments"]
			}
			item["type"] = "function_call"
			item["arguments"] = jsonString(map[string]any{"input": inputValue})
			delete(item, "input")
		}
	}
	return input
}

func cleanNativeResponsesContent(raw any, role string) any {
	if text, ok := raw.(string); ok {
		return text
	}
	parts, _ := raw.([]any)
	var out []any
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		kind := strings.ToLower(trimmedString(part["type"]))
		switch kind {
		case "input_text", "output_text":
			if text, ok := part["text"].(string); ok {
				out = append(out, map[string]any{"type": kind, "text": text})
			}
		case "input_image":
			if role != "user" {
				continue
			}
			if imageURL, ok := part["image_url"].(string); ok && strings.TrimSpace(imageURL) != "" {
				out = append(out, map[string]any{"type": "input_image", "image_url": imageURL})
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanReasoningParts(raw any) []any {
	parts, _ := raw.([]any)
	var out []any
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		kind := trimmedString(part["type"])
		if kind != "summary_text" && kind != "text" {
			continue
		}
		if text, ok := part["text"].(string); ok {
			out = append(out, map[string]any{"type": kind, "text": text})
		}
	}
	return out
}

func responseToolSources(body map[string]any) []any {
	tools, _ := body["tools"].([]any)
	out := append([]any(nil), tools...)
	input, _ := body["input"].([]any)
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if trimmedString(item["type"]) != "tool_search_output" || !clientToolExecution(item) {
			continue
		}
		loaded, _ := item["tools"].([]any)
		for _, rawTool := range loaded {
			if tool, ok := rawTool.(map[string]any); ok {
				tool = cloneMap(tool)
				delete(tool, "defer_loading")
				out = append(out, tool)
			}
		}
	}
	return out
}

func cleanNativeResponsesTools(tools []any, allowSearch bool) ([]any, map[string]ToolAlias) {
	aliases := map[string]ToolAlias{}
	positions := map[string]int{}
	var out []any
	clientSearch := false
	searchDescription := ""
	var searchParameters map[string]any
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		if trimmedString(tool["type"]) == "tool_search" && clientToolExecution(tool) {
			clientSearch = true
			if searchDescription == "" {
				searchDescription = stringValue(tool["description"])
			}
			if searchParameters == nil {
				if parameters, ok := tool["parameters"].(map[string]any); ok {
					searchParameters = cloneMap(parameters)
				}
			}
		}
	}
	alias := func(identity ToolAlias) string {
		return ensureToolAlias(aliases, identity)
	}
	add := func(tool map[string]any) {
		key := trimmedString(tool["type"]) + "\x00" + trimmedString(tool["name"])
		if index, exists := positions[key]; exists {
			out[index] = tool
			return
		}
		positions[key] = len(out)
		out = append(out, tool)
	}
	var deferred []string
	var visit func(map[string]any, string, bool)
	visit = func(tool map[string]any, namespace string, force bool) {
		kind := strings.ToLower(trimmedString(tool["type"]))
		switch kind {
		case "function":
			name := trimmedString(tool["name"])
			parameters, ok := tool["parameters"].(map[string]any)
			if !ok && tool["parameters"] == nil {
				parameters = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
				ok = true
			}
			if name == "" || !ok {
				return
			}
			if boolValue(tool["defer_loading"]) && clientSearch && !force {
				deferred = append(deferred, namespace+name)
				return
			}
			identity := ToolAlias{Kind: "function", Name: name, Namespace: namespace}
			clean := map[string]any{"type": "function", "name": alias(identity), "parameters": cloneMap(parameters)}
			if description, ok := tool["description"].(string); ok {
				clean["description"] = description
			}
			add(clean)
		case "custom":
			name := trimmedString(tool["name"])
			if name == "" {
				return
			}
			identity := ToolAlias{Kind: "custom", Name: name, Namespace: namespace}
			description := stringValue(tool["description"])
			if description != "" {
				description += "\n"
			}
			description += "Provide the custom tool input in the input string field."
			add(map[string]any{
				"type": "function", "name": alias(identity), "description": description,
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []any{"input"}, "additionalProperties": false},
			})
		case "namespace":
			name := trimmedString(tool["name"])
			children, ok := tool["tools"].([]any)
			if name == "" || !ok {
				return
			}
			deferred = append(deferred, name)
			for _, rawChild := range children {
				child, _ := rawChild.(map[string]any)
				visit(child, name, force)
			}
		case "tool_search":
			// The shim is added after deferred descriptions are collected.
		case "web_search", "web_search_preview", "x_search":
			if allowSearch {
				add(map[string]any{"type": kind})
			}
		case "image_generation":
			clean := map[string]any{"type": kind}
			for _, key := range []string{"background", "input_fidelity", "moderation", "output_compression", "output_format", "quality", "size"} {
				if value := tool[key]; value != nil {
					clean[key] = cloneValue(value)
				}
			}
			add(clean)
		}
	}
	for _, rawTool := range tools {
		if tool, ok := rawTool.(map[string]any); ok {
			visit(tool, "", false)
		}
	}
	if clientSearch {
		identity := ToolAlias{Kind: "tool_search", Name: "tool_search", Execution: "client"}
		description := searchDescription
		if strings.TrimSpace(description) == "" {
			description = "Search for tools needed to continue the task."
		}
		if len(deferred) > 0 {
			description += " Available deferred tools: " + strings.Join(deferred, ", ")
		}
		if searchParameters == nil {
			searchParameters = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
		}
		add(map[string]any{
			"type": "function", "name": alias(identity), "description": description,
			"parameters": searchParameters,
		})
	}
	if len(aliases) == 0 {
		aliases = nil
	}
	return out, aliases
}

func ensureResponseInputToolAliases(raw any, aliases map[string]ToolAlias) map[string]ToolAlias {
	input, _ := raw.([]any)
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		kind := strings.ToLower(trimmedString(item["type"]))
		if kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		name := trimmedString(item["name"])
		if name == "" {
			continue
		}
		if _, alreadyWireName := aliases[name]; alreadyWireName && trimmedString(item["namespace"]) == "" {
			continue
		}
		if aliases == nil {
			aliases = make(map[string]ToolAlias)
		}
		identityKind := "function"
		if kind == "custom_tool_call" {
			identityKind = "custom"
		}
		ensureToolAlias(aliases, ToolAlias{Kind: identityKind, Name: name, Namespace: trimmedString(item["namespace"])})
	}
	return aliases
}

func ensureToolAlias(aliases map[string]ToolAlias, identity ToolAlias) string {
	for wire, existing := range aliases {
		if existing == identity {
			return wire
		}
	}
	key := identity.Kind + "\x00" + identity.Namespace + "\x00" + identity.Name + "\x00" + identity.Execution
	base := identity.Namespace + identity.Name
	if identity.Kind == "tool_search" {
		base = "grokcli2api_tool_search"
	}
	if base == "" {
		base = "grokcli2api_tool"
	}
	wire := base
	if len(wire) > 128 {
		wire = wire[:117] + "__" + inferenceShortHash(key)
	}
	if existing, collision := aliases[wire]; collision && existing != identity {
		limit := 117
		if len(base) < limit {
			limit = len(base)
		}
		wire = base[:limit] + "__" + inferenceShortHash(key)
	}
	aliases[wire] = identity
	return wire
}

func clientToolExecution(item map[string]any) bool {
	execution := firstString(item, "execution")
	return execution == "" || strings.EqualFold(execution, "client")
}

func inferenceShortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:9]
}

func responsesToolChoice(body map[string]any, tools []any, aliases map[string]ToolAlias) any {
	choice := parseToolChoice(body)
	if choice.mode != "" {
		return choice.mode
	}
	declared := func(kind, name string) bool {
		for _, rawTool := range tools {
			tool, _ := rawTool.(map[string]any)
			if trimmedString(tool["type"]) == kind && trimmedString(tool["name"]) == name {
				return true
			}
		}
		return false
	}
	if choice.name != "" {
		for wire, alias := range aliases {
			kindMatches := choice.kind == "tool" || choice.kind == alias.Kind
			if kindMatches && alias.Name == choice.name && alias.Namespace == choice.namespace && declared("function", wire) {
				return map[string]any{"type": "function", "name": wire}
			}
		}
		// Cross-protocol function tools have no alias table. Also accept a
		// client that already supplied the flattened native wire name.
		if declared("function", choice.name) {
			return map[string]any{"type": "function", "name": choice.name}
		}
		return nil
	}

	// Grok's Responses wire accepts only the function-shaped object choice.
	// Hosted-tool choices therefore retain the strongest portable intent.
	raw, _ := body["tool_choice"].(map[string]any)
	hostedKind := strings.ToLower(trimmedString(raw["type"]))
	if hostedKind == "tool_search" {
		for wire, alias := range aliases {
			if alias.Kind == "tool_search" && declared("function", wire) {
				return map[string]any{"type": "function", "name": wire}
			}
		}
	}
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		if hostedKind != "" && trimmedString(tool["type"]) == hostedKind {
			return "required"
		}
	}
	return nil
}

func messagesToolChoice(body map[string]any, tools []any, aliases map[string]ToolAlias) any {
	choice := parseToolChoice(body)
	parallel, hasParallel := requestedParallelToolCalls(body)
	var out map[string]any
	switch {
	case choice.mode == "none":
		out = map[string]any{"type": "none"}
	case choice.mode == "required":
		out = map[string]any{"type": "any"}
	case choice.mode == "auto":
		out = map[string]any{"type": "auto"}
	case choice.name != "":
		name := toolChoiceWireName(choice, aliases)
		for _, rawTool := range tools {
			tool, _ := rawTool.(map[string]any)
			if trimmedString(tool["name"]) == name {
				out = map[string]any{"type": "tool", "name": name}
				break
			}
		}
	}
	if out == nil && hasParallel {
		out = map[string]any{"type": "auto"}
	}
	if out == nil {
		return nil
	}
	if out != nil && hasParallel {
		out["disable_parallel_tool_use"] = !parallel
	}
	return out
}

func (p *RequestPlan) renderNativeMessages(model string, descriptor modelcatalog.ModelDescriptor) (map[string]any, bool, error) {
	maxTokens := p.body["max_tokens"]
	if err := positiveInteger(maxTokens, "max_tokens", true); err != nil {
		return nil, false, err
	}
	messages, preserved := cleanNativeMessages(p.body["messages"])
	if len(messages) == 0 {
		return nil, false, noInput(p.protocol)
	}
	out := map[string]any{"model": model, "max_tokens": maxTokens, "messages": messages, "stream": p.stream}
	if system := cleanNativeSystem(p.body["system"]); system != nil {
		out["system"] = system
	}
	if err := copyOptionalNumber(out, p.body, "temperature", 0, 1); err != nil {
		return nil, false, err
	}
	if err := copyOptionalNumber(out, p.body, "top_p", 0, 1); err != nil {
		return nil, false, err
	}
	if value, exists := p.body["top_k"]; exists {
		if err := positiveInteger(value, "top_k", false); err != nil {
			return nil, false, err
		}
		out["top_k"] = value
	}
	if stops := cleanStrings(p.body["stop_sequences"]); len(stops) > 0 {
		out["stop_sequences"] = stops
	} else if p.body["stop_sequences"] != nil {
		if _, ok := p.body["stop_sequences"].([]any); !ok {
			return nil, false, requestError("stop_sequences", "stop_sequences must be an array of strings")
		}
	}
	if tools := cleanNativeMessageTools(p.body["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if choice := cleanMessageToolChoice(p.body["tool_choice"], tools); choice != nil {
			out["tool_choice"] = choice
		}
	}
	if metadata, ok := p.body["metadata"].(map[string]any); ok {
		if user := trimmedString(metadata["user_id"]); user != "" {
			out["metadata"] = map[string]any{"user_id": user}
		}
	}
	if thinking, err := cleanThinking(p.body["thinking"]); err != nil {
		return nil, false, err
	} else if thinking != nil {
		out["thinking"] = thinking
	}
	if config, ok := p.body["output_config"].(map[string]any); ok {
		if format := cleanMessagesFormat(config["format"]); format != nil {
			out["output_config"] = map[string]any{"format": format}
		}
	}
	_ = descriptor // retained for future backend capability gates
	return out, preserved, nil
}

func cleanNativeMessages(raw any) ([]any, bool) {
	messages, _ := raw.([]any)
	var out []any
	preserved := false
	knownCalls := make(map[string]struct{})
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(trimmedString(message["role"]))
		if role != "user" && role != "assistant" {
			continue
		}
		if text, ok := message["content"].(string); ok {
			out = append(out, map[string]any{"role": role, "content": text})
			continue
		}
		blocks, _ := message["content"].([]any)
		var clean []any
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(trimmedString(block["type"])) {
			case "text":
				if text, ok := block["text"].(string); ok {
					item := map[string]any{"type": "text", "text": text}
					copyCacheControl(item, block)
					clean = append(clean, item)
				}
			case "image":
				if role == "user" {
					if image, ok := canonicalImage(block); ok {
						source := map[string]any{"type": "url", "url": image.url}
						if image.data != "" && image.mediaType != "" {
							source = map[string]any{"type": "base64", "media_type": image.mediaType, "data": image.data}
						}
						clean = append(clean, map[string]any{"type": "image", "source": source})
					}
				}
			case "tool_use":
				id, name := trimmedString(block["id"]), trimmedString(block["name"])
				if role == "assistant" && id != "" && name != "" && block["input"] != nil {
					clean = append(clean, map[string]any{"type": "tool_use", "id": id, "name": name, "input": cloneValue(block["input"])})
					knownCalls[id] = struct{}{}
				}
			case "tool_result":
				id := trimmedString(block["tool_use_id"])
				_, known := knownCalls[id]
				if role == "user" && id != "" && known {
					if content := cleanToolResultContent(block["content"]); content != nil {
						item := map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}
						copyCacheControl(item, block)
						clean = append(clean, item)
					}
				}
			case "thinking":
				thinking, thinkingOK := block["thinking"].(string)
				signature, signatureOK := block["signature"].(string)
				if role == "assistant" && thinkingOK && signatureOK && signature != "" {
					clean = append(clean, map[string]any{"type": "thinking", "thinking": thinking, "signature": signature})
					preserved = true
				}
			}
		}
		if len(clean) > 0 {
			out = append(out, map[string]any{"role": role, "content": clean})
		}
	}
	return out, preserved
}

func cleanNativeSystem(raw any) any {
	if text, ok := raw.(string); ok {
		return text
	}
	blocks, _ := raw.([]any)
	var out []any
	for _, rawBlock := range blocks {
		block, _ := rawBlock.(map[string]any)
		if trimmedString(block["type"]) != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			item := map[string]any{"type": "text", "text": text}
			copyCacheControl(item, block)
			out = append(out, item)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanToolResultContent(raw any) any {
	if text, ok := raw.(string); ok {
		return text
	}
	blocks, _ := raw.([]any)
	var out []any
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		switch trimmedString(block["type"]) {
		case "text":
			if text, ok := block["text"].(string); ok {
				item := map[string]any{"type": "text", "text": text}
				copyCacheControl(item, block)
				out = append(out, item)
			}
		case "image":
			if image, ok := canonicalImage(block); ok {
				source := map[string]any{"type": "url", "url": image.url}
				if image.data != "" {
					source = map[string]any{"type": "base64", "media_type": image.mediaType, "data": image.data}
				}
				out = append(out, map[string]any{"type": "image", "source": source})
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func copyCacheControl(out, source map[string]any) {
	cache, _ := source["cache_control"].(map[string]any)
	if trimmedString(cache["type"]) == "ephemeral" {
		out["cache_control"] = map[string]any{"type": "ephemeral"}
	}
}

func cleanNativeMessageTools(raw any) []any {
	tools, _ := raw.([]any)
	var out []any
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok || trimmedString(tool["type"]) != "" {
			continue
		}
		name := trimmedString(tool["name"])
		schema, ok := tool["input_schema"].(map[string]any)
		if name == "" || !ok {
			continue
		}
		clean := map[string]any{"name": name, "input_schema": cloneMap(schema)}
		if description, ok := tool["description"].(string); ok {
			clean["description"] = description
		}
		out = append(out, clean)
	}
	return out
}

func cleanMessageToolChoice(raw any, tools []any) any {
	choice, _ := raw.(map[string]any)
	kind := trimmedString(choice["type"])
	var out map[string]any
	switch kind {
	case "auto", "any", "none":
		out = map[string]any{"type": kind}
	case "tool":
		name := trimmedString(choice["name"])
		for _, rawTool := range tools {
			tool, _ := rawTool.(map[string]any)
			if trimmedString(tool["name"]) == name {
				out = map[string]any{"type": "tool", "name": name}
				break
			}
		}
	}
	if out == nil {
		return nil
	}
	if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
		out["disable_parallel_tool_use"] = disabled
	}
	return out
}

func cleanThinking(raw any) (any, error) {
	if raw == nil {
		return nil, nil
	}
	thinking, ok := raw.(map[string]any)
	if !ok {
		return nil, requestError("thinking", "thinking must be an object")
	}
	kind := strings.ToLower(trimmedString(thinking["type"]))
	switch kind {
	case "disabled":
		return map[string]any{"type": "disabled"}, nil
	case "adaptive":
		out := map[string]any{"type": "adaptive"}
		if display, exists := thinking["display"]; exists {
			value, ok := display.(string)
			value = strings.ToLower(strings.TrimSpace(value))
			if !ok || (value != "omitted" && value != "summarized") {
				return nil, requestError("thinking.display", "thinking.display must be omitted or summarized")
			}
			out["display"] = value
		}
		return out, nil
	case "enabled":
		budget := thinking["budget_tokens"]
		if err := positiveInteger(budget, "thinking.budget_tokens", true); err != nil {
			return nil, err
		}
		return map[string]any{"type": "enabled", "budget_tokens": budget}, nil
	default:
		return nil, requestError("thinking.type", "thinking.type must be adaptive, enabled, or disabled")
	}
}

func cleanMessagesFormat(raw any) any {
	format, ok := raw.(map[string]any)
	if !ok || trimmedString(format["type"]) != "json_schema" || format["schema"] == nil {
		return nil
	}
	return map[string]any{"type": "json_schema", "schema": cloneValue(format["schema"])}
}
