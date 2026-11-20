package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	actool "github.com/Autumn-27/norma/tool"
)

func ragTool(name, desc string, params ...string) actool.CoreTool {
	props := map[string]any{}
	for _, p := range params {
		props[p] = map[string]any{"type": "string"}
	}
	return actool.Build(actool.Spec{Name: name, Description: desc, Schema: map[string]any{"type": "object", "properties": props}})
}

// ragCatalog: two core tools, three meaningful fixtures, and filler up to n tools.
func ragCatalog(n int) []actool.CoreTool {
	tools := []actool.CoreTool{
		ragTool("record_fact", "Record a fact"),
		ragTool("port_scan", "Scan TCP ports on a host, nmap-like service discovery", "host", "ports"),
		ragTool("sqli_probe", "Probe web parameters for SQL injection", "url", "param"),
		ragTool("http_request", "Send an HTTP request to a web endpoint", "url", "method"),
		ragTool("list_assets", "List assets"),
	}
	for i := len(tools); i < n; i++ {
		tools = append(tools, ragTool(fmt.Sprintf("filler_%02d", i), "Unrelated helper"))
	}
	return tools
}

func ragNames(tools []actool.CoreTool) (out []string) {
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}

const ragQuery = "Find SQL injection in the web login form"

func TestToolRAGRanksInjectionAboveScan(t *testing.T) {
	ranked, err := rankTools(ragQuery, ragCatalog(5), bm25Score)
	if err != nil {
		t.Fatal(err)
	}
	n := ragNames(ranked)
	if scan := slices.Index(n, "port_scan"); slices.Index(n, "sqli_probe") > scan || slices.Index(n, "http_request") > scan {
		t.Fatalf("ranking = %v, want sqli_probe and http_request above port_scan", n)
	}
	if n[0] != "sqli_probe" {
		t.Fatalf("top = %s, want sqli_probe", n[0])
	}
}

func TestToolRAGCoreKeptAndKRespected(t *testing.T) {
	keep := toolRAGKeep(nil)
	for _, k := range []int{1, 3} {
		out, projected, err := projectTools(ragQuery, ragCatalog(25), keep, k, bm25Score)
		if err != nil || !projected {
			t.Fatalf("k=%d: projected=%v err=%v", k, projected, err)
		}
		n := ragNames(out)
		if !slices.Contains(n, "record_fact") || !slices.Contains(n, "list_assets") {
			t.Fatalf("k=%d: core tool hidden: %v", k, n)
		}
		if slices.Contains(n, "goal_met") {
			t.Fatalf("k=%d: unbound core tool added: %v", k, n)
		}
		if nonCore := len(out) - 2; nonCore > k {
			t.Fatalf("k=%d: %d non-core tools selected: %v", k, nonCore, n)
		}
		if !slices.Contains(n, "sqli_probe") {
			t.Fatalf("k=%d: best match missing: %v", k, n)
		}
	}
}

