# broker-acceptance-cleanup

Owner: Daniel. Integrator / installer / live verifier: Apex.
Worktree: `/Users/danieliser/Toolkit/capability-broker-wt/acceptance-cleanup`
Branch: `feat/acceptance-cleanup-20261103`, based on capability-broker main `e4eb789`.
Input: `QUEUE.md` → `## Reconciled 2026-11-03`, especially C operation constraints, full steward broker path, and smoke residue.

## Disposition

1. **Done — operation map and declarative constraints.** All 17 operations in the three embedded connector YAMLs are mapped in `docs/operation-constraints.md`. Added `allowlist` (to/recipient and any string/list path), `prefix` (subject templates), `maximum` (amount ceilings), `row_limit`, and durable `max_per_day`. Connector and operation values are conjunctive; child grants retain parent restrictions. Preparation now precedes final argument checks and intent receipts; receipts digest effective provider arguments. Daily limits survive grant rotation and SQLite close/reopen. Added missing WordPress fan-out site limits, memory-update namespace/importance limits, ClickHouse result/deadline enforcement, and tests for malformed values, zero/negative/fractional ceilings, concurrent admission, UTC rollover, inheritance, and post-enrichment denial.
2. **Done — full local steward-shaped acceptance.** Real broker HTTP front and upstream adapter with local MCP fixtures: named-query read → broker `/mcp/memory` store of the observed result → scoped recall. Verified namespace/importance enrichment, bound params, exclusion of foreign records, effective digests, memory ID, and eight persisted receipts: three intent/outcome pairs plus two denials. This is fixture qualification, not an installed daemon, real provider, PERSIST wake, or customer outcome claim.
3. **Done — exact live read inventory and safe cleanup command.** `recall_memory` exact tag enumeration at **2026-11-03 10:27:03 UTC** returned **one** record, page 0 / limit 200 / `has_more=false`. Locally intersected with steward namespace. Exact ID: **`671c092f-9496-483a-a60e-4a3d3c792053`**. Both tags: `broker-smoke-test`, `agent:companykit.data-sync-steward`. API evidence and original content/tags/hash are preserved under `docs/evidence/`. Cleanup CLI is read-only by default, requires an explicit execute flag and reviewed manifest for deletion, validates every ID's tags/content before any delete, deletes only by ID, and verifies absence by fresh enumeration. Built CLI exercised through a local broker in inventory, dry-run-manifest, and explicit fixture-delete modes. **No live memories deleted.**

No requested deliverable was skipped. Live install, overlay/config edits, scheduled steward wake, real-provider write, and live smoke deletion were intentionally excluded by the brief and remain Apex-owned.

## Commits

- `972910a` — `feat(policy): enforce declarative operation constraints`
- `3600325` — `feat(memory): add reviewed smoke cleanup dry runs`
- `ef9bbb2` — `test(broker): prove steward read and memory write receipts`
- Final documentation/handoff commit follows these three (`docs(broker): document acceptance and integration handoff`). Every commit has a GPT-6 Codex co-author trailer. No push or merge.

## Files

- Policy/catalog: `pkg/catalog/constraints.go`, `catalog.go`, all `pkg/catalog/connectors/*.yaml`, `pkg/policy/arguments.go`, `policy.go`, and focused tests.
- Grants/storage: `pkg/auth/capabilities.go`, `grants.go`, tests, `pkg/durable/sqlite.go`.
- Call path: `pkg/adapter/adapter.go`, `pkg/adapters/upstream/upstream.go`, adapter tests, `pkg/mcpfront/front.go`.
- Acceptance: `test/e2e/steward_test.go`, `cleanup_test.go`, `e2e_test.go` (built CLI fixture support and receipt-writer cleanup).
- Cleanup: `cmd/brokerd-memory-cleanup/main.go`, `pkg/memorycleanup/cleanup.go`, tests.
- Docs: `README.md`, `CHANGELOG.md` [Unreleased], `docs/reference.md`, `docs/operation-constraints.md`, evidence files, this handoff.

## Validation

Runtime: `go1.26.5 darwin/arm64`. All temporary build/test files were directed into ignored `output/tmp` for the final gates.

- `go build ./...` — PASS.
- `go test ./...` — PASS, including broker e2e, declarative constraints, access controls, amount limits, daily persistence/concurrency, and destructive cleanup fixture tests.
- `go vet ./...` — PASS.
- `go test -race ./pkg/mcpfront ./pkg/auth ./pkg/policy ./pkg/memorycleanup` — PASS.
- `go test ./test/e2e -run '^Test(CleanupCommandDefaultsToDryRun|StewardBrokerAcceptance)$' -v -count=1` — PASS. CLI default modes made zero fixture writes; explicit execute deleted exactly one fixture record and verified absence. Steward test persisted and checked all eight receipts.
- Source `gofmt` and `git diff --check` — PASS.
- Secret scans — focused staged plaintext credential-pattern scan: 0 findings. Gitleaks 8.30.1 scanned all four branch commits (`e4eb789..HEAD`) with full redaction: no leaks found.
- Built `output/brokerd-memory-cleanup`, read `-h`, and confirmed `-execute` without a manifest refuses before connecting — PASS.

