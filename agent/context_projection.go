package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	var trim func(any, int) any
	trim = func(v any, depth int) any {
		if depth > 6 {
			return map[string]any{"detail_required": true}
		}
		switch x := v.(type) {
		case string:
			part, _, _ := sliceUTF8(x, 0, 512)
			return part
		case []any:
			out := []any{}
			seen := map[string]bool{}
			for _, item := range x {
				if len(out) >= 20 {
					break
				}
				b, _ := json.Marshal(item)
				key := string(b)
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, trim(item, depth+1))
			}
			return out
		case map[string]any:
			out := map[string]any{}
			keys := []string{}
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for i, k := range keys {
				if i >= 50 {
					break
				}
				out[k] = trim(x[k], depth+1)
			}
			return out
		default:
			return v
		}
	}
	out := map[string]any{"projection": true, "detail_required": true, "retrieval": "graph_overview, node_detail(id), list_facts, list_assets; omitted evidence is not negative evidence; obtain full scope before acting"}
	keys := []string{"task", "goals", "hints", "scope", "frontier_open", "running_intents", "open_intents", "recent_facts", "finding_list", "recent_done_intents", "cold_digests", "facts", "findings_total", "done_intents_total"}
	for _, k := range keys {
		v, ok := normalized[k]
		if !ok {
			continue
		}
		out[k] = trim(v, 0)
		b, _ := json.Marshal(out)
		if len(b) > overviewBudget {
			delete(out, k)
		}
	}
	return out
}

func workerContext(data map[string]any, intent *db.Node) map[string]any {
	out := projectOverview(data)
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
			if deps[fmt.Sprint(m["id"])] || fmt.Sprint(m["from_intent"]) == fmt.Sprint(intent.ID) {
				selected = append(selected, v)
			}
		}
		out[key] = selected
	}
	delete(out, "cold_digests")
	out["intent_id"] = intent.ID
	out["scope_required_before_action"] = true
	return out
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
			}
		}
		out = append(out, m)
	}
	return map[string]any{"assets": out, "omitted_assets": max(0, len(assets)-8), "detail_tool": "list_assets(ids); omitted fields are not absent evidence"}
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
	reset := func() { b.base = current; b.baseVersion = b.version }
	if b.base == nil || b.version-b.baseVersion >= 8 {
		reset()
	}
	replace := map[string]any{}
	removed := []string{}
	for k, v := range current {
		a, _ := json.Marshal(v)
		old, _ := json.Marshal(b.base[k])
		if !bytes.Equal(a, old) {
			replace[k] = v
		}
	}
	for k := range b.base {
		if _, ok := current[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(removed)
	p := map[string]any{"epoch": b.epoch, "version": b.version, "base_version": b.baseVersion, "base": b.base, "replace": replace, "remove": removed, "self_contained": true}
	raw, _ := json.Marshal(p)
	if len(raw) > plannerPacketBudget {
		reset()
		p["base"] = current
		p["base_version"] = b.baseVersion
		p["replace"] = map[string]any{}
		p["remove"] = []string{}
	}
	return p
}