// The ranking runs after ToolResolve, so a tool the binding/enabled filter dropped
// is never ranked even when it is the best lexical match, and AugmentTools still
// returns the resolved list unchanged (observation never filters).
func TestToolRAGNeverSelectsUnboundOrDisabled(t *testing.T) {
	oldResolve, oldAugment, oldPolicy := ToolResolve(), ToolAugment(), FindingTrafficBindingEnabled()
	t.Cleanup(func() {
		SetToolResolve(oldResolve)
		SetToolAugment(oldAugment)
		SetFindingTrafficBindingEnabled(oldPolicy)
	})
	SetToolAugment(nil)
	SetFindingTrafficBindingEnabled(nil)
	t.Setenv(toolRAGTopKEnv, "2")
	SetToolResolve(func(_ context.Context, _ string, in []actool.CoreTool) []actool.CoreTool {
		var out []actool.CoreTool
		for _, tool := range in {
			if tool.Name() != "sql_injection_exploit" {
				out = append(out, tool)
			}
		}
		return out
	})
	base := append(ragCatalog(25), ragTool("sql_injection_exploit", "Exploit SQL injection in web login form", "url"))
	ctx := WithToolQuery(t.Context(), ragQuery)
	out, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()
	n := ragNames(out)
	if len(out) != 25 || slices.Contains(n, "sql_injection_exploit") {
		t.Fatalf("AugmentTools did not return the resolved list unchanged: %v", n)
	}
	obs := observeToolRAG(ctx, "worker", out, def, nil)
	if slices.Contains(obs.Ranking, "sql_injection_exploit") {
		t.Fatalf("unbound tool ranked: %v", obs.Ranking)
	}
	if len(obs.Ranking) == 0 || obs.Ranking[0] != "sqli_probe" || obs.K != 2 || obs.Outcome != toolRAGProjected {
		t.Fatalf("ranking = %v k=%d outcome=%s, want sqli_probe first, k=2, projected", obs.Ranking, obs.K, obs.Outcome)
	}
}

func TestToolRAGFallbacks(t *testing.T) {
	keep := toolRAGKeep(nil)
	failing := func(string, []actool.CoreTool) ([]float64, error) { return nil, errors.New("boom") }
	for name, c := range map[string]struct {
		query string
		n     int
		score ToolScorer
	}{"empty query": {"  ", 25, bm25Score}, "catalog under 20": {ragQuery, 19, bm25Score}, "scoring error": {ragQuery, 25, failing}} {
		in := ragCatalog(c.n)
		out, projected, err := projectTools(c.query, in, keep, 1, c.score)
		if projected || (err != nil) != (name == "scoring error") || !slices.Equal(ragNames(out), ragNames(in)) {
			t.Errorf("%s: projected=%v err=%v out=%v", name, projected, err, ragNames(out))
		}
	}
}

func TestToolRAGDeterministic(t *testing.T) {
	keep := toolRAGKeep(nil)
	// "zzz" matches nothing: every candidate ties at 0 and the name tie-break decides.
	for _, q := range []string{ragQuery, "zzz"} {
		first, _, _ := projectTools(q, ragCatalog(30), keep, 4, bm25Score)
		for i := 0; i < 20; i++ {
			again, _, _ := projectTools(q, ragCatalog(30), keep, 4, bm25Score)
			if !slices.Equal(ragNames(first), ragNames(again)) {
				t.Fatalf("%q: run %d = %v, want %v", q, i, ragNames(again), ragNames(first))
			}
		}
	}
	ranked, _ := rankTools("zzz", ragCatalog(8), bm25Score)
	if n := ragNames(ranked); !slices.IsSorted(n) {
		t.Fatalf("ties not ordered by name: %v", n)
	}
}

// Pins the keep set beyond the core list: harness tools, Skill and deferred names
// are never hidden, even when the query matches something else.
func TestToolRAGKeepsHarnessSkillAndDeferred(t *testing.T) {
	in := append(ragCatalog(15), actool.DefaultTools()...)
	in = append(in, ragTool("Skill", "Load a skill"), ragTool("mcp__srv__crawl", "Crawl pages"))
	out, projected, err := projectTools(ragQuery, in, toolRAGKeep([]string{"mcp__srv__crawl"}), 1, bm25Score)
	if err != nil || !projected || len(in) <= 20 {
		t.Fatalf("projected=%v err=%v n=%d", projected, err, len(in))
	}
	n := ragNames(out)
	for _, want := range append(ragNames(actool.DefaultTools()), "Skill", "mcp__srv__crawl", "sqli_probe") {
		if !slices.Contains(n, want) {
			t.Fatalf("%s hidden: %v", want, n)
		}
	}
	if slices.Contains(n, "http_request") {
		t.Fatalf("k=1 exceeded: %v", n)
	}
}

