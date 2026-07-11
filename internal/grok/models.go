package grok

import "time"

var ModelIDs = []string{
	"grok-build", "grok-4", "grok-4.5", "grok-auto",
	"grok-4-fast-reasoning", "grok-4-fast-non-reasoning",
	"grok-3", "grok-3-mini", "grok-code-fast-1", "grok-2-vision",
}

func Models() map[string]any {
	data := make([]map[string]any, 0, len(ModelIDs))
	for _, id := range ModelIDs {
		data = append(data, Model(id))
	}
	return map[string]any{"object": "list", "data": data}
}

func Model(id string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": time.Now().Unix(), "owned_by": "xai"}
}

func HasModel(id string) bool {
	for _, candidate := range ModelIDs {
		if candidate == id {
			return true
		}
	}
	return false
}
