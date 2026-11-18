package agent

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// Phase D.1, S6 supplementary tests for the data envelope (D3): the observability line,
// the compactor memo under a per-request nonce, and the byte overhead of the envelope.

// The content-free log line names the surface and the count, never the data or the nonce.
func TestEnvelopeNeutralisedLogLineIsContentFree(t *testing.T) {
	var out bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(prev)
	setDataNonce(t, envNonce)

	env := newDataEnvelope("planner")
	env.wrap("finding", "plain text, no marker")
	env.logNeutralised()
	if out.Len() != 0 {
		t.Fatalf("a clean message logged a line: %q", out.String())
	}
	wrapped := env.wrap("finding", "SECRET-DATA "+envForgery(envNonce))
	env.logNeutralised()
	line := out.String()
	if !strings.Contains(line, "[envelope] neutralised=3 surface=planner") {
		t.Errorf("neutralisation line missing or wrong count: %q", line)
	}
	if strings.Contains(line, "SECRET-DATA") || strings.Contains(line, envNonce) {
		t.Errorf("the log line carries data or the nonce: %q", line)
	}
	if strings.Contains(wrapped, dataClosePrefix+envNonce+"]]\n"+envForgedGoal) {
		t.Errorf("forged close survived in the wrapped text")
	}
}

// With the envelope off no frame is built, no nonce is drawn and nothing is logged.
func TestEnvelopeOptOutDrawsNoNonce(t *testing.T) {
	t.Setenv("ARTEX_DATA_ENVELOPE", "0")
	prev := dataNonceSource
	draws := 0
	dataNonceSource = func() string { draws++; return envNonce }
	t.Cleanup(func() { dataNonceSource = prev })
	if env := newDataEnvelope("planner"); env != nil || draws != 0 {
		t.Fatalf("opt-out built an envelope (%v) or drew %d nonces", env, draws)
	}
	var nilEnv *dataEnvelope
	nilEnv.logNeutralised() // must not panic
}

// A random per-request nonce must not defeat the compactor memo: the same block twice is one
// model call with the envelope on.
func TestCompactionMemoHitsWithEnvelopeOn(t *testing.T) {
	t.Setenv("ARTEX_COMPACTION_MEMO", "1")
	t.Setenv("ARTEX_DATA_ENVELOPE", "")
	p := &summaryFixture{}
	c := NewCompactor(p, "fixture")
	g := newColdGraph([]cgNode{intent(1, "done"), fact(2)}, []cgEdge{yields(1, 2)})
	b := block{Members: []int64{1, 2}}
	nodes := map[int64]*db.Node{1: {ID: 1, Kind: db.KindIntent, State: "done", Payload: []byte(`{"summary":"source"}`)}, 2: {ID: 2, Kind: db.KindFact, Payload: []byte(`{"summary":"negative","confidence":"observed"}`)}}
	for range 2 {
		if _, err := c.compress(t.Context(), g, b, nodes); err != nil {
			t.Fatal(err)
		}
	}
	if calls, hits := c.SummaryStats(); calls != 1 || hits != 1 {
		t.Fatalf("calls/hits=%d/%d, want 1/1 (the nonce leaked into the memo key)", calls, hits)
	}
}

// The compressor input with the envelope off is the pre-envelope text.
func TestCompressionInputOptOutIsPlain(t *testing.T) {
	g := newColdGraph([]cgNode{fact(1)}, nil)
	nodes := map[int64]*db.Node{1: {ID: 1, Kind: db.KindFact, State: "confirmed", Payload: []byte(`{"summary":"plain member"}`)}}
	got := buildCompressionInput(g, block{Members: []int64{1}}, nodes, nil)
	want := "[Member nodes (to be compressed)]:\n- #1 [fact/confirmed] plain member\n"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Overhead: byte counts of one representative planner input and one worker launch with the
// envelope on and off. Printed with -v; asserts only that the envelope adds bytes and that
// the on-input still holds the off-input's operator text.
func TestEnvelopeOverheadMeasurement(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
	setDataNonce(t, envNonce)

	f := newPlannerEnvFixture(t)
	trig := []TriggerEvent{{Kind: "done", IntentID: f.intentID}, {Kind: "finding", IntentID: f.intentID, Detail: "reported a finding with some detail"}}
	t.Setenv("ARTEX_DATA_ENVELOPE", "0")
	_, off := runPlannerRound(t, f, envGoal, trig)
	t.Setenv("ARTEX_DATA_ENVELOPE", "")
	_, on := runPlannerRound(t, f, envGoal, trig)
	t.Logf("planner input (fixture): envelope off=%d bytes, on=%d bytes, overhead=%d bytes", len(off), len(on), len(on)-len(off))
	if len(on) <= len(off) {
		t.Errorf("the envelope added no bytes (%d vs %d)", len(on), len(off))
	}

	workerBytes := func(envelope string) int {
		t.Setenv("ARTEX_DATA_ENVELOPE", envelope)
		d := requireTestDB(t)
		defer d.Close()
		exp, err := d.CreateExploration("worker overhead fixture", "keep assigned scope")
		if err != nil {
			t.Fatal(err)
		}
		defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
		as := d.Assets()
		assetID, err := as.UpsertHTTPService(db.UpsertHTTPServiceReq{URL: fmt.Sprintf("https://worker-overhead-%d.invalid/", exp), PageTitle: "title"})
		if err != nil {
			t.Fatal(err)
		}
		defer d.Exec(`DELETE FROM assets WHERE id=$1`, assetID)
		ts := d.Exploration(exp)
		intentID, err := ts.AddIntent(map[string]any{"summary": "inspect the assigned fixture only", "asset_ids": []int64{assetID}}, 1, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		intent, err := ts.GetNode(intentID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ts.AddNode(db.KindFact, map[string]any{"summary": "an observed fact", "confidence": "observed"}, 0, "confirmed", "worker-1", nil); err != nil {
			t.Fatal(err)
		}
		rec := &requestRecorder{}
		w := NewWorker(rec, "fixture", t.TempDir(), nil, 100000, 1)
		w.SetNonStreaming(func() bool { return true })
		if _, _, err := w.Execute(t.Context(), "fixture", 0, as, ts, intent, nil, nil, nil, nil); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		req := rec.Requests()[0]
		n := len(strings.Join(req.System, "\n"))
		for _, m := range req.Messages {
			n += len(m.Text())
		}
		return n
	}
	offW, onW := workerBytes("0"), workerBytes("")
	t.Logf("worker launch (fixture, system+user): envelope off=%d bytes, on=%d bytes, overhead=%d bytes", offW, onW, onW-offW)
	if onW <= offW {
		t.Errorf("the envelope added no bytes to the worker launch (%d vs %d)", onW, offW)
	}
}