// obsFixture: 2 keep tools, 2 global + 2 skill-gated deferred tools, and 19 candidates
// whose BM25 order for "alpha beta" is known: ab_tool (both terms), then the four
// single-term tools by name, then zero-score fillers by name.
func obsFixture() (tools []actool.CoreTool, def DeferredInfo) {
	tools = []actool.CoreTool{
		ragTool("record_fact", "Record a fact"),
		ragTool("list_assets", "List assets"),
		ragTool("mcp__srv__g1", "global one"),
		ragTool("mcp__srv__g2", "global two"),
		ragTool("mcp__srv__s1", "gated one"),
		ragTool("mcp__srv__s2", "gated two"),
		ragTool("ab_tool", "alpha beta"),
		ragTool("a1_tool", "alpha"), ragTool("a2_tool", "alpha"),
		ragTool("b1_tool", "beta"), ragTool("b2_tool", "beta"),
	}
	for i := len(tools); i < 25; i++ {
		tools = append(tools, ragTool(fmt.Sprintf("filler_%02d", i), "Unrelated helper"))
	}
	def = DeferredInfo{
		Deferred:    []string{"mcp__srv__g1", "mcp__srv__g2", "mcp__srv__s1", "mcp__srv__s2"},
		GlobalNames: []string{"mcp__srv__g1", "mcp__srv__g2"},
		Unlock:      actool.NewUnlockSet("mcp__srv__g1", "mcp__srv__g2"),
	}
	return tools, def
}

func TestToolRAGObserveComputedValues(t *testing.T) {
	t.Setenv(toolRAGTopKEnv, "3")
	tools, def := obsFixture()
	obs := observeToolRAG(WithToolQuery(t.Context(), "alpha beta"), "worker", tools, def, nil)
	wantHead := []string{"ab_tool", "a1_tool", "a2_tool", "b1_tool", "b2_tool", "filler_11", "filler_12"}
	if !slices.Equal(obs.Ranking[:len(wantHead)], wantHead) || len(obs.Ranking) != 19 {
		t.Fatalf("ranking = %v", obs.Ranking)
	}
	for name, c := range map[string][2]int{
		"candidate_count": {obs.CandidateCount, 19}, "keep_count": {obs.KeepCount, 2},
		"nonzero_count": {obs.NonzeroCount, 5}, "would_hide_count": {obs.WouldHideCount, 16},
		"host_list_count": {obs.HostListCount, 21}, "deferred_count": {obs.DeferredCount, 4},
		"locked_at_start": {obs.LockedAtStart, 2}, "query_tokens": {obs.QueryTokens, 2}, "k": {obs.K, 3},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %d, want %d", name, c[0], c[1])
		}
	}
	if obs.Outcome != toolRAGProjected || obs.Scorer != "bm25-v1" || obs.Mode != "observe" || obs.AgentKey != "worker" {
		t.Errorf("outcome=%s scorer=%s mode=%s agent=%s", obs.Outcome, obs.Scorer, obs.Mode, obs.AgentKey)
	}
	// Deferred names are never ranked; locked ones unlock later (AC12).
	for _, n := range def.Deferred {
		if slices.Contains(obs.Ranking, n) {
			t.Errorf("deferred %s ranked", n)
		}
	}
	if obs.UnlockedDuringRun() != 0 {
		t.Errorf("unlocked before skill load = %d", obs.UnlockedDuringRun())
	}
	def.Unlock.Add("mcp__srv__s1", "mcp__srv__s2")
	if obs.UnlockedDuringRun() != 2 {
		t.Errorf("unlocked after skill load = %d, want 2", obs.UnlockedDuringRun())
	}
	wantClass := map[string]string{
		"ab_tool": toolClassCandidate, "record_fact": toolClassKeep, "mcp__srv__g1": toolClassDeferredGlobal,
		"mcp__srv__s1": toolClassDeferredLocked,
	}
	for n, c := range wantClass {
		if obs.Classes[n] != c {
			t.Errorf("class[%s] = %q, want %q", n, obs.Classes[n], c)
		}
	}
}

