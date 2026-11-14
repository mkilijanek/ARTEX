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
