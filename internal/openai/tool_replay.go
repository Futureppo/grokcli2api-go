package openai

import (
	"strings"
	"sync"
	"time"
)

// ToolReplayCache stores completed function/custom tool calls so multi-turn
// OpenAI Responses clients (notably Alma) can continue with only
// previous_response_id / item_reference + tool outputs.
//
// Only tool calls are cached — never encrypted reasoning — so a multi-account
// pool cannot re-inject another account's opaque ciphertext.
type ToolReplayCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]toolReplayEntry
}

type toolReplayEntry struct {
	items      []map[string]any
	store      bool
	storeKnown bool
	expiresAt  time.Time
}

type toolReplayRecord struct {
	items      []map[string]any
	store      bool
	storeKnown bool
}

const (
	defaultToolReplayTTL = time.Hour
	defaultToolReplayMax = 10240
)

// DefaultToolReplay is the process-wide cache used by the Responses path.
var DefaultToolReplay = NewToolReplayCache(defaultToolReplayTTL, defaultToolReplayMax)

func NewToolReplayCache(ttl time.Duration, maxEntries int) *ToolReplayCache {
	if ttl <= 0 {
		ttl = defaultToolReplayTTL
	}
	if maxEntries < 1 {
		maxEntries = defaultToolReplayMax
	}
	return &ToolReplayCache{
		ttl:     ttl,
		max:     maxEntries,
		entries: make(map[string]toolReplayEntry),
	}
}

func toolReplayKey(model, key string) string {
	return model + "\x00" + key
}

func (c *ToolReplayCache) Get(model, key string) []map[string]any {
	record, ok := c.getRecord(model, key)
	if !ok {
		return nil
	}
	return record.items
}

func (c *ToolReplayCache) getRecord(model, key string) (toolReplayRecord, bool) {
	if c == nil || model == "" || key == "" {
		return toolReplayRecord{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[toolReplayKey(model, key)]
	if !ok {
		return toolReplayRecord{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(c.entries, toolReplayKey(model, key))
		return toolReplayRecord{}, false
	}
	// Sliding TTL: each successful read refreshes the entry expiry.
	entry.expiresAt = time.Now().Add(c.ttl)
	c.entries[toolReplayKey(model, key)] = entry
	return toolReplayRecord{items: cloneToolReplayItems(entry.items), store: entry.store, storeKnown: entry.storeKnown}, true
}

func (c *ToolReplayCache) Put(model, key string, items []map[string]any) {
	c.put(model, key, items, false, false)
}

func (c *ToolReplayCache) put(model, key string, items []map[string]any, store, storeKnown bool) {
	if c == nil || model == "" || key == "" || len(items) == 0 {
		return
	}
	normalized := normalizeReplayItems(items)
	if len(normalized) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		c.evictExpiredLocked(time.Now())
	}
	if len(c.entries) >= c.max {
		for existing := range c.entries {
			delete(c.entries, existing)
			break
		}
	}
	c.entries[toolReplayKey(model, key)] = toolReplayEntry{
		items:      cloneToolReplayItems(normalized),
		store:      store,
		storeKnown: storeKnown,
		expiresAt:  time.Now().Add(c.ttl),
	}
}

func (c *ToolReplayCache) evictExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
}

func cloneToolReplayItems(items []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, clone(item))
	}
	return out
}

// normalizeReplayItems keeps only the minimal function/custom tool-call shape
// that Grok accepts when replaying prior turns.
func normalizeReplayItems(items []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch String(item, "type", "") {
		case "function_call":
			callID := strings.TrimSpace(String(item, "call_id", ""))
			name := strings.TrimSpace(String(item, "name", ""))
			args, ok := item["arguments"].(string)
			if callID == "" || name == "" || !ok {
				continue
			}
			normalized := map[string]any{
				"type":      "function_call",
				"call_id":   callID,
				"name":      name,
				"arguments": args,
			}
			if id := strings.TrimSpace(String(item, "id", "")); id != "" {
				normalized["id"] = id
			}
			if namespace := strings.TrimSpace(String(item, "namespace", "")); namespace != "" {
				normalized["namespace"] = namespace
			}
			out = append(out, normalized)
		case "custom_tool_call":
			callID := strings.TrimSpace(String(item, "call_id", ""))
			name := strings.TrimSpace(String(item, "name", ""))
			if callID == "" || name == "" || item["input"] == nil {
				continue
			}
			normalized := map[string]any{
				"type":    "custom_tool_call",
				"status":  "completed",
				"call_id": callID,
				"name":    name,
				"input":   item["input"],
			}
			if id := strings.TrimSpace(String(item, "id", "")); id != "" {
				normalized["id"] = id
			}
			if status := strings.TrimSpace(String(item, "status", "")); status != "" {
				normalized["status"] = status
			}
			if namespace := strings.TrimSpace(String(item, "namespace", "")); namespace != "" {
				normalized["namespace"] = namespace
			}
			out = append(out, normalized)
		}
	}
	return out
}

