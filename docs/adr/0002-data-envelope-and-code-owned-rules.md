# ADR 0002: Data envelope and code-owned untrusted-content rules

- **Status:** Accepted (Phase D.1, PR 2, branch `phase-d/data-envelope-provenance`)
- **Date:** 2026-11-18
- **Issues:** #23 (Phase D.1), #7 (agentic attack-class audit)
- **Plan:** [phase-d1-plan.md](../phase-d1-plan.md), design D3 to D5 and section 10b

## Context

Host-built messages splice target-derived text (worker output, findings, facts,
digests, related tasks, hints, asset JSON) into the planner round, the worker launch
and the cold-digest compressor request. Nothing marked where that text began or ended,
so a target could write text that reads as instructions to the next agent.

The rules telling agents to treat such text as data lived in the editable prompt
seeds. A DB prompt override or an operator edit could drop them.

## Decision

1. **Nonce frames.** Derived content is wrapped as
   `[[ARTEX_DATA nonce=N field=<name>]]` … `[[END_ARTEX_DATA nonce=N]]`
   (`agent/data_envelope.go`). One random nonce is drawn per planner message, per worker
   launch and per compressor request; every frame in that message shares it. Any
   occurrence of the marker stem inside the data is rewritten
   (`dataMarkerNeutraliser`), so data cannot open or close a frame whatever nonce it
   guesses. The nonce is never logged: the neutralisation log line is content-free.
2. **Default on, opt-out.** `ARTEX_DATA_ENVELOPE=0` (or `false`/`off`/`no`) switches off
   the frames and the planner goal anchor only. No envelope is built and no nonce drawn;
   the pre-change renderers are kept as the off path.
3. **Code-owned rules.** `untrustedDataRules` (planner, compactor) and
   `untrustedContentInstructions` (worker, main, auto, pentest, chat) are appended by
   code after the rendered body (`agent/untrusted_prompt.go`), exactly once, whatever the
   prompt override; a DB row that still carries the old seeded block gets no second copy.
   With `ARTEX_CACHE_PREFIX=1` the block goes before the body's volatile part so it stays
   in the cached prefix. These rules are unconditional.
4. **Per-part budgets.** The journal snapshot is bounded by `plannerPacketBudget`; the
   derived frames are projected to their own budget (`overviewBudget`) plus a fixed
   framing overhead. That budget is soft: mandatory keys such as `agent_hints` and
   `task_credentials` are copied untrimmed. Trigger frames sit outside both.
5. **Fact labels.** `origin` and `source` on facts are unconditional and serve
   labelling and display only; they do not gate `prove_goal` or any other decision.
6. **Hints split on origin.** Non-`human` hints are framed as data; `human` hints stay
   outside the frames. `human` origin is not proof of operator authorship (the main
   agent also writes with origin `human`).

## Alternatives rejected

- **XML tags.** Target text can contain the same tags; no per-message secret.
- **Framing the intent summary (C15).** It is the worker's assignment; framing it as
  data would tell the worker to ignore its task.
- **A static delimiter without a nonce.** A target can forge the closing line.
- **The nonce in the planner system prompt.** It would change the cached prefix every
  round; the nonce is named in the user message's intro instead.
- **Rules in the editable seeds.** An override or edit could drop them.
- **One combined budget.** Large derived parts would crowd out the journal snapshot.

## Consequences

- The envelope is advisory: it relies on the model honouring the rules. There is no
  live-model evaluation.
- The worker system prompt varies per launch, because the `assets` frame in it carries
  the launch nonce. A resumed worker draws a new nonce.
- The opt-out input is not the pre-PR input: rules and fact labels remain.
- The planner system prompt is 13478 B against its 13500 B test ceiling.

### Residuals (not addressed by this decision)

1. Main-agent hints and goal nodes (`set_goals`) keep origin `human` and stay unframed
   (C14). A goal is the stronger case: it defines done for the planner and the goal anchor.
2. Intent summaries and `stagnation_warning.directions` are not framed (C15).
3. The `finding` trigger frame has no size cap.
4. The declared fact `source` is spoofable.

## Related

- [SECURITY.md](../../SECURITY.md)
- [agentic-attack-audit.md](../agentic-attack-audit.md)
- [ADR 0001](0001-webfetch-destination-policy.md)