Portable evidence: `docs/evidence/2026-11-03-local-qualification.txt`. Raw local logs: `output/verified-gates.json`, `output/verified-go-{build,test,vet}.log`, `output/final-go-race.log`, `output/final-acceptance.log`. Live inventory page evidence: `docs/evidence/2026-11-03-broker-smoke-api.json`; executable reviewed manifest: `docs/evidence/2026-11-03-broker-smoke.json`.

## Migrations / live state

Additive and idempotent `daily_counters(scope, operation, day, count)` table in the SQLite bootstrap schema. No existing rows are rewritten or deleted. Test close/reopen verifies resulting persisted budgets. The old binary ignores the table. **No direct live DB/config edits, reinstall, service restart, release flip, push, merge, or other-worktree changes.** Live API inventory reads naturally generate the broker's normal receipts/access metadata.

## Apex verification / integration

1. Review these commits and run local gates in an isolated checkout; independent review remains Apex-owned. Reproduce locally:
   ```sh
   mkdir -p output/tmp
   TMPDIR="$PWD/output/tmp" go test ./test/e2e -run '^Test(CleanupCommandDefaultsToDryRun|StewardBrokerAcceptance)$' -v -count=1
   ```
2. Inspect active `catalog.d` overlays before installing. An overlay **replaces** its embedded connector, so the new embedded rules do not automatically apply to an older live overlay. Port/review the needed rules and choose daily caps in the control plane; no new cap is guessed for existing grants. Review `operation_constraints` passthrough in any upstream control-plane schema before depending on it.
3. Apex alone installs/restarts the verified target using existing release/rollback procedures. Keep current binary/config available. Mint a fresh test grant with an approved named query and a **private test memory namespace**, not the steward production namespace. Run query read → broker-memory store → scoped recall through the broker URLs. Check actual result/content/tags, effective limits, and paired receipts (grant/agent/project/session/run, policy version, prepared argument digest, returned memory ID). A real scheduled PERSIST wake is separate live proof. Record test IDs for later separately authorized cleanup.
4. To review current smoke residue, securely supply an existing owner token as `BROKER_TOKEN` (no argv/logging), then:
   ```sh
   TMPDIR="$PWD/output/tmp" go build -o output/brokerd-memory-cleanup ./cmd/brokerd-memory-cleanup
   output/brokerd-memory-cleanup > output/smoke-current.json
   output/brokerd-memory-cleanup -manifest docs/evidence/2026-11-03-broker-smoke.json
   ```
   Both commands only read. Only after Daniel/Apex authorizes deletion of the reviewed IDs:
   ```sh
   output/brokerd-memory-cleanup -manifest docs/evidence/2026-11-03-broker-smoke.json -execute
   ```
   Revalidation or enumeration failure is a stop, not permission to bulk-delete by tag. Keep the original manifest and cleanup receipts.

## Rollback and risks

- Roll back to the previously installed binary through Apex's normal tooling. Leave the additive daily counter table intact; do not delete operational history. Older code ignores it but also lacks the new enforcement. Revert new overlay/control-plane settings separately if needed, preserving a snapshot.
- Row ceilings cover recognized collection payloads and verbose responses; they do **not** cap provider scans/cost. The stock ClickHouse tool has no request settings fields. Retain read-only provider profile limits; broker deadline cancellation is best effort. Qualify new result envelopes explicitly.
- Daily budgets are per agent/project/connector/operation and UTC day, and count admitted attempts even if audit/provider later fails. Legacy per-run counters may consume attempts before final argument denial. No daily limit is introduced for a grant that supplies none.
- Existing memory recall gains a default request/result bound of 50. Empty WordPress site filters are now denied, fan-out defaults to grant sites, and memory updates cannot drop their own namespace or exceed importance bounds. `run_across` remains an explicitly broad write-capable operation, not a read/draft substitute.
- No embedded mail/spend connector exists. Generic recipient/prefix/amount rules are locally tested; they are not live email/payment qualification, and they do not implement Phase 2 approval.
- AutoMem has no transactional conditional delete. Content/tags may change between revalidation and deletion. The command checks all IDs before any delete, but provider failure can still leave a partial batch. Re-inventory/review before retrying. The manifest preserves content/tags, not an undo: recreated memories get new IDs and graph links would need separate recovery.
