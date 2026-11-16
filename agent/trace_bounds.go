package agent

import (
	"encoding/json"
	"os"
	"unicode/utf8"
)

func boundedContextEnabled() bool { return os.Getenv("ARTEX_BOUNDED_CONTEXT") == "1" }

// sliceUTF8 uses byte offsets, returning the actual aligned interval so callers
// can reconstruct evidence without gaps. No stored content is changed.
func sliceUTF8(text string, offset, limit int) (string, int, int) {
	start := max(0, min(offset, len(text)))
	for start > 0 && start < len(text) && !utf8.RuneStart(text[start]) {
		start--
	}
	end := min(len(text), start+max(0, limit))
	for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[start:end], start, end
}

func traceSlice(text string, offset, limit int) map[string]any {
	limit = max(4, min(limit, 4096))
	part, start, end := sliceUTF8(text, offset, limit)
	// Bound serialized bytes too: JSON control-character escaping can expand 6x.
	for b, _ := json.Marshal(part); len(b) > 4096; b, _ = json.Marshal(part) {
		part, start, end = sliceUTF8(text, offset, max(4, len(part)/2))
	}
	return map[string]any{"detail": part, "offset": start, "next_offset": end, "total_bytes": len(text), "returned_bytes": len(part), "has_more": end < len(text), "truncated": start > 0 || end < len(text)}
}

func traceInputSchema(schema map[string]any) map[string]any {
	if !boundedContextEnabled() {
		props := schema["properties"].(map[string]any)
		for _, key := range []string{"offset", "max_bytes", "before_step_id"} {
			delete(props, key)
		}
	}
	return schema
}
func tracePagingDescription() string {
	if !boundedContextEnabled() {
		return ""
	}
	return "Detail is a bounded excerpt: use offset=next_offset; summaries page with before_step_id=next_before_step_id. Stored evidence remains complete. "
}

// Bound the aggregate encoded response, retaining explicit continuation per step.
func boundTraceResult(result map[string]any, steps []map[string]any) {
	for {
		total, returned := 0, 0
		for _, step := range steps {
			if n, ok := step["total_bytes"].(int); ok {
				total += n
			}
			if text, ok := step["detail"].(string); ok {
				returned += len(text)
			}
		}
		result["total_detail_bytes"], result["returned_detail_bytes"] = total, returned
		encoded, _ := json.Marshal(result)
		if len(encoded) <= 16*1024 {
			return
		}
		changed := false
		for _, step := range steps {
			text, _ := step["detail"].(string)
			if len(text) <= 4 {
				continue
			}
			part, _, end := sliceUTF8(text, 0, max(4, len(text)/2))
			step["detail"] = part
			step["returned_bytes"] = end
			step["next_offset"] = step["offset"].(int) + end
			step["has_more"] = true
			step["truncated"] = true
			changed = true
		}
		if !changed {
			return
		}
	}
}