func TestToolRAGObserveOutcomes(t *testing.T) {
	ctx := WithToolQuery(t.Context(), ragQuery)
	if o := observeToolRAG(ctx, "planner", ragCatalog(19), DeferredInfo{}, nil); o.Outcome != toolRAGSmallCatalog || len(o.Ranking) == 0 || o.WouldHideCount != 0 {
		t.Errorf("19 tools: outcome=%s ranking=%v hide=%d", o.Outcome, o.Ranking, o.WouldHideCount)
	}
	if o := observeToolRAG(WithToolQuery(t.Context(), "  "), "worker", ragCatalog(25), DeferredInfo{}, nil); o.Outcome != toolRAGNoQuery || len(o.Ranking) != 0 || o.QueryTokens != 0 {
		t.Errorf("empty query: outcome=%s ranking=%v", o.Outcome, o.Ranking)
	}
	if o := observeToolRAG(t.Context(), "worker", ragCatalog(25), DeferredInfo{}, nil); o.Outcome != toolRAGNoQuery {
		t.Errorf("no query value: outcome=%s", o.Outcome)
	}
	if o := observeToolRAG(ctx, "worker", ragCatalog(25), DeferredInfo{}, nil); o.Outcome != toolRAGProjected || o.Ranking[0] != "sqli_probe" {
		t.Errorf("25 tools: outcome=%s ranking=%v", o.Outcome, o.Ranking)
	}
	for _, key := range []string{"mainagent", "chat", ""} {
		if o := observeToolRAG(ctx, key, ragCatalog(25), DeferredInfo{}, nil); o != nil {
			t.Errorf("agent %q observed", key)
		}
	}
}

func TestToolRAGObserveDeterministicAndNoSignal(t *testing.T) {
	ctx := WithToolQuery(t.Context(), ragQuery)
	first := observeToolRAG(ctx, "worker", ragCatalog(30), DeferredInfo{}, nil)
	for i := 0; i < 20; i++ {
		if again := observeToolRAG(ctx, "worker", ragCatalog(30), DeferredInfo{}, nil); !slices.Equal(first.Ranking, again.Ranking) {
			t.Fatalf("run %d = %v, want %v", i, again.Ranking, first.Ranking)
		}
	}
	zz := observeToolRAG(WithToolQuery(t.Context(), "zzz"), "worker", ragCatalog(30), DeferredInfo{}, nil)
	if zz.Outcome != toolRAGNoSignal || zz.NonzeroCount != 0 || zz.WouldHideCount != 0 || !slices.IsSorted(zz.Ranking) {
		t.Fatalf("zzz: outcome=%s nonzero=%d hide=%d ranking=%v", zz.Outcome, zz.NonzeroCount, zz.WouldHideCount, zz.Ranking)
	}
}