// RememberCompletedResponse indexes tool calls from a completed Responses
// object. model must be the client-requested model (not free-tier rewrite).
//
// Keys:
//   - prev-resp:{response.id}
//   - cache:{prompt_cache_key} when present
//   - item:{item.id} for each tool call
func RememberCompletedResponse(cache *ToolReplayCache, model string, response map[string]any, promptCacheKey string) {
	RememberCompletedResponseWithStore(cache, model, response, promptCacheKey, false)
}

// RememberCompletedResponseWithStore also records whether the upstream can
// restore the response. Only store:false responses are eligible for local
// minimal tool-call replay.
func RememberCompletedResponseWithStore(cache *ToolReplayCache, model string, response map[string]any, promptCacheKey string, store bool) {
	if cache == nil || response == nil || strings.TrimSpace(model) == "" {
		return
	}
	items := extractReplayToolCalls(response["output"])
	if len(items) == 0 {
		return
	}
	responseID := String(response, "id", "")
	if responseID != "" {
		cache.put(model, "prev-resp:"+responseID, items, store, true)
	}
	if promptCacheKey = strings.TrimSpace(promptCacheKey); promptCacheKey != "" {
		cache.put(model, "cache:"+promptCacheKey, items, store, true)
	}
	for _, item := range items {
		if id := String(item, "id", ""); id != "" {
			cache.put(model, "item:"+id, []map[string]any{item}, store, true)
		}
	}
}

// RememberStreamToolCall indexes one tool call from output_item.done.
// Session keys (prev-resp / cache) are committed on response.completed after
// patching empty output; item:{id} is safe to write immediately so
// item_reference works mid-stream.
func RememberStreamToolCall(cache *ToolReplayCache, model string, item map[string]any, responseID, promptCacheKey string) {
	RememberStreamToolCallWithStore(cache, model, item, responseID, promptCacheKey, false)
}

func RememberStreamToolCallWithStore(cache *ToolReplayCache, model string, item map[string]any, responseID, promptCacheKey string, store bool) {
	if cache == nil || item == nil || strings.TrimSpace(model) == "" {
		return
	}
	items := normalizeReplayItems([]map[string]any{item})
	if len(items) == 0 {
		return
	}
	if responseID = strings.TrimSpace(responseID); responseID != "" {
		cache.put(model, "prev-resp:"+responseID, mergeReplayItems(cache.Get(model, "prev-resp:"+responseID), items), store, true)
	}
	if promptCacheKey = strings.TrimSpace(promptCacheKey); promptCacheKey != "" {
		cache.put(model, "cache:"+promptCacheKey, mergeReplayItems(cache.Get(model, "cache:"+promptCacheKey), items), store, true)
	}
	for _, normalized := range items {
		if id := String(normalized, "id", ""); id != "" {
			cache.put(model, "item:"+id, []map[string]any{normalized}, store, true)
		}
	}
}

// mergeReplayItems appends newItems, de-duplicating by call_id (then id).
func mergeReplayItems(existing, newItems []map[string]any) []map[string]any {
	if len(existing) == 0 {
		return newItems
	}
	seen := make(map[string]struct{}, len(existing)+len(newItems))
	out := make([]map[string]any, 0, len(existing)+len(newItems))
	add := func(item map[string]any) {
		key := strings.TrimSpace(String(item, "call_id", ""))
		if key == "" {
			key = strings.TrimSpace(String(item, "id", ""))
		}
		if key != "" {
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
		}
		out = append(out, item)
	}
	for _, item := range existing {
		add(item)
	}
	for _, item := range newItems {
		add(item)
	}
	return out
}

func extractReplayToolCalls(raw any) []map[string]any {
	output, ok := raw.([]any)
	if !ok {
		return nil
	}
	items := make([]map[string]any, 0, len(output))
	for _, entry := range output {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		switch String(item, "type", "") {
		case "function_call", "custom_tool_call":
			items = append(items, clone(item))
		}
	}
	return items
}

