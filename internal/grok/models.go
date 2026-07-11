package grok

import "time"

func Model(id string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": time.Now().Unix(), "owned_by": "xai"}
}
