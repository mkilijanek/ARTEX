# Phase B completion: durable replay and evidence references

Implementation follow-up for issues [#17](https://github.com/mkilijanek/ARTEX/issues/17)
and [#20](https://github.com/mkilijanek/ARTEX/issues/20). Both mechanisms use the
existing `ARTEX_BOUNDED_CONTEXT=1` opt-in. Default remains off. Stored graph data,
evidence, permissions, raw tool output, activity and SDK transcripts are unchanged.

## Planner checkpoint and fresh-session replay (#17)

`db/planner_context.go` stores one checkpoint per exploration in
`planner_context_checkpoints`, with an exploration foreign key and delete cascade.
An exploration-row lock serializes callback/write transactions across processes,
including first creation. A failed callback/write does not consume a version.
The checkpoint is disposable read-model state; task archives need not copy it.
Restore without a checkpoint rebuilds it from the restored graph.

`agent/planner_context.go` writes protocol version, epoch, immutable base/version,
ordered whole-key replacements/removals, current version and SHA256 checksum.
Fresh Planner/Norma instances receive the full replay, never a standalone delta
and never a dependency on an old model response. Each replay is validated before
advance. Corruption, missing versions or invalid checksum cause a new epoch and
full snapshot of the current graph. A database failure stops the planning round
before model execution. JSONB ordering is canonicalized with json.Number to keep
large IDs exact and the base prefix stable across restarts.

At eight updates after the base, a new base replaces the old chain. Replay over
40 KiB also resyncs immediately. Optional data already passes the existing 16 KiB
projection. Mandatory goals/hints/coverage/credentials remain lossless; one large
mandatory snapshot may exceed the soft limit, explicitly marked. History growth
is bounded independently of how many rounds the task has run. On-demand detail
tools and omission notices remain available. A remove operation changes the
view; it never proves an observation false.

## Prefetch/tool/history deduplication (#20)

`agent/context_dedup.go` transforms a copy of each actual post-compaction API
request. Code registers the exact Planner/Worker prefetch fragment for the run.
Known graph read results (`graph_overview`, `list_facts`, `list_findings`,
`node_detail`, `expand_digest`) are recognized through preceding tool-use IDs.
Unknown tools, errors, caller prose, arbitrary embedded JSON and assistant
thinking/redacted/tool signatures are left intact.

Within recognized structural slots, an entity field's identity contains kind,
source namespace, node ID, field, state, confidence and exact canonical value.
Different IDs/sources/states/confidence/content never share a version. Related
task records keep explicit source_task_id/inherited provenance. A summary cannot
replace the distinct full payload of node_detail. Other fields, ownership,
confidence, lineage and pagination metadata stay at each occurrence.

The first repeated value receives `$id`, `value`, `context_status=new`; later
occurrences use `$ref`, `context_status=previous`. The key names the entity/source,
field and SHA256 version. These labels describe representation within this
request, not a new observation or a real-world validity judgment. The model gets
a code-owned reference interpretation rule; referenced values remain untrusted
data. Expand references before applying journal replacements/removals.

The ledger is rebuilt on every request from currently visible data. If compaction
removes the earlier definition, the next occurrence is full again. References
never enter the mutable SDK history/transcript, so resume and further compaction
retain original evidence. SDK transcript summaries receive original data, not
wire references. Short values remain full when references would cost more. The
entire transformation is reverted unless the complete encoded request, including
reference rules, definitions and metadata, is smaller.

## Validation and measured limits

Focused PostgreSQL and race tests cover concurrent writers, fresh DB handles,
large numeric IDs, checksum recovery, rollback, removals, periodic/size resync,
mandatory overflow, fresh Planner sessions and provider-reported usage.
Dedup tests expand wire references and reconstruct exact original JSON, retain
changed evidence and full detail, preserve pagination/signatures, remove dangling
refs after compaction, and run real Norma tool calls plus transcript resume.

Reproduce deterministic measurements:

```sh
go test ./agent -run TestPhaseBContextMeasurements -count=1 -v
```

Fixture: 12 identified observations; two repeated list_facts reads; four rounds.
All request byte counts include JSON escaping and reference interpretation rules.

| Scenario / round | Projected snapshot bytes | Journal replay bytes | Latest update bytes | Request before refs | Request after refs |
| --- | ---: | ---: | ---: | ---: | ---: |
| Steady / 1 | 5523 | 5741 | 5741 | 17710 | 13810 |
| Steady / 4 | 5523 | 5831 | 38 | 17814 | 13914 |
| Count changes / 4 | 5523 | 5861 | 48 | 17850 | 13950 |
| Evidence state changes / 3 | 5522 | 11090 | 5336 | 23187 | 16394 |

References reduced these complete fixture requests by about 22–29%, relative to
the same journal request without references. This is **not** a comparison against
the former single-snapshot transport. Replay can be larger than a current
snapshot, especially when an array is replaced. The stable immutable base makes
prefix reuse possible, but provider caching/cost/quality must be measured rather
than assumed. No live-model saving or default-on acceptance is claimed here.

Logs report source/packet/update bytes, estimator-labelled `ceil(UTF8 bytes/4)`
tokens and, separately, provider-reported input/output/cache tokens at the end of
each Planner round. Request dedup logs count full before/after request bytes and
label estimates the same way. Physical usage/cost attribution remains in the
existing Phase A ledger. Fixtures test reported-usage plumbing with a synthetic
123-token provider value; that value is not a live-model measurement.

Representative live-model quality/cost comparisons, default-on rollout and
production rollback acceptance remain tracked in Phase E/F and #1/#2/#33. They
are not prerequisites for exercising the opt-in transport and are not marked done
by this implementation.
