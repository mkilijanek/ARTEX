package agent

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/mkilijanek/artex/db"
)

// Tool RAG: a lexical ranking of an agent's already-bound tool list, ported from
// upstream f3a20ef/e640c1b. In this fork it is observation only (see
// docs/phase-d3-plan.md): the ranking is computed next to the real list and never
// replaces it. projectTools is kept as upstream wrote it so the port stays
// comparable; observeToolRAG is the only entry point the host uses.

const (
	toolRAGTopKEnv    = "ARTEX_TOOL_RAG_TOPK" // non-core tools kept; default toolRAGDefaultK
	toolRAGDefaultK   = 12
	toolRAGMinCatalog = 20 // smaller catalogs are passed through whole

	toolRAGScorerName = "bm25-v1"

	// Caps on scoring input (plan 3.2.3): hostile or huge text must stay cheap.
	toolRAGMaxQueryBytes = 4096
	toolRAGMaxToolBytes  = 8192
	toolRAGMaxCandidates = 512
	// Caps on what is stored (plan 3.3).
	toolRAGMaxNames     = 256
	toolRAGMaxNameBytes = 128
	toolRAGUnknownName  = "_unknown"
	toolRAGOverflowName = "_overflow"
)

// Outcomes record what upstream's projection would have done (plan 3.2.2).
const (
	toolRAGProjected     = "projected"
	toolRAGSmallCatalog  = "small_catalog"
	toolRAGNoQuery       = "no_query"
	toolRAGNoSignal      = "no_signal"
	toolRAGScorerError   = "scorer_error"
	toolRAGObserverPanic = "observer_panic"
)

// Classes of a tool name in an observation (plan 3.3).
const (
	toolClassCandidate      = "candidate"
	toolClassKeep           = "keep"
	toolClassDeferredGlobal = "deferred_global"
	toolClassDeferredLocked = "deferred_locked"
	toolClassOther          = "other"
)

// toolRAGCore is always kept when bound and never counts against k.
var toolRAGCore = map[string]bool{
	"record_fact": true, "insert_assets": true, "report_finding": true, "list_findings": true,
	"graph_overview": true, "node_detail": true, "add_intent": true, "steer_work": true,
	"ask_operator": true, "prove_goal": true, "goal_met": true, "list_assets": true,
}

type toolQueryKey struct{}

// WithToolQuery attaches the retrieval query (goal + current intent/round summary)
// read by AugmentTools. No query = no ranking.
func WithToolQuery(ctx context.Context, q string) context.Context {
	return context.WithValue(ctx, toolQueryKey{}, q)
}

// plannerToolQuery is the Tool RAG query for one planning round: the goal plus the
// short texts the round's triggers already carry (no store lookups).
func plannerToolQuery(goal string, triggers []TriggerEvent) string {
	parts := []string{goal}
	for _, ev := range triggers {
		parts = append(parts, ev.Summary, ev.NewGoal)
		parts = append(parts, ev.Goals...)
		parts = append(parts, ev.Hints...)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// ToolScorer returns one relevance score per tool (same order as tools); higher is
// more relevant. It must be pure: no network, no database.
type ToolScorer func(query string, tools []actool.CoreTool) ([]float64, error)

// projectTools keeps the tools in keep (core / harness / deferred names) plus the
// k best-scoring other tools, preserving the input order. It falls back to the full
// list (projected=false) on an empty query, a catalog under toolRAGMinCatalog, or a
// scorer error. Observation mode does not call it; it documents what a later filter
// mode would do and is pinned by the ported tests.
func projectTools(query string, tools []actool.CoreTool, keep map[string]bool, k int, score ToolScorer) (out []actool.CoreTool, projected bool, err error) {
	if strings.TrimSpace(query) == "" || len(tools) < toolRAGMinCatalog {
		return tools, false, nil
	}
	var cands []actool.CoreTool
	for _, t := range tools {
		if !keep[t.Name()] {
			cands = append(cands, t)
		}
	}
	ranked, err := rankTools(query, cands, score)
	if err != nil {
		return tools, false, err
	}
	sel := map[string]bool{}
	for i := 0; i < len(ranked) && i < k; i++ {
		sel[ranked[i].Name()] = true
	}
	for _, t := range tools {
		if keep[t.Name()] || sel[t.Name()] {
			out = append(out, t)
		}
	}
	return out, true, nil
}

// rankTools orders tools by score descending, ties broken by name ascending.
func rankTools(query string, tools []actool.CoreTool, score ToolScorer) ([]actool.CoreTool, error) {
	idx, _, err := rankIndex(query, tools, score)
	if err != nil {
		return nil, err
	}
	out := make([]actool.CoreTool, len(idx))
	for i, j := range idx {
		out[i] = tools[j]
	}
	return out, nil
}

// rankIndex is rankTools returning positions and the scorer's scores, so a caller
// can map the order back to its own records. tools itself is never reordered.
func rankIndex(query string, tools []actool.CoreTool, score ToolScorer) (idx []int, scores []float64, err error) {
	scores, err = score(query, tools)
	if err == nil && len(scores) != len(tools) {
		err = fmt.Errorf("scorer returned %d scores for %d tools", len(scores), len(tools))
	}
	if err != nil {
		return nil, nil, err
	}
	idx = make([]int, len(tools))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if sa, sb := scores[idx[a]], scores[idx[b]]; sa != sb {
			return sa > sb
		}
		return tools[idx[a]].Name() < tools[idx[b]].Name()
	})
	return idx, scores, nil
}

