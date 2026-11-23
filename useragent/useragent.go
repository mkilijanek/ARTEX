// Package useragent resolves the User-Agent ARTEX puts on its own outbound requests. It is a
// leaf package (it imports nothing from the repo) so the request-building packages can use it
// without import cycles. All state is atomic: the settings API updates it at runtime and each
// request resolves the value when it is built.
package useragent

import (
	"slices"
	"sync/atomic"
)

// Configurable lists the components whose User-Agent the operator can switch on. WebFetch and
// WebSearch are sent by the Norma library with its own User-Agent and cannot be configured yet.
func Configurable() []string {
	return []string{"LLM", "Enrichment", "MCP", "Notify", "Tools", "API Recon"}
}

// Locked lists the components that are shown in the UI but never active.
func Locked() []string { return []string{"WebFetch", "WebSearch"} }

type state struct {
	hidden     bool
	custom     string
	components []string
}

var (
	version atomic.Value // string
	cur     atomic.Pointer[state]
)

func init() {
	version.Store("dev")
	cur.Store(&state{hidden: true, components: Configurable()})
}

// SetVersion sets the build version shown in the User-Agent.
func SetVersion(v string) { version.Store(v) }

func update(f func(s *state)) {
	for {
		old := cur.Load()
		n := &state{hidden: old.hidden, custom: old.custom, components: old.components}
		f(n)
		if cur.CompareAndSwap(old, n) {
			return
		}
	}
}

// SetHidden sets the master switch. While hidden, For returns "" for every component.
func SetHidden(h bool) { update(func(s *state) { s.hidden = h }) }

// SetCustom sets the operator's own User-Agent (already validated by the caller); "" = unset.
func SetCustom(c string) { update(func(s *state) { s.custom = c }) }

// SetComponents sets the checked components. Names that are not configurable (unknown ones and
// the locked Norma components) are dropped.
func SetComponents(names []string) {
	keep := make([]string, 0, len(names))
	for _, n := range Configurable() {
		if slices.Contains(names, n) {
			keep = append(keep, n)
		}
	}
	update(func(s *state) { s.components = keep })
}

// Hidden reports the master switch.
func Hidden() bool { return cur.Load().hidden }

// Custom returns the operator's custom User-Agent, "" when unset.
func Custom() string { return cur.Load().custom }

// Components returns the checked components in their fixed order.
func Components() []string { return slices.Clone(cur.Load().components) }

// For returns the User-Agent for a component, or "" meaning "do not set the header". It is ""
// while the master switch is on and for a component that is not checked. Otherwise it is the
// custom value, or the ARTEX string with the version and the component.
func For(component string) string {
	s := cur.Load()
	if s.hidden || !slices.Contains(s.components, component) {
		return ""
	}
	if s.custom != "" {
		return s.custom
	}
	return "Mozilla/5.0 (X11; Linux x86_64; rv:146.0) Gecko/20100101 ARTEX/" + version.Load().(string) + " (+" + component + ")"
}

// Version returns the build version shown in the User-Agent.
func Version() string { return version.Load().(string) }

// Fixed values the Norma library sends for WebFetch and WebSearch. They are not ours to change;
// they are here only so the settings page can show them. A test reads the Norma sources and fails
// when these drift (third_party/norma/tool/webfetch.go:236, websearch.go:226).
const (
	WebFetchUA  = "norma/0.4"
	WebSearchUA = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"
)

// What goes out when For returns "": the Go HTTP client's own value, or, for API Recon, the
// strings the skill scripts carry (skills/api-recon/scripts/harvest_static.py and spider_mpa.py).
const (
	goDefault        = "Go default (Go-http-client/1.1)"
	apiReconDefaults = "script default (Mozilla/5.0 (spa-api-recon) / Mozilla/5.0 (spa-api-recon spider))"
)

// Effective returns, for every component the settings page lists, the User-Agent it sends right
// now. It uses For, so the page and the wire cannot disagree. A User-Agent configured on a
// provider profile, MCP server, notify channel or custom tool overrides the value for that one.
func Effective() map[string]string {
	out := make(map[string]string, 8)
	for _, c := range Configurable() {
		if ua := For(c); ua != "" {
			out[c] = ua
		} else if c == "API Recon" {
			out[c] = apiReconDefaults
		} else {
			out[c] = goDefault
		}
	}
	out["WebFetch"] = WebFetchUA
	out["WebSearch"] = WebSearchUA
	return out
}
