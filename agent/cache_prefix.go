package agent

import (
	"os"
	"regexp"
	"strings"
)

var volatilePrompt = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}|[/\\](tasks|sessions)[/\\]|\[The intent you claimed`)

// cachePrefix splits only at an existing paragraph boundary. Joining the parts
// recreates the exact original prompt; instructions and their order are unchanged.
// A custom template without a safe boundary stays unsplit.
func cachePrefix(system []string, boundary int) ([]string, int) {
	if os.Getenv("ARTEX_CACHE_PREFIX") != "1" || len(system) == 0 {
		return system, boundary
	}
	text := system[0]
	loc := volatilePrompt.FindStringIndex(text)
	if loc == nil {
		return system, boundary
	}
	i := strings.LastIndex(text[:loc[0]], "\n\n")
	if i <= 0 {
		return system, boundary
	}
	out := []string{text[:i], text[i+2:]}
	out = append(out, system[1:]...)
	return out, 1
}