// toolScoringText is what BM25 indexes for a tool: name + description + parameter
// names (sorted, so the text and any cut of it are deterministic).
func toolScoringText(t actool.CoreTool) string {
	text := t.Name() + " " + t.Description()
	if props, ok := t.InputSchema()["properties"].(map[string]any); ok {
		names := make([]string, 0, len(props))
		for p := range props {
			names = append(names, p)
		}
		sort.Strings(names)
		for _, p := range names {
			text += " " + p
		}
	}
	return text
}

// bm25Score is the default ToolScorer: Okapi BM25 (k1=1.2, b=0.75) over each tool's
// name + description + parameter names, IDF computed across the candidate set.
func bm25Score(query string, tools []actool.CoreTool) ([]float64, error) {
	const k1, b = 1.2, 0.75
	docs := make([]map[string]int, len(tools))
	lens := make([]float64, len(tools))
	df := map[string]int{}
	var total float64
	for i, t := range tools {
		tf := map[string]int{}
		for _, w := range tokenize(toolScoringText(t)) {
			tf[w]++
			lens[i]++
		}
		for w := range tf {
			df[w]++
		}
		docs[i], total = tf, total+lens[i]
	}
	scores := make([]float64, len(tools))
	n := float64(len(tools))
	avg, seen := math.Max(total/math.Max(n, 1), 1), map[string]bool{}
	for _, q := range tokenize(query) {
		if seen[q] || df[q] == 0 {
			continue
		}
		seen[q] = true
		idf := math.Log(1 + (n-float64(df[q])+0.5)/(float64(df[q])+0.5))
		for i, tf := range docs {
			if f := float64(tf[q]); f > 0 {
				scores[i] += idf * f * (k1 + 1) / (f + k1*(1-b+b*lens[i]/avg))
			}
		}
	}
	return scores, nil
}

// tokenize lowercases and splits on non-alphanumerics ("sql_injection" → sql, injection).
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// toolRAGKeep: names never hidden by the projection — the core set, the harness
// tools every agent relies on (file/shell tools, interactive shell, Skill
// meta-tool), and deferred tools (schemas already withheld; dropping them would
// desync the deferred block and the skill unlock gate).
func toolRAGKeep(deferred []string) map[string]bool {
	keep := map[string]bool{"Skill": true}
	for n := range toolRAGCore {
		keep[n] = true
	}
	for _, t := range append(actool.DefaultTools(), actool.ShellSessionTools()...) {
		keep[t.Name()] = true
	}
	for _, n := range deferred {
		keep[n] = true
	}
	return keep
}

// toolRAGTopK reads ARTEX_TOOL_RAG_TOPK; anything that is not a positive integer
// means the default.
func toolRAGTopK() int {
	k, err := strconv.Atoi(strings.TrimSpace(os.Getenv(toolRAGTopKEnv)))
	if err != nil || k <= 0 {
		return toolRAGDefaultK
	}
	return k
}

// truncUTF8 cuts s to at most n bytes on a rune boundary.
func truncUTF8(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// toolObsName applies the stored-name rule (plan 3.3): non-empty, at most 128 bytes,
// valid UTF-8, no control characters, else "_unknown".
func toolObsName(name string) string {
	if name == "" || len(name) > toolRAGMaxNameBytes || !utf8.ValidString(name) {
		return toolRAGUnknownName
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return toolRAGUnknownName
		}
	}
	return name
}

// toolObservation is what observeToolRAG records about one assembly. Everything in
// it is a name or a count: no query text, no description, no hash (plan 3.8).
type toolObservation struct {
	AgentKey string
	Scorer   string
	Mode     string
	Outcome  string
	K        int

	HostListCount  int // tools handed in, minus deferred names
	KeepCount      int // non-deferred tools in the keep set
	CandidateCount int // non-keep tools in the list (before the scoring cap)
	NonzeroCount   int
	WouldHideCount int // max(0, CandidateCount-K) when Outcome is projected, else 0

	DeferredCount int
	LockedAtStart int

	QueryTokens    int
	QueryTruncated bool
	DurationUS     int64

	Ranking []string // candidate names in rank order, name rule, at most 256

	// FailType is the Go type of the scorer error or of the recovered panic value
	// (never its text), for the caller to log (plan 3.2.4).
	FailType string

	// Classes maps every handed-in tool name to its class, for the call counter.
	Classes map[string]string
	// lockedNames are the deferred names not unlocked at assembly (plan 3.4).
	lockedNames []string
	unlock      *actool.UnlockSet

	// The call counter (toolrag_observe.go): calls per stored name until sealed.
	callMu sync.Mutex
	called map[string]db.ToolRAGCall
	sealed bool
}

