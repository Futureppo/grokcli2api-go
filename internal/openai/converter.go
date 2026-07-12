package openai

import (
	"fmt"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/grok"
)

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
	input, ok := body["input"]
	if !ok {
		input, ok = body["messages"]
	}
	if !ok || input == nil {
		return fmt.Errorf("input is required and must be a string or array")
	}
	switch input.(type) {
	case string, []any:
		return nil
	default:
		return fmt.Errorf("input is required and must be a string or array")
	}
}

func validateModel(body map[string]any) error {
	model, ok := body["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return fmt.Errorf("model is required and must be a string")
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

func PrepareChat(body map[string]any) map[string]any {
	out := clone(body)
	out["stream"] = IsStreaming(body)
	if rejectsCompatibilityParameters(body) {
		// These models reject the OpenAI sampling penalty, while older
		// upstream models may still accept it. Handle both spellings so
		// OpenAI-compatible and direct clients behave consistently.
		delete(out, "presence_penalty")
		delete(out, "presencePenalty")
	}
	return out
}

// PrepareResponses accepts the current OpenAI Responses request shape while
// retaining compatibility with the old chat-shaped implementation.
func PrepareResponses(body map[string]any) map[string]any {
	out := clone(body)
	out["model"] = UpstreamModel(String(body, "model", ""))
	out["stream"] = IsStreaming(body)
	if rejectsCompatibilityParameters(body) {
		// Codex clients may send this extension, but these models do not
		// expose the corresponding Responses API argument.
		delete(out, "external_web_access")
		delete(out, "externalWebAccess")
	}
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
		out["store"] = false
	}
	return out
}

func rejectsCompatibilityParameters(body map[string]any) bool {
	switch strings.ToLower(strings.TrimSpace(String(body, "model", ""))) {
	case "grok-4.5", "composer":
		return true
	default:
		return false
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
	if model, _ := out["model"].(string); model == "" {
		out["model"] = fallbackModel
	}
	if _, ok := out["status"]; !ok {
		out["status"] = "completed"
	}
	if _, ok := out["output"]; !ok {
		out["output"] = []any{}
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
	return map[string]any{"error": map[string]any{"message": message, "type": kind, "param": nil, "code": code}}
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
