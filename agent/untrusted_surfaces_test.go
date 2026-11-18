package agent

import (
	"context"
	"iter"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/mkilijanek/artex/db"
)

// Phase D.1, S2 red tests for the code-owned prompt rules (D4, AC15) and the cold-digest
// compressor (AC20). The envelope tests live in data_envelope_test.go.

// requireTestDB opens the test database. A missing database config skips only when no
// DSN is set; with ARTEX_PG_DSN set, an unreachable database is a failure, so a gate run
// cannot pass by skipping.
func requireTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		if os.Getenv("ARTEX_PG_DSN") != "" {
			t.Fatalf("ARTEX_PG_DSN is set but unusable: %v", err)
		}
		t.Skipf("no database config (%v)", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		if os.Getenv("ARTEX_PG_DSN") != "" {
			t.Fatalf("ARTEX_PG_DSN is set but postgres is unreachable: %v", err)
		}
		t.Skipf("postgres unavailable (%v)", err)
	}
	return d
}

// requestRecorder is a provider that answers every request with a one-line summary and
// keeps the requests it was given.
type requestRecorder struct {
	mu   sync.Mutex
	reqs []llm.CompletionRequest
}

func (r *requestRecorder) Complete(_ context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("digest body")}}, "stop", llm.Usage{}, nil
}

func (r *requestRecorder) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(func(llm.StreamEvent, error) bool) {}
}

func (r *requestRecorder) Requests() []llm.CompletionRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.CompletionRequest(nil), r.reqs...)
}

const (
	digestMarkMember = "MARK-DIGEST-MEMBER"
	digestMarkBenign = "benign member summary"
)

// compressionRequest runs the real cold-digest compressor on two fact members and returns
// the request it sent to the model.
func compressionRequest(t *testing.T) llm.CompletionRequest {
	t.Helper()
	rec := &requestRecorder{}
	c := NewCompactor(rec, "fixture")
	g := newColdGraph([]cgNode{fact(1), fact(2)}, nil)
	nodes := map[int64]*db.Node{
		1: {ID: 1, Kind: db.KindFact, State: "confirmed", Origin: "worker-1", Payload: []byte(`{"summary":` + jsonString(digestMarkMember+envForgery(envNonce)) + `}`)},
		2: {ID: 2, Kind: db.KindFact, State: "confirmed", Origin: "worker-1", Payload: []byte(`{"summary":"` + digestMarkBenign + `"}`)},
	}
	if _, err := c.compress(t.Context(), g, block{Members: []int64{1, 2}}, nodes); err != nil {
		t.Fatalf("compress: %v", err)
	}
	reqs := rec.Requests()
	if len(reqs) != 1 {
		t.Fatalf("compressor made %d model calls, want 1", len(reqs))
	}
	return reqs[0]
}

// T-DIGEST-1 (AC20, C12): the compressor request carries the compactor rule (the data rule
// plus the digest-specific consequence) once in its system prompt, and the member text is
// framed as data, with a forged frame close neutralised.
func TestDigestCompressorDataRule(t *testing.T) {
	setDataNonce(t, envNonce)
	req := compressionRequest(t)
	system := strings.Join(req.System, "\n")
	if n := strings.Count(system, untrustedDataRules); n != 1 {
		t.Errorf("compressor system carries the untrusted data rule %d times, want 1", n)
	}
	if n := strings.Count(system, compactorDataRule); n != 1 {
		t.Errorf("compressor system carries the compactor rule %d times, want 1", n)
	}

	input := ""
	for _, m := range req.Messages {
		input += m.Text()
	}
	if len(frameFields(input, envNonce)) == 0 {
		t.Fatalf("compressor input has no data frame with the injected nonce: %q", truncateForLog(input))
	}
	inside, outside := splitFrames(t, input, envNonce)
	if !strings.Contains(inside, digestMarkMember) || strings.Contains(outside, digestMarkMember) {
		t.Errorf("member text is not only inside a frame: inside=%q outside=%q", truncateForLog(inside), truncateForLog(outside))
	}
	if strings.Contains(outside, envForgedGoal) {
		t.Errorf("a forged frame close let member text escape the frame: %q", truncateForLog(outside))
	}
	if opens, closes := len(frameFields(input, envNonce)), strings.Count(input, dataClosePrefix+envNonce+"]]"); opens != closes || forgedFrames(input, envNonce) > 0 {
		t.Errorf("frame markers unbalanced: %d opens, %d closes, %d forged (forged markers must be neutralised)", opens, closes, forgedFrames(input, envNonce))
	}
}

// T-PROMPT-1 (AC15, C2): a DB prompt override that lacks the untrusted block must not drop
// the code-owned rules. Worker, pentest, main, auto and chat carry the untrusted-content
// rule exactly once; the planner and the compactor carry the data rule exactly once. The
// same holds with ARTEX_DATA_ENVELOPE=0 (the opt-out switches off only the envelope and the
// goal anchor), and the shipped defaults carry each rule once as well (no duplicates).
func TestCodeOwnedTailsSurviveOverrideAndOptOut(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override bool
		envelope string // ARTEX_DATA_ENVELOPE
	}{
		{"override", true, ""},
		{"override-optout", true, "0"},
		{"default", false, ""},
		{"default-optout", false, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := PromptOverride()
			if tc.override {
				SetPromptOverride(func(string) (string, bool) { return "BARE BODY", true })
			} else {
				SetPromptOverride(nil)
			}
			t.Cleanup(func() { SetPromptOverride(prev) })
			t.Setenv("ARTEX_DATA_ENVELOPE", tc.envelope)
			setDataNonce(t, envNonce)

			untrusted := map[string]string{
				"worker":  workerSystem("", "", "/data", "/run"),
				"pentest": chatSystem("pentest", "/data", "/work"),
				"main":    mainAgentSystem("goal", "/data", "/work"),
				"auto":    chatSystem("auto", "/data", "/work"),
				"chat":    chatSystem("assistant", "/data", "/work"),
			}
			for role, sys := range untrusted {
				if tc.override && !strings.HasPrefix(sys, "BARE BODY") {
					t.Errorf("%s: the override body was not used: %q", role, truncateForLog(sys))
				}
				if n := strings.Count(sys, untrustedContentInstructions); n != 1 {
					t.Errorf("%s: untrusted-content rule rendered %d times, want exactly 1", role, n)
				}
			}

			for _, cache := range []string{"0", "1"} {
				t.Setenv("ARTEX_CACHE_PREFIX", cache)
				sys := plannerSystem("goal", "/data", "/work")
				if tc.override && !strings.HasPrefix(sys, "BARE BODY") {
					t.Errorf("planner cache=%s: the override body was not used: %q", cache, truncateForLog(sys))
				}
				if n := strings.Count(sys, untrustedDataRules); n != 1 {
					t.Errorf("planner cache=%s: untrusted data rule rendered %d times, want exactly 1", cache, n)
				}
			}

			system := strings.Join(compressionRequest(t).System, "\n")
			if n := strings.Count(system, untrustedDataRules); n != 1 {
				t.Errorf("compactor: untrusted data rule rendered %d times, want exactly 1", n)
			}
			if n := strings.Count(system, compactorDataRule); n != 1 {
				t.Errorf("compactor: compactor rule rendered %d times, want exactly 1", n)
			}
		})
	}
}