// UnlockedDuringRun counts the names that were locked at assembly and are unlocked
// now. It includes unlocks seeded from a resumed session's history (plan 3.4).
func (o *toolObservation) UnlockedDuringRun() int {
	if o == nil || o.unlock == nil {
		return 0
	}
	n := 0
	for _, name := range o.lockedNames {
		if o.unlock.Has(name) {
			n++
		}
	}
	return n
}

// toolRAGNow is the clock for DurationUS (test seam).
var toolRAGNow = time.Now

// observeToolRAG ranks the planner's or worker's already-resolved tool list and
// returns what upstream's projection would have done. It has no path back to the
// list: it copies name and scoring text of every tool into its own slice first and
// ranks only that copy, and never sorts, re-slices, appends to or writes tools. It
// returns nil for other agent keys. A nil score means bm25Score. A scorer error or
// panic is recorded in the outcome; it never propagates.
func observeToolRAG(ctx context.Context, agentKey string, tools []actool.CoreTool, def DeferredInfo, score ToolScorer) (obs *toolObservation) {
	if agentKey != "planner" && agentKey != "worker" {
		return nil
	}
	if score == nil {
		score = bm25Score
	}
	start := toolRAGNow()
	query, _ := ctx.Value(toolQueryKey{}).(string)
	query, truncated := truncUTF8(query, toolRAGMaxQueryBytes)
	k := toolRAGTopK()

	deferred := map[string]bool{}
	for _, n := range def.Deferred {
		deferred[n] = true
	}
	keep := toolRAGKeep(def.Deferred)
	obs = &toolObservation{
		AgentKey: agentKey, Scorer: toolRAGScorerName, Mode: "observe", K: k,
		DeferredCount: len(deferred), QueryTruncated: truncated,
		Ranking: []string{}, Classes: make(map[string]string, len(tools)), unlock: def.Unlock,
	}
	for n := range deferred {
		if def.Unlock == nil || !def.Unlock.Has(n) {
			obs.LockedAtStart++
			obs.lockedNames = append(obs.lockedNames, n)
		}
	}

	// Own copy of what scoring needs; tools is not touched after this loop.
	var names []string
	var cands []actool.CoreTool
	for _, t := range tools {
		name := t.Name()
		switch {
		case deferred[name]:
			obs.Classes[name] = toolClassDeferredGlobal
			if def.Unlock == nil || !def.Unlock.Has(name) {
				obs.Classes[name] = toolClassDeferredLocked
			}
			continue
		case keep[name]:
			obs.Classes[name] = toolClassKeep
			obs.KeepCount++
		default:
			obs.Classes[name] = toolClassCandidate
			obs.CandidateCount++
			if len(cands) < toolRAGMaxCandidates {
				names = append(names, name)
				cands = append(cands, cappedToolCopy(t))
			}
		}
		obs.HostListCount++
	}

	defer func() {
		if r := recover(); r != nil {
			obs.Outcome, obs.FailType = toolRAGObserverPanic, fmt.Sprintf("%T", r)
			obs.Ranking, obs.NonzeroCount, obs.WouldHideCount = []string{}, 0, 0
		}
		obs.DurationUS = toolRAGNow().Sub(start).Microseconds()
	}()

	if strings.TrimSpace(query) == "" {
		obs.Outcome = toolRAGNoQuery
		return obs
	}
	obs.QueryTokens = len(tokenize(query))
	idx, scores, err := rankIndex(query, cands, score)
	if err != nil {
		obs.Outcome, obs.FailType = toolRAGScorerError, fmt.Sprintf("%T", err)
		return obs
	}
	for _, s := range scores {
		if s > 0 {
			obs.NonzeroCount++
		}
	}
	for i, j := range idx {
		if i >= toolRAGMaxNames {
			break
		}
		obs.Ranking = append(obs.Ranking, toolObsName(names[j]))
	}
	switch {
	case len(tools) < toolRAGMinCatalog:
		obs.Outcome = toolRAGSmallCatalog
	case len(cands) > 0 && obs.NonzeroCount == 0:
		obs.Outcome = toolRAGNoSignal
	default:
		obs.Outcome = toolRAGProjected
		obs.WouldHideCount = max(0, obs.CandidateCount-k)
	}
	return obs
}

// cappedToolCopy is a detached stand-in for t holding the scoring text cut to
// toolRAGMaxToolBytes: the name stays as is (it is the tie-break key), description
// and parameter names are folded into the description.
func cappedToolCopy(t actool.CoreTool) actool.CoreTool {
	name := t.Name()
	text := toolScoringText(t)
	rest := strings.TrimLeft(strings.TrimPrefix(text, name), " ")
	room := toolRAGMaxToolBytes - len(name) - 1
	if room < 0 {
		room = 0
	}
	rest, _ = truncUTF8(rest, room)
	return actool.Build(actool.Spec{Name: name, Description: rest, Schema: map[string]any{"type": "object"}})
}