// expandItemReferences replaces item_reference with cached tool calls.
// Unresolved references are left for normalize to drop.
func expandItemReferences(cache *ToolReplayCache, model string, input []any) []any {
	if cache == nil || len(input) == 0 {
		return input
	}
	var out []any
	for index, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || String(item, "type", "") != "item_reference" {
			if out != nil {
				out = append(out, raw)
			}
			continue
		}
		id := strings.TrimSpace(String(item, "id", ""))
		if id == "" {
			if out != nil {
				out = append(out, raw)
			}
			continue
		}
		cached := cache.Get(model, "item:"+id)
		if len(cached) == 0 {
			if out != nil {
				out = append(out, raw)
			}
			continue
		}
		if out == nil {
			out = make([]any, 0, len(input)+len(cached)-1)
			out = append(out, input[:index]...)
		}
		for _, c := range cached {
			out = append(out, c)
		}
	}
	if out == nil {
		return input
	}
	return out
}

// applyToolCallReplay re-inserts cached function/custom calls for matching
// tool outputs from previous_response_id / prompt_cache_key sessions.
func applyToolCallReplay(cache *ToolReplayCache, model string, body map[string]any, previousResponseID, promptCacheKey string) bool {
	if cache == nil {
		return false
	}
	input, ok := body["input"].([]any)
	if !ok || len(input) == 0 {
		return false
	}

	// Collect existing calls and outputs already present in the request input.
	existingCalls := make(map[string]struct{})
	existingOutputs := make(map[string]struct{})
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		callID := strings.TrimSpace(String(item, "call_id", ""))
		switch String(item, "type", "") {
		case "function_call", "custom_tool_call":
			if callID != "" {
				existingCalls[callID] = struct{}{}
			}
		case "function_call_output", "custom_tool_call_output":
			if callID != "" {
				existingOutputs[callID] = struct{}{}
			}
		}
	}
	if len(existingOutputs) == 0 {
		return false
	}

	// Prefer previous_response_id session, then prompt_cache_key session.
	var candidates []map[string]any
	if previousResponseID != "" {
		record, found := cache.getRecord(model, "prev-resp:"+previousResponseID)
		if !found || !record.storeKnown || record.store {
			return false
		}
		candidates = append(candidates, record.items...)
	} else if promptCacheKey != "" {
		candidates = append(candidates, cache.Get(model, "cache:"+promptCacheKey)...)
	}
	if len(candidates) == 0 {
		return false
	}

	// Filter: only function/custom calls whose call_id has a matching output
	// and is not already present in input.
	filtered := make([]map[string]any, 0)
	seen := make(map[string]struct{})
	for _, item := range candidates {
		kind := String(item, "type", "")
		if kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		callID := strings.TrimSpace(String(item, "call_id", ""))
		if callID == "" {
			continue
		}
		if _, ok := existingCalls[callID]; ok {
			continue
		}
		if _, ok := existingOutputs[callID]; !ok {
			continue
		}
		if _, ok := seen[callID]; ok {
			continue
		}
		seen[callID] = struct{}{}
		existingCalls[callID] = struct{}{}
		filtered = append(filtered, clone(item))
	}
	if len(filtered) == 0 {
		// Calls may already be included by the client. This is still a
		// complete stateless continuation when every output is matched.
		for callID := range existingOutputs {
			if _, ok := existingCalls[callID]; !ok {
				return false
			}
		}
		return true
	}
	for callID := range existingOutputs {
		if _, ok := existingCalls[callID]; !ok {
			return false
		}
	}

	// Insert immediately before the first matching tool output.
	insertAt := len(input)
	for index, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind := String(item, "type", "")
		if kind != "function_call_output" && kind != "custom_tool_call_output" {
			continue
		}
		callID := strings.TrimSpace(String(item, "call_id", ""))
		if callID == "" {
			insertAt = index
			break
		}
		if _, ok := seen[callID]; ok {
			insertAt = index
			break
		}
		// First output overall if none of the restored calls match yet.
		if insertAt == len(input) {
			insertAt = index
		}
	}

	missing := make([]any, 0, len(filtered))
	for _, item := range filtered {
		missing = append(missing, item)
	}
	rewritten := make([]any, 0, len(input)+len(missing))
	rewritten = append(rewritten, input[:insertAt]...)
	rewritten = append(rewritten, missing...)
	rewritten = append(rewritten, input[insertAt:]...)
	body["input"] = rewritten
	return true
}
