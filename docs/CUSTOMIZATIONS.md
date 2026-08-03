# ResearchFlow Customizations

This ledger records every ResearchFlow-specific divergence from Tencent/WeKnora upstream on the `researchflow-ext` branch.

## Upstream baseline

- Synchronized on: 2026-08-02
- Upstream remote: `https://github.com/Tencent/WeKnora.git`
- Baseline commit: `5780affdfc76342ddd0f5cf95b548a1a4d0b2a5a`
- Baseline describe: `v0.7.1-115-g5780affd`

## Branch discipline

- `main` only mirrors `upstream/main`; no ResearchFlow-specific commit belongs on `main`.
- Synchronize `main` with `git fetch upstream` followed by `git merge --ff-only upstream/main`.
- Before each W-track substage, synchronize `main`, then merge the synchronized `main` into `researchflow-ext`.
- Keep extensions on connector, parser, provider, or DTO plugin surfaces whenever possible.
- Before changing a core pipeline, add the proposed divergence to this ledger and record in a ResearchFlow ADR why the plugin surface is insufficient.
- Evaluate every customization for contribution to upstream and record expected resynchronization impact.

## Entry requirements

Each entry must record its W-track stage, scope, reason, changed files, tests, upstream contribution suitability, and expected impact when resynchronizing with upstream.

## Customization entries

| ID | Stage | Scope | Reason | Files | Tests | Upstream contribution | Resynchronization impact |
|---|---|---|---|---|---|---|---|
| W1-001 | W1 | test-only (`chunker`) | ResearchFlow re-reads every hit at its own range before it may be cited, so it depends on one chunker invariant: `0 <= Start < End <= runeLen(text)` and `Content` ends with `text[Start:End]`. `mergeUnits` prepends a table header outside the range on purpose, so equality is not the contract and the weaker suffix form is what callers can rely on. Pinned across six document shapes × five strategies so a chunker change cannot silently move offsets away from the content they address. | `internal/infrastructure/chunker/chunk_span_contract_test.go` | `go test ./internal/infrastructure/chunker/ -run TestChunkSpanContract` | Suitable — no ResearchFlow concept appears; it states an invariant the package's own doc comment already claims. | Low — test-only, no production symbol touched. Fails loudly if upstream changes offset semantics, which is the point. |
| W1-002 | W1 | test-only (`service`) | `buildSearchResult` is the single crossing where a chunk's range and its knowledge's ingest metadata enter the API shape. Governed retrieval cannot verify a quote without both surviving verbatim, so the passthrough is pinned rather than the wiring. Also pins that synthesized chunks (summary/FAQ/table-summary) keep their empty `[0,0)` range: a consumer can only refuse what it can see, and an empty range widened to the body would read as an anchor. | `internal/application/service/knowledgebase_search_results_span_test.go` | `go test ./internal/application/service/ -run TestBuildSearchResult` | Suitable — DTO passthrough test with no ResearchFlow-specific keys asserted by name beyond generic metadata. | Low — test-only. |
| W1-003 | W1 | test-only (`chat_pipeline`) | Regression lock for the reason governed retrieval must not use `/knowledge-search`: `mergeSequentialChunks` joins neighbouring bodies and deliberately leaves `StartAt`/`EndAt` where they were, so a merged result can no longer be read back at its own range. Turning that documented decision into an executable assertion keeps it from being quietly reverted, and makes the endpoint choice in ADR-0010 verifiable rather than asserted. | `internal/application/service/chat_pipeline/merge_overlap_span_test.go` | `go test ./internal/application/service/chat_pipeline/ -run TestMergeSequentialChunks_LeavesTheRange` | Suitable — documents existing intended behaviour. | Low — test-only. If merging ever starts maintaining the range this test fails, which is the signal to revisit the endpoint restriction. |

## W1 findings carried into ResearchFlow ADR-0010

Measured on this baseline, test-only, no fork production change proposed:

- **Parent-child splitting loses the range on documents with tables.** `SplitParentChild` re-splits each parent's own `Content` and shifts child offsets by `parent.Start`; that arithmetic is sound only while `parent.Content == text[parent.Start:parent.End]`, which the prepended table header violates. At parent=160/child=60: `single_table` 5 of 10 children unreadable at their range (1 prefixed parent), `two_tables` 5 of 14 (1 prefixed parent), `headings_and_table` 7 of 10 (2 prefixed parents); prose and protected-span corpora 0 broken. Parents themselves always satisfy the contract. Governed knowledge bases must therefore pin `chunking_config.enable_parent_child = false`. Fail-closed on the ResearchFlow side (`locator_mismatch`), so it costs recall on tables, never correctness.
- **Image resolution rewrites markdown before chunking.** `knowledge_process.go` runs `ResolveAndStore` / `ResolveRemoteImages` ahead of splitting, so offsets address the rewritten body when a locally promoted `.md` carries resolvable images. Fail-closed; revisited at W3.
- **Query text reaches logs and langfuse spans on the search path.** Not on the certified `hybrid-search` route's chat pipeline, but real integration must still disable query logging and telemetry before any governed knowledge base is pointed at a live deployment.
