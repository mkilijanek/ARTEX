package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/google/uuid"
)

const overviewBudget = 16 * 1024
const plannerPacketBudget = 40 * 1024

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
		"related_tasks": true, "task_credentials": true, "task_credentials_note": true}
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
	sort.Strings(keys)
	out["omitted"] = omitted
	for _, k := range keys {
		out[k] = trim(normalized[k], k, 0)
		raw, _ := json.Marshal(out)
		if len(raw) > overviewBudget {
			delete(out, k)
			omitted[k] = "optional field omitted by byte budget; retrieve detail"
		}
	}
	raw, _ = json.Marshal(out)
	if len(raw) > overviewBudget {
		out["mandatory_budget_exceeded"] = true
	}

	return out
}

func workerContext(data map[string]any, intent *db.Node) map[string]any {
	raw, _ := json.Marshal(data)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	out := map[string]any{}
	_ = dec.Decode(&out)
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
		for _, v := range items {
			m, _ := v.(map[string]any)
			if deps[fmt.Sprint(m["from_intent"])] || deps[fmt.Sprint(m["id"])] {
				selected = append(selected, v)
			}
		}
		out[key] = selected
	}
	delete(out, "cold_digests")
	out["intent_id"] = intent.ID
	out["scope_required_before_action"] = true
	return projectOverview(out)
}

func projectAssets(assets []*db.Asset) map[string]any {
	out := []any{}
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
	}
	return map[string]any{"assets": out, "omitted_assets": max(0, len(assets)-8), "omitted_fields": []string{"page_title", "headers", "body", "metadata"}, "detail_tool": "list_assets(ids); omitted fields are not absent evidence"}
}

type overviewBase struct {
	epoch                string
	version, baseVersion int
	base                 map[string]any
}
type overviewSnapshots struct {
	mu    sync.Mutex
	tasks map[int64]*overviewBase
}

// Every packet contains its base and cumulative replacements, so a new planner
// session never relies on remembered deltas. Restart/eviction starts a new epoch.
func (s *overviewSnapshots) packet(task int64, data map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := projectOverview(data)
	if s.tasks == nil || len(s.tasks) > 64 {
		s.tasks = map[int64]*overviewBase{}
	}
	b := s.tasks[task]
	if b == nil {
		b = &overviewBase{epoch: uuid.NewString()}
		s.tasks[task] = b
	}
	b.version++

	// Each planner call starts a new SDK session. Sending the previous base plus
	// replacements costs more than sending current state and cannot save tokens.
	b.base = current
	b.baseVersion = b.version
	p := map[string]any{"epoch": b.epoch, "version": b.version, "base_version": b.version,
		"base": current, "replace": map[string]any{}, "remove": []string{}, "self_contained": true}
	before, _ := json.Marshal(data)
	after, _ := json.Marshal(p)
	log.Printf("[context] planner task=%d round=%d source_bytes=%d packet_bytes=%d", task, b.version, len(before), len(after))
	return p
}