func TestToolRAGCaps(t *testing.T) {
	// Query: 1 MiB of "x " cut to 4096 bytes = 2048 tokens.
	q := strings.Repeat("x ", 1<<19)
	o := observeToolRAG(WithToolQuery(t.Context(), q), "worker", ragCatalog(25), DeferredInfo{}, nil)
	if !o.QueryTruncated || o.QueryTokens != toolRAGMaxQueryBytes/2 {
		t.Errorf("truncated=%v tokens=%d, want true/%d", o.QueryTruncated, o.QueryTokens, toolRAGMaxQueryBytes/2)
	}
	if o := observeToolRAG(WithToolQuery(t.Context(), ragQuery), "worker", ragCatalog(25), DeferredInfo{}, nil); o.QueryTruncated {
		t.Error("short query reported as truncated")
	}
	// Rune boundary: a 3-byte rune never gets split.
	cut, did := truncUTF8(strings.Repeat("\u20ac", 3000), toolRAGMaxQueryBytes)
	if !did || !utf8.ValidString(cut) || len(cut) != 4095 {
		t.Errorf("cut: valid=%v len=%d truncated=%v", utf8.ValidString(cut), len(cut), did)
	}
	if s, did := truncUTF8("abc", 3); s != "abc" || did {
		t.Errorf("exact fit cut: %q %v", s, did)
	}

	// Description: 1 MiB, the match word sits past the cap, so it is not scored.
	var seen []actool.CoreTool
	spy := func(q string, tools []actool.CoreTool) ([]float64, error) {
		seen = tools
		return bm25Score(q, tools)
	}
	big := ragTool("big_tool", "head "+strings.Repeat("x ", 1<<19)+"needle", "p")
	tools := append(ragCatalog(25), big)
	o = observeToolRAG(WithToolQuery(t.Context(), "needle head"), "worker", tools, DeferredInfo{}, spy)
	for _, c := range seen {
		if c.Name() == "big_tool" {
			if n := len(toolScoringText(c)); n > toolRAGMaxToolBytes {
				t.Errorf("scored text = %d bytes, cap %d", n, toolRAGMaxToolBytes)
			}
			if strings.Contains(toolScoringText(c), "needle") || !strings.Contains(toolScoringText(c), "head") {
				t.Error("cap cut the wrong end")
			}
		}
	}
	if o.Ranking[0] != "big_tool" {
		t.Errorf("head match not ranked first: %v", o.Ranking[:3])
	}

	// 600 candidates: 512 scored, the true count kept, ranking capped at 256.
	many := ragCatalog(25)
	for i := 0; i < 575; i++ {
		many = append(many, ragTool(fmt.Sprintf("many_%03d", i), "alpha"))
	}
	scored := 0
	o = observeToolRAG(WithToolQuery(t.Context(), "alpha"), "worker", many, DeferredInfo{},
		func(q string, tools []actool.CoreTool) ([]float64, error) {
			scored = len(tools)
			return bm25Score(q, tools)
		})
	if len(many) != 600 || scored != 512 || len(o.Ranking) != 256 || o.CandidateCount != 600-2 {
		t.Errorf("tools=%d scored=%d ranking=%d candidates=%d", len(many), scored, len(o.Ranking), o.CandidateCount)
	}
}

func TestToolRAGObserveNeverTouchesInput(t *testing.T) {
	tools := ragCatalog(30)
	// Reverse-alphabetical so any in-place sort by name would be visible.
	slices.SortFunc(tools, func(a, b actool.CoreTool) int { return strings.Compare(b.Name(), a.Name()) })
	before := slices.Clone(tools)
	c := cap(tools)
	observeToolRAG(WithToolQuery(t.Context(), ragQuery), "worker", tools, DeferredInfo{}, nil)
	if len(tools) != len(before) || cap(tools) != c {
		t.Fatal("length or capacity changed")
	}
	full := tools[:c]
	for i := range before {
		if full[i] != before[i] {
			t.Fatalf("element %d replaced", i)
		}
	}
}

func TestToolRAGObserveScorerFailures(t *testing.T) {
	tools := ragCatalog(25)
	before := ragNames(tools)
	ctx := WithToolQuery(t.Context(), ragQuery)
	o := observeToolRAG(ctx, "worker", tools, DeferredInfo{}, func(string, []actool.CoreTool) ([]float64, error) { return nil, errors.New("secret-detail") })
	if o.Outcome != toolRAGScorerError || o.FailType != "*errors.errorString" || len(o.Ranking) != 0 || o.WouldHideCount != 0 {
		t.Errorf("error: %+v", o)
	}
	o = observeToolRAG(ctx, "worker", tools, DeferredInfo{}, func(string, []actool.CoreTool) ([]float64, error) { panic("secret-detail") })
	if o.Outcome != toolRAGObserverPanic || o.FailType != "string" || len(o.Ranking) != 0 || o.NonzeroCount != 0 || o.DurationUS < 0 {
		t.Errorf("panic: %+v", o)
	}
	o = observeToolRAG(ctx, "worker", tools, DeferredInfo{}, func(string, []actool.CoreTool) ([]float64, error) { return []float64{1}, nil })
	if o.Outcome != toolRAGScorerError {
		t.Errorf("short scores: outcome=%s", o.Outcome)
	}
	if !slices.Equal(ragNames(tools), before) {
		t.Error("input changed after failures")
	}
}

