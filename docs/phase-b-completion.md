# Phase B completion: durable replay and evidence references

Implementation follow-up for issues [#17](https://github.com/mkilijanek/ARTEX/issues/17)
and [#20](https://github.com/mkilijanek/ARTEX/issues/20). Both mechanisms use the
existing `ARTEX_BOUNDED_CONTEXT=1` opt-in. Default remains off. Stored graph data,
evidence, permissions, raw tool output, activity and SDK transcripts are unchanged.

## Planner checkpoint and fresh-session replay (#17)

`db/planner_context.go` stores one checkpoint per exploration in
`planner_context_checkpoints`, with an exploration foreign key and delete cascade.
An exploration-row `FOR NO KEY UPDATE` lock serializes checkpoint writers across processes
without blocking the key-share locks taken by child-node inserts,
including first creation. A failed callback/write does not consume a version.
The checkpoint is disposable trusted read-model state; its checksum is not authentication or password hashing. Task credentials are excluded from it and rendered live, losslessly, each round; old bases never retain old passwords. Task archives need not copy it.
Restore without a checkpoint rebuilds it from the restored graph.

`agent/planner_context.go` writes protocol version, epoch, immutable base/version,
ordered whole-key replacements/removals, current version and CRC64 accidental-corruption checksum.
The server validates and materializes the replay. Fresh Planner/Norma instances
receive only the exact current projected snapshot, never the base/delta chain or
an instruction to reconstruct it. Corruption, missing versions or invalid
checksum cause a new epoch and full snapshot. A checkpoint database failure
falls back to a one-off current snapshot; cancellation still stops the round.
JSONB ordering is canonicalized with json.Number to keep large IDs exact.

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

The first full value stays unchanged. Later occurrences use `$ref`,
`context_status=previous` and `at`. The natural key names the entity/source/field
and SHA256 version; `at` gives its earlier host-history location and JSON pointer.
These labels describe repeated representation, not real-world validity.
Interpretation rules are fixed from the first request, before any duplicate is
eligible. Eligibility depends only on that field and its earlier occurrence, so
new duplicates do not rewrite previously sent messages or system prefixes.
References and their source values remain untrusted data.

Reserved reference-shaped syntax inside evidence is quoted deterministically as
`literal_evidence_json` from its first appearance. Its contents are exact serialized
source data, never nested reference protocol. These ambiguous values are not
deduplicated. Quoting preserves raw stored evidence and may add transport bytes.

The ledger is rebuilt on every request from currently visible data. If compaction
removes the earlier definition, the next occurrence is full again. References
never enter the mutable SDK history/transcript, so resume and further compaction
retain original evidence. SDK transcript summaries receive original data, not
wire references. Short values remain full when references would cost more,
including the second escaping layer inside ContentBlock.Text. The fixed protocol
rule has an upfront cost even on the first request with no duplicates. It stays
fixed to protect cached prefixes; no future-dependent whole-request decision can
retroactively change an earlier message. Logs distinguish this overhead from the
field replacement savings. No cache hit/cost saving is assumed.

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
After-ref counts include JSON escaping and fixed interpretation rules. Before-ref
counts omit those rules, so the comparison includes the protocol's upfront cost.
Checkpoint bytes below are **storage only**, never sent to the model.

| Scenario / round | Snapshot sent bytes | Stored journal bytes | Latest stored update | Request before protocol | Request after protocol |
| --- | ---: | ---: | ---: | ---: | ---: |
| Steady / 1 | 5523 | 5697 | 5697 | 17327 | 13911 |
| Steady / 4 | 5523 | 5787 | 38 | 17327 | 13911 |
| Count changes / 4 | 5523 | 5817 | 48 | 17327 | 13911 |
| Evidence state changes / 3 | 5522 | 11046 | 5336 | 17324 | 13908 |

References reduced these complete repeated-read fixture requests by about 19.7%,
including a 630-byte fixed protocol cost. A prefetch-only request adds that cost
without replacement savings. Stored replay can grow after changes, but the sent
snapshot does not grow with it. No user-message cache breakpoint or model-side
patch application is needed. Tests compare every sent snapshot with the exact
current projection and compare earlier request prefixes after new duplicates.
Provider cache reads, live-model cost/quality and default-on acceptance remain
unmeasured; smaller bytes alone do not establish lower billed cost.

Logs report source/packet/update bytes, estimator-labelled `ceil(UTF8 bytes/4)`
tokens and, separately, provider-reported input/output/cache tokens at the end of
each Planner round. Request dedup logs distinguish original bytes, fixed protocol
overhead, before/after replacement bytes and quoted literal fields, and
label estimates the same way. Physical usage/cost attribution remains in the
existing Phase A ledger. Fixtures test reported-usage plumbing with a synthetic
123-token provider value; that value is not a live-model measurement.

Representative live-model quality/cost comparisons, default-on rollout and
production rollback acceptance remain tracked in Phase E/F and #1/#2/#33. They
are not prerequisites for exercising the opt-in transport and are not marked done
by this implementation.
