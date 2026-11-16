package noaadapter

import (
	"sort"

	"github.com/Autumn-27/norma/noa"
)

// The upstream valve excludes results below 1000 tokens. Many such results can
// still overflow the final request. Trim only outgoing copies, preserving raw
// history, call arguments, signed blocks and Compress archive/navigation panels.
func boundToolCopies(view []noa.CoreMessage, target int, size func([]noa.CoreMessage) int) ([]noa.CoreMessage, int) {
	var candidates []int
	for i, m := range view {
		if m.ContentType == noa.CTToolResult && m.ToolName != noa.CompressToolName && len([]rune(m.Text)) > 512 {
			candidates = append(candidates, i)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return len(view[candidates[i]].Text) > len(view[candidates[j]].Text) })
	out := append([]noa.CoreMessage(nil), view...)
	trimmed := 0
	for _, i := range candidates {
		if size(out) <= target {
			break
		}
		r := []rune(out[i].Text)
		out[i].Text = string(r[:128]) + "\n[NOA request-view excerpt; original tool result retained in session history; use the original call/source to recover evidence.]\n" + string(r[len(r)-128:])
		trimmed++
	}
	return out, trimmed
}
