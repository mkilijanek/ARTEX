package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
)

// Phase D.1, S6 review tests: opt-out byte identity against literal pre-change JSON,
// task credentials, and the actual size bounds of the bounded planner input.

// T-OPTOUT-LIT (AC16): with ARTEX_DATA_ENVELOPE=0 a human hint and an agent-origin hint reach
// the planner (both context modes) and the worker as the pre-change hint JSON, with no
// "origin" key. The expected strings are literals, not built from graphOverviewData().
func TestOptOutHintJSONIsPreChangeLiteral(t *testing.T) {
	t.Setenv("ARTEX_DATA_ENVELOPE", "0")
	setDataNonce(t, envNonce)
	hintLiteral := func(id int64, text string) string {
		return fmt.Sprintf(`{"id":%d,"state":"active","text":%q}`, id, text)
	}

	for _, bounded := range []string{"0", "1"} {
		t.Run("planner/bounded="+bounded, func(t *testing.T) {
			t.Setenv("ARTEX_BOUNDED_CONTEXT", bounded)
			f := newPlannerEnvFixture(t)
			humanID, err := f.ts.AddNode(db.KindHint, map[string]any{"text": "literal human hint"}, 0, "active", "human", nil)
			if err != nil {
				t.Fatal(err)
			}
			agentID, err := f.ts.AddNode(db.KindHint, map[string]any{"text": "literal agent hint"}, 0, "active", "worker-7", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, input := runPlannerRound(t, f, envGoal, nil)
			for _, lit := range []string{hintLiteral(humanID, "literal human hint"), hintLiteral(agentID, "literal agent hint")} {
				if !strings.Contains(input, lit) {
					t.Errorf("pre-change hint JSON %s not found in the opt-out planner input", lit)
				}
			}
		})
	}

	t.Run("worker", func(t *testing.T) {
		t.Setenv("ARTEX_BOUNDED_CONTEXT", "0")
		t.Setenv(workerFlags, "")
		d := requireTestDB(t)
		defer d.Close()
		exp, err := d.CreateExploration("opt-out worker fixture", "keep assigned scope")
		if err != nil {
			t.Fatal(err)
		}
		defer d.Exec(`DELETE FROM explorations WHERE id=$1`, exp)
		ts := d.Exploration(exp)
		intentID, err := ts.AddIntent(map[string]any{"summary": "inspect the fixture"}, 1, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		intent, err := ts.GetNode(intentID)
		if err != nil {
			t.Fatal(err)
		}
		humanID, err := ts.AddNode(db.KindHint, map[string]any{"text": "literal human hint"}, 0, "active", "human", nil)
		if err != nil {
			t.Fatal(err)
		}
		agentID, err := ts.AddNode(db.KindHint, map[string]any{"text": "literal agent hint"}, 0, "active", "worker-7", nil)
		if err != nil {
			t.Fatal(err)
		}
		rec := &requestRecorder{}
		w := NewWorker(rec, "fixture", t.TempDir(), nil, 100000, 1)
		w.SetNonStreaming(func() bool { return true })
		if _, _, err := w.Execute(t.Context(), "fixture", 0, nil, ts, intent, nil, nil, nil, nil); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		all := ""
		for _, m := range rec.Requests()[0].Messages {
			all += m.Text()
		}
		for _, lit := range []string{hintLiteral(humanID, "literal human hint"), hintLiteral(agentID, "literal agent hint")} {
			if !strings.Contains(all, lit) {
				t.Errorf("pre-change hint JSON %s not found in the opt-out worker launch", lit)
			}
		}
		if len(frameFields(all, envNonce)) != 0 {
			t.Errorf("the opt-out worker launch carries a frame")
		}
	})
}

// With the envelope on, task credentials (an agent writes them through save_task_credential)
// sit in their own frame in both context modes; the code-owned note stays outside.
func TestTaskCredentialsFramed(t *testing.T) {
	const markCred = "MARK-CRED-NOTE"
	for _, bounded := range []string{"0", "1"} {
		t.Run("bounded="+bounded, func(t *testing.T) {
			t.Setenv("ARTEX_BOUNDED_CONTEXT", bounded)
			t.Setenv("ARTEX_DATA_ENVELOPE", "")
			setDataNonce(t, envNonce)
			f := newPlannerEnvFixture(t)
			if _, err := f.ts.UpsertTaskCredential(f.taskID, db.TaskCredentialInput{Domain: "app.example.test", Username: "mark-user", Password: "pw", Source: "agent", Note: markCred + envForgery(envNonce)}); err != nil {
				t.Fatal(err)
			}
			_, input := runPlannerRound(t, f, envGoal, nil)
			if !strings.Contains(input, dataOpenPrefix+envNonce+" field=task_credentials]]") {
				t.Fatalf("no task_credentials frame: %q", truncateForLog(input))
			}
			inside, outside := splitFrames(t, input, envNonce)
			for _, mark := range []string{markCred, "mark-user"} {
				if !strings.Contains(inside, mark) || strings.Contains(outside, mark) {
					t.Errorf("%s must appear only inside a frame", mark)
				}
			}
			if !strings.Contains(outside, "task_credentials_note") {
				t.Errorf("the code-owned credentials note is missing outside the frames")
			}
			if n := forgedFrames(input, envNonce); n > 0 {
				t.Errorf("%d forged frames survived", n)
			}
			if bounded == "1" {
				start := strings.Index(input, plannerSnapshotPrefix)
				var view map[string]json.RawMessage
				dec := json.NewDecoder(strings.NewReader(input[start+len(plannerSnapshotPrefix):]))
				if err := dec.Decode(&view); err != nil {
					t.Fatal(err)
				}
				if _, ok := view["task_credentials"]; ok {
					t.Errorf("task_credentials is inside the snapshot JSON")
				}
			}
		})
	}
}

// frameBody returns the content of the first frame of field carrying nonce.
func frameBody(s, nonce, field string) (string, bool) {
	open := dataOpenPrefix + nonce + " field=" + field + "]]\n"
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, "\n"+dataClosePrefix+nonce+"]]")
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// Pins the real size bounds of the bounded-context planner input (AC13): the snapshot JSON
// stays within plannerPacketBudget, and the derived frames, which sit after it, stay within
// their own projection budget (overviewBudget) plus a fixed framing overhead. The two budgets
// apply per part, not to the sum. The trigger frames are separate again: worker_output is
// capped by truncOutput, the finding detail is not capped anywhere.
func TestBoundedPlannerInputBudgetsPerPart(t *testing.T) {
	t.Setenv("ARTEX_BOUNDED_CONTEXT", "1")
	t.Setenv("ARTEX_DATA_ENVELOPE", "")
	setDataNonce(t, envNonce)
	f := newPlannerEnvFixture(t)
	big := strings.Repeat("y", 3000)
	for i := 0; i < 40; i++ {
		if _, err := f.ts.AddNode(db.KindFact, map[string]any{"summary": fmt.Sprintf("%s fact %d", big, i), "confidence": "observed"}, 0, "confirmed", "worker-1", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := f.ts.AddNode(db.KindFinding, map[string]any{"summary": fmt.Sprintf("%s finding %d", big, i)}, 9, "confirmed", "worker-1", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.ts.AppendActivity(db.Activity{NodeID: &f.intentID, Worker: "worker-1", Kind: "result", Tool: "Bash", Summary: "done", Detail: strings.Repeat("w", 5000)}); err != nil {
		t.Fatal(err)
	}
	findingDetail := strings.Repeat("d", 50000)
	_, input := runPlannerRound(t, f, envGoal, []TriggerEvent{{Kind: "done", IntentID: f.intentID}, {Kind: "finding", IntentID: f.intentID, Detail: findingDetail}})

	start := strings.Index(input, plannerSnapshotPrefix)
	if start < 0 {
		t.Fatalf("no snapshot: %q", truncateForLog(input))
	}
	rest := input[start+len(plannerSnapshotPrefix):]
	var view map[string]json.RawMessage
	dec := json.NewDecoder(strings.NewReader(rest))
	if err := dec.Decode(&view); err != nil {
		t.Fatal(err)
	}
	journal := int(dec.InputOffset())
	after := rest[journal:]
	payload, frames := 0, 0
	for _, field := range []string{"recent_facts", "finding_list", "cold_digests", "related_tasks", "agent_hints", "task_credentials", "projection_notes"} {
		if body, ok := frameBody(after, envNonce, field); ok {
			payload += len(body)
			frames++
		}
	}
	// Everything after the snapshot JSON up to the goal anchor is frames and separators.
	anchor := plannerGoalAnchor(envGoal, "")
	derivedSpan := after[:strings.Index(after, "\n\nJudge the goals")]
	overhead := len(derivedSpan) - payload
	t.Logf("journal JSON=%d (budget %d); derived frames: %d frames, payload=%d (projection budget %d), framing overhead=%d; goal anchor=%d",
		journal, plannerPacketBudget, frames, payload, overviewBudget, overhead, len(anchor))
	if journal > plannerPacketBudget {
		t.Errorf("snapshot JSON %d > plannerPacketBudget %d", journal, plannerPacketBudget)
	}
	if frames == 0 || payload > overviewBudget {
		t.Errorf("derived frame payload %d outside (0, overviewBudget %d] over %d frames", payload, overviewBudget, frames)
	}
	if overhead > 1200 {
		t.Errorf("framing overhead %d bytes is not small and fixed", overhead)
	}

	// Trigger text: worker_output is capped, the finding detail is passed through whole.
	wo, _ := frameBody(input, envNonce, "worker_output")
	fd, _ := frameBody(input, envNonce, "finding")
	t.Logf("trigger frames: worker_output=%d bytes (cap 800 runes + notice), finding=%d bytes (uncapped)", len(wo), len(fd))
	if len(wo) > 800+120 {
		t.Errorf("worker_output frame %d bytes is over the truncOutput cap", len(wo))
	}
	if len(fd) < len(findingDetail) {
		t.Errorf("finding detail was truncated (%d < %d): update the documented bound", len(fd), len(findingDetail))
	}
}
