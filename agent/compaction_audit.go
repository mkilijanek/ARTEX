package agent

import (
	"encoding/json"
	"log"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// CompactionAudit is a content-free lifetime snapshot of one Compactor. Usage is
// reported response usage, not a second billing ledger or an inferred cost.
// ZeroUsageResponses means unavailable/zero counters, never proof of free work.
type CompactionAudit struct {
	MaintenanceRounds      int64 `json:"maintenance_rounds"`
	MaintenanceErrors      int64 `json:"maintenance_errors"`
	SuppressedRuns         int64 `json:"suppressed_runs"`
	MinorRuns              int64 `json:"minor_runs"`
	MajorRuns              int64 `json:"major_runs"`
	CandidateBlocks        int64 `json:"candidate_blocks"`
	CandidateMembers       int64 `json:"candidate_members"`
	SignatureReuses        int64 `json:"signature_reuses"`
	SignatureReusedMembers int64 `json:"signature_reused_members"`
	ProviderInvocations    int   `json:"provider_invocations"`
	MemoReturns            int   `json:"memo_returns"`
	CacheHits              int64 `json:"cache_hits"`
	JoinedFlights          int64 `json:"joined_flights"`
	SharedReturns          int64 `json:"shared_returns"`
	ProviderErrors         int64 `json:"provider_errors"`
	RejectedSummaries      int64 `json:"rejected_summaries"`
	TruncatedResponses     int64 `json:"truncated_responses"`
	ZeroUsageResponses     int64 `json:"zero_usage_responses"`
	InputBytes             int64 `json:"input_bytes"`
	OutputBytes            int64 `json:"output_bytes"`
	InputTokens            int64 `json:"input_tokens"`
	OutputTokens           int64 `json:"output_tokens"`
	CacheReadTokens        int64 `json:"cache_read_tokens"`
	CacheWriteTokens       int64 `json:"cache_write_tokens"`
	InferenceMillis        int64 `json:"inference_ms"`
}

func (c *Compactor) AuditStats() CompactionAudit {
	if c == nil {
		return CompactionAudit{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.audit
	out.ProviderInvocations = c.summaryCalls
	out.MemoReturns = c.summaryMemoHits
	return out
}
func (c *Compactor) updateAudit(f func(*CompactionAudit)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.audit)
}
func (c *Compactor) auditBlocks(blocks []block) {
	c.updateAudit(func(a *CompactionAudit) {
		a.CandidateBlocks += int64(len(blocks))
		for _, b := range blocks {
			a.CandidateMembers += int64(len(b.Members))
		}
	})
}
func (c *Compactor) auditInference(req llm.CompletionRequest, msg llm.Message, stop string, usage llm.Usage, err error, elapsed time.Duration) {
	raw, _ := json.Marshal(req)
	c.updateAudit(func(a *CompactionAudit) {
		a.InputBytes += int64(len(raw))
		a.OutputBytes += int64(len(msg.Text()))
		a.InferenceMillis += elapsed.Milliseconds()
		a.InputTokens += int64(usage.InputTokens)
		a.OutputTokens += int64(usage.OutputTokens)
		a.CacheReadTokens += int64(usage.CacheReadTokens)
		a.CacheWriteTokens += int64(usage.CacheWriteTokens)
		if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheWriteTokens == 0 {
			a.ZeroUsageResponses++
		}
		if err != nil {
			a.ProviderErrors++
		}
		if stop == "max_tokens" || stop == "length" {
			a.TruncatedResponses++
		}
	})
}
func (c *Compactor) logAudit(exploration int64) {
	raw, _ := json.Marshal(c.AuditStats())
	log.Printf("[compaction_audit] exploration=%d scope=compactor_lifetime counters=%s", exploration, raw)
}