func TestToolRAGObservedNamesFollowNameRule(t *testing.T) {
	long := strings.Repeat("n", toolRAGMaxNameBytes+1)
	tools := append(ragCatalog(25), ragTool(long, "alpha"), ragTool("bad\x01name", "alpha"), ragTool("bad\xffutf", "alpha"), ragTool(strings.Repeat("n", toolRAGMaxNameBytes), "alpha"))
	o := observeToolRAG(WithToolQuery(t.Context(), "alpha"), "worker", tools, DeferredInfo{}, nil)
	if got := strings.Count(strings.Join(o.Ranking, "\n"), toolRAGUnknownName); got != 3 {
		t.Errorf("_unknown count = %d, want 3: %v", got, o.Ranking[:5])
	}
	for _, n := range o.Ranking {
		if len(n) > toolRAGMaxNameBytes || strings.ContainsFunc(n, func(r rune) bool { return r < 0x20 }) {
			t.Errorf("name %q breaks the rule", n)
		}
	}
	if !slices.Contains(o.Ranking, strings.Repeat("n", toolRAGMaxNameBytes)) {
		t.Error("128-byte name wrongly replaced")
	}
}

func TestToolRAGRankedNamesAreCandidatesOfTheList(t *testing.T) {
	tools, def := obsFixture()
	o := observeToolRAG(WithToolQuery(t.Context(), "alpha beta"), "worker", tools[:20], def, nil) // some tools "dropped by ToolResolve"
	have := ragNames(tools[:20])
	keep := toolRAGKeep(def.Deferred)
	for _, n := range o.Ranking {
		if !slices.Contains(have, n) || keep[n] {
			t.Errorf("ranked %q: not in the list or a keep/deferred name", n)
		}
	}
}

func TestPlannerToolQuery(t *testing.T) {
	got := plannerToolQuery(" goal ", []TriggerEvent{{Summary: "s", NewGoal: "ng", Goals: []string{"g1", "g2"}, Hints: []string{"h"}}})
	if want := "goal  s ng g1 g2 h"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if plannerToolQuery("", nil) != "" {
		t.Error("empty inputs give a non-empty query")
	}
}

// Equal scores between tools with different names are ordered by name, whatever
// the input order: the stored ranking must not depend on catalog order. A higher
// score still wins over a name that sorts first.
func TestToolRAGTiesBetweenDistinctNamesOrderByName(t *testing.T) {
	tools := []actool.CoreTool{
		ragTool("zeta_tool", "x"), ragTool("alpha_tool", "x"), ragTool("mid_tool", "x"), ragTool("beta_tool", "x"),
	}
	byName := map[string]float64{"zeta_tool": 1, "alpha_tool": 1, "mid_tool": 2, "beta_tool": 1}
	scorer := func(_ string, ts []actool.CoreTool) ([]float64, error) {
		out := make([]float64, len(ts))
		for i, tl := range ts {
			out[i] = byName[tl.Name()]
		}
		return out, nil
	}
	want := []string{"mid_tool", "alpha_tool", "beta_tool", "zeta_tool"}
	for _, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}} {
		in := make([]actool.CoreTool, 0, len(order))
		for _, i := range order {
			in = append(in, tools[i])
		}
		ranked, err := rankTools("q", in, scorer)
		if err != nil {
			t.Fatal(err)
		}
		if got := ragNames(ranked); !slices.Equal(got, want) {
			t.Errorf("input order %v: ranking %v, want %v", order, got, want)
		}
	}
}
