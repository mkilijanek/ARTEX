package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Autumn-27/artex/db"
)

const overviewBudget = 16 * 1024
const plannerPacketBudget = 40 * 1024
const workerAssetBudget = 8 * 1024

// workerDependencyCap bounds how many dependency observations are loaded by id
// for one launch; the remainder is reported, never silently dropped.
const workerDependencyCap = 40

// Projection only changes launch context. Original graph/evidence and mandatory
// system constraints remain intact. Omitted observations are never negatives.
func projectOverview(data map[string]any) map[string]any {
	raw, err := json.Marshal(data)
	if err != nil {
		return map[string]any{"detail_required": true}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var normalized map[string]any
	if d.Decode(&normalized) != nil {
		return map[string]any{"detail_required": true}
	}

	// Mandatory instructions and decision signals are never truncated. The budget
	// applies to optional detail; a mandatory overflow is explicit, not data loss.
	mandatory := map[string]bool{"task": true, "goals": true, "hints": true,
		"scope": true, "stagnation_warning": true, "coverage": true,
		"intent_context": true, "intent_id": true, "scope_required_before_action": true, "task_credentials": true, "task_credentials_note": true}
	if normalized["intent_id"] != nil {
		mandatory["recent_facts"], mandatory["finding_list"] = true, true
		mandatory["recent_facts_filtered_unrelated"], mandatory["finding_list_filtered_unrelated"] = true, true
		mandatory["recent_facts_omitted_items"], mandatory["finding_list_omitted_items"] = true, true
		mandatory["retrieval"] = true
		mandatory["dependency_evidence_omitted"], mandatory["dependency_evidence_unavailable"] = true, true
	}
	out := map[string]any{"projection": true, "detail_required": true,
		"retrieval": "graph_overview, node_detail(id), list_facts, list_assets; omitted evidence is not negative evidence"}
	omitted := map[string]any{}
	for k, v := range normalized {
		if mandatory[k] {
			out[k] = v
		}
	}
	var trim func(any, string, int) any
	trim = func(v any, path string, depth int) any {
		if depth > 8 {
			omitted[path] = "depth limit"
			return nil
		}
		switch x := v.(type) {
		case string:
			part, _, end := sliceUTF8(x, 0, 512)
			if end < len(x) {
				omitted[path] = map[string]any{"omitted_bytes": len(x) - end}
			}
			return part
		case []any:
			result := []any{}
			seen := map[string]bool{}
			for i, item := range x {
				// Identity includes provenance and state/content: equal text from different
				// sources, or changed evidence for one ID, must remain distinct.
				raw, _ := json.Marshal(item)
				key := string(raw)
				if m, ok := item.(map[string]any); ok && m["id"] != nil {
					key = fmt.Sprint(m["source_task_id"], "/", m["id"], "/", string(raw))
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				if len(result) >= 20 {
					omitted[path] = map[string]any{"omitted_items": len(x) - i}
					break
				}
				result = append(result, trim(item, fmt.Sprintf("%s[%d]", path, i), depth+1))
			}
			return result
		case map[string]any:
			result := map[string]any{}
			for k, item := range x {
				result[k] = trim(item, path+"."+k, depth+1)
			}
			return result
		default:
			return v
		}
	}
	keys := []string{}
	for k := range normalized {
		if !mandatory[k] {
			keys = append(keys, k)
		}
	}
	priority := map[string]int{"recent_facts": 0, "finding_list": 1, "running_intents": 2, "open_intents": 3, "recent_done_intents": 4, "related_tasks": 90}
	sort.Slice(keys, func(i, j int) bool {
		a, ok := priority[keys[i]]
		if !ok {
			a = 50
		}
		b, ok := priority[keys[j]]
		if !ok {
			b = 50
		}
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	out["omitted"] = omitted
	mandatoryBytes, _ := json.Marshal(out)
	if len(mandatoryBytes) > overviewBudget {
		out["mandatory_budget_exceeded"] = true
	}
	for _, k := range keys {
		out[k] = trim(normalized[k], k, 0)
		raw, _ := json.Marshal(out)
		if len(raw) > overviewBudget {
			delete(out, k)
			omitted[k] = "optional field omitted by byte budget; retrieve detail"
		}
	}
	// Omission metadata counts toward the soft projection budget too. Evict
	// the lowest-priority admitted optional key until metadata also fits.
	for i := len(keys) - 1; i >= 0; i-- {
		encoded, _ := json.Marshal(out)
		if len(encoded) <= overviewBudget {
			break
		}
		k := keys[i]
		if _, ok := out[k]; ok {
			delete(out, k)
			omitted[k] = "optional field omitted by byte budget; retrieve detail"
		}
	}

	return out
}

func workerContext(data map[string]any, intent *db.Node) map[string]any {
	return projectOverview(scopedWorkerContext(data, intent, false))
}

// The compact worker launch projection is independent of experimental planner,
// transcript-reference and trace-paging features. It is opt-in: it changes what a
// worker knows about other intents' results, so the default stays off until a
// live comparison supports changing it. Unset, empty or unrecognised values keep
// the legacy launch.
func compactWorkerLaunchEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_WORKER_CONTEXT"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// addWorkerDependencyEvidence loads, by id, the facts and findings the assigned
// intent depends on: its direct parents and everything it or a parent intent
// yielded. The overview's recent_facts/finding_list are task-wide newest-first
// windows, so on a busy task dependency evidence is often older than the window;
// without this step the compact launch would select from whatever happened to be
// recent. Entries use the overview's shape and are merged newest first.
func (t *ToolSet) addWorkerDependencyEvidence(data map[string]any, intentID int64) {
	unavailable := func() { data["dependency_evidence_unavailable"] = "lookup failed; use list_facts / list_findings" }
	own, err := t.ts.EdgesTouching([]int64{intentID})
	if err != nil {
		unavailable()
		return
	}
	producers := []int64{intentID}
	from := map[int64]int64{}
	wanted := map[int64]bool{}
	for _, e := range own {
		switch {
		case (e.Rel == db.RelDerivedFrom || e.Rel == db.RelSpawns) && e.To == intentID:
			producers = append(producers, e.From)
			wanted[e.From] = true
		case e.Rel == db.RelYields && e.From == intentID:
			wanted[e.To], from[e.To] = true, intentID
		}
	}
	if len(producers) > 1 {
		edges, err := t.ts.EdgesTouching(producers[1:])
		if err != nil {
			unavailable()
			return
		}
		parent := map[int64]bool{}
		for _, id := range producers[1:] {
			parent[id] = true
		}
		for _, e := range edges {
			if e.Rel == db.RelYields && parent[e.From] {
				wanted[e.To], from[e.To] = true, e.From
			}
		}
	}
	present := map[string]bool{}
	for _, key := range []string{"recent_facts", "finding_list"} {
		items, _ := data[key].([]map[string]any)
		for _, m := range items {
			present[fmt.Sprint(m["id"])] = true
		}
	}
	ids := []int64{}
	for id := range wanted {
		if !present[fmt.Sprint(id)] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	if len(ids) > workerDependencyCap {
		data["dependency_evidence_omitted"] = len(ids) - workerDependencyCap
		ids = ids[:workerDependencyCap]
	}
	nodes, err := t.ts.NodesByIDs(ids)
	if err != nil {
		unavailable()
		return
	}
	facts, _ := data["recent_facts"].([]map[string]any)
	findings, _ := data["finding_list"].([]map[string]any)
	for _, n := range nodes {
		var payload map[string]any
		_ = json.Unmarshal(n.Payload, &payload)
		switch n.Kind {
		case db.KindFact:
			m := compactNode(n)
			if c, ok := payload["confidence"].(string); ok && c != "" {
				m["confidence"] = c
			}
			if from[n.ID] > 0 {
				m["from_intent"] = from[n.ID]
			}
			facts = append(facts, m)
		case db.KindFinding:
			m := map[string]any{"id": n.ID, "summary": payload["summary"]}
			if from[n.ID] > 0 {
				m["from_intent"] = from[n.ID]
			}
			findings = append(findings, m)
		}
	}
	newest := func(items []map[string]any) {
		sort.SliceStable(items, func(i, j int) bool {
			a, _ := items[i]["id"].(int64)
			b, _ := items[j]["id"].(int64)
			return a > b
		})
	}
	newest(facts)
	newest(findings)
	data["recent_facts"], data["finding_list"] = facts, findings
}

func workerLaunchContext(data map[string]any, intent *db.Node) map[string]any {
	out := scopedWorkerContext(data, intent, true)
	// The complete assigned payload is pinned in renderIntentTask's system tail.
	// Keep lineage here without paying for the same summary in the launch turn.
	if assignment, ok := out["intent_context"].(map[string]any); ok {
		delete(assignment, "summary")
		assignment["summary_in_system_intent"] = true
	}
	for _, key := range []string{"recent_facts", "finding_list", "awareness_recent_facts", "awareness_finding_list"} {
		items, _ := out[key].([]any)
		for _, item := range items {
			m, _ := item.(map[string]any)
			text, ok := m["summary"].(string)
			if !ok {
				continue
			}
			part, _, end := sliceUTF8(text, 0, 384)
			for raw, _ := json.Marshal(part); len(raw) > 384; raw, _ = json.Marshal(part) {
				part, _, end = sliceUTF8(text, 0, len(part)/2)
			}
			if end < len(text) {
				m["summary"] = part
				m["summary_truncated"] = true
				m["summary_omitted_bytes"] = len(text) - end
				m["requires_node_detail"] = true
			}
		}
		if len(items) > 20 {
			out[key] = items[:20]
			out[key+"_omitted_items"] = len(items) - 20
		}
	}
	out["retrieval"] = "node_detail(id) for complete evidence; list_assets(ids) for targets; partial summaries and omissions are not negative evidence"
	return projectOverview(out)
}

func scopedWorkerContext(data map[string]any, intent *db.Node, awareness bool) map[string]any {
	raw, _ := json.Marshal(data)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	out := map[string]any{}
	_ = dec.Decode(&out)
	delete(out, "coverage") // worker render omits coverage; do not spend its budget
	deps := map[string]bool{fmt.Sprint(intent.ID): true}
	for _, key := range []string{"open_intents", "running_intents", "recent_done_intents"} {
		items, _ := out[key].([]any)
		for _, v := range items {
			m, _ := v.(map[string]any)
			if fmt.Sprint(m["id"]) == fmt.Sprint(intent.ID) {
				out["intent_context"] = m
				if ps, ok := m["parents"].([]any); ok {
					for _, id := range ps {
						deps[fmt.Sprint(id)] = true
					}
				}
			}
		}
		delete(out, key)
	}
	for _, key := range []string{"recent_facts", "finding_list"} {
		items, _ := out[key].([]any)
		selected := []any{}
		other := []any{}
		filtered := 0
		// Findings are the task's highest-value output and the source window is
		// small (newest ten), so every unrelated one stays visible: a worker that
		// cannot see a finding may report it again. Facts keep three samples.
		awarenessCap := 3
		if key == "finding_list" {
			awarenessCap = len(items)
		}
		for _, v := range items {
			m, _ := v.(map[string]any)
			if deps[fmt.Sprint(m["from_intent"])] || deps[fmt.Sprint(m["id"])] {
				selected = append(selected, v)
			} else {
				filtered++
				if awareness && len(other) < awarenessCap {
					other = append(other, v)
				}
			}
		}
		out[key] = selected
		out[key+"_filtered_unrelated"] = filtered
		if awareness {
			out["awareness_"+key] = other
			out["awareness_"+key+"_total"] = filtered
		}
	}
	delete(out, "cold_digests")
	out["intent_id"] = intent.ID
	out["scope_required_before_action"] = true
	out["retrieval"] = "node_detail(id) for complete evidence; list_assets(ids) for targets; partial summaries and omissions are not negative evidence"
	return out
}

func projectAssets(assets []*db.Asset) map[string]any {
	out := []any{}
	result := map[string]any{"assets": out, "omitted_assets": len(assets), "omitted_fields": []string{"page_title", "headers", "body", "metadata"}, "detail_tool": "list_assets(ids); omitted fields are not absent evidence"}
	for _, a := range assets[:min(len(assets), 8)] {
		m := map[string]any{"id": a.ID, "type": a.Type, "port": a.Port, "status_code": a.StatusCode}
		for k, v := range map[string]string{"url": a.URL, "domain": a.Domain, "ip": a.IP} {
			if len(v) <= 1024 {
				m[k] = v
			} else {
				m["requires_asset_detail"] = true
				m[k+"_omitted"] = true
			}
		}
		out = append(out, m)
		result["assets"] = out
		result["omitted_assets"] = len(assets) - len(out)
		encoded, _ := json.Marshal(result)
		if len(encoded) > workerAssetBudget {
			out = out[:len(out)-1]
			result["assets"] = out
			result["omitted_assets"] = len(assets) - len(out)
			break
		}
	}
	return result
}
