# HANDOFF — review-broker-agentd

Owner: Daniel. Independent reviewer: review-broker-agentd (GPT-6 Codex).
Integrator, installer and live verifier: Apex. Date: 2026-11-03.

## Verdicts

| Repository | Reviewed input | Verdict |
| --- | --- | --- |
| capability-broker | `feat/acceptance-cleanup-20261103`, `27b6e38` against pinned main `e4eb789` | **ship-with-notes** after the three fixes below; all required local gates pass. |
| agentruntime | `fix/transport-isolation-20261103`, `7359ad9` against pinned main `ade7004` | **ship-with-notes** after the two fixes below; all required local gates pass. |

No unresolved evidenced blocker remains in the requested review scope. These are
source/local-fixture verdicts, not serving-daemon or live-provider qualification.
No original builder work was authored by this reviewer. Both requested worktrees
were clean on entry. All changes stay in those worktrees and their original branches.

## Findings and fixes

Locations below refer to the corrected source; failing regressions were run before
implementation. Severity: P1 high, P2 medium.

| ID | Severity | Location | Reproduction / consequence | Fixed? |
| --- | --- | --- | --- | --- |
| A1 | P2 | `pkg/auth/capabilities.go:184` | Parent prefix `""` is denied, but intersecting a child prefix `"approved:"` produced an allowed grant. A child could repair a fail-closed parent restriction. | Yes, `ff88524`; empty/incompatible prefixes remain denied. |
| A2 | P1 | `pkg/policy/policy.go:68`, `pkg/adapters/upstream/upstream.go:373`, `:494`, `pkg/mcpfront/front.go:628` | Owner SQL hit SELECT/named-query guards; WordPress `status:publish` became draft and `persist_refs:true` became false; agent-only operation mapping hid owner extensions/actions. This contradicts the v0.7.0 transparent-owner contract. Much of this defect also exists in the pinned base; the new execution path could additionally apply policy deadlines. | Yes, `d2cb8f9`; owner arguments bypass agent policy/guards, uncatalogued upstream tools/actions pass through, and explicit connector exclusions remain enforced. |
| A3 | P2 | `pkg/memorycleanup/cleanup.go:124`, `:137` | Revalidation accepted changed scope tags using Unicode case folding; uppercase and long-s variants could still reach deletion. Inventory also omitted explicit exact-match mode. | Yes, `0e32b31`; byte-exact scope comparisons and `tag_match:exact`. Deletion remains ID-only. |
| B1 | P1 | `pkg/nativeprotocol/transport.go:298`, `pkg/nativeprotocol/claude_steer.go:98`, `:116` | A short write with nil error was treated as complete and authorized a steer boundary. A partial write with an error still permitted another input on the same transport. Queued writers did not recheck retirement. | Yes, `5bce86f`; short/error writes retire input, queued writers fail closed, and cancellation is finalized before publishing a steer boundary. No fragment is resent. |
| B2 | P2 | `pkg/eventstream/activity.go:68` | A same-session poll advancing history held its gate across storage IO. Another request whose older snapshot was already cached could not finish until that IO completed. | Yes, `a3c6624`; copy derived maps under a short mutex, replay without locks, publish only newer generation/cursor state, and prevent stale failures from invalidating newer results. |

## Commits and changed files

Broker review commits:

- `ff88524` — `fix(auth): preserve denied prefixes in child grants`
  (`pkg/auth/capabilities.go`, `pkg/policy/review_constraints_test.go`).
- `d2cb8f9` — `fix(broker): preserve owner tool transparency`
  (`pkg/catalog/catalog.go`, `pkg/mcpfront/front.go`, `pkg/policy/policy.go`,
  `pkg/adapters/upstream/upstream.go`, existing owner/SQL tests,
  `pkg/adapters/upstream/review_owner_test.go`, `test/e2e/review_owner_test.go`).
- `0e32b31` — `fix(memory): require exact cleanup scope tags`
  (`pkg/memorycleanup/cleanup.go`, `cleanup_test.go`).

AgentD review commits:

- `5bce86f` — `fix(transport): retire uncertain native input writes`
  (`pkg/nativeprotocol/claude_steer.go`, `transport.go`, `write_deadline_test.go`).
- `a3c6624` — `fix(api): release activity cache locks before replay IO`
  (`pkg/eventstream/activity.go`, `review_cache_test.go`).

Each repository also has a final `docs(review)` commit containing this combined
handoff, its CHANGELOG [Unreleased] update, and its relevant behavior documentation
(`docs/operation-constraints.md` or `docs/transport-isolation.md`). Every review
commit has the `Co-Authored-By: GPT-6 Codex <noreply@openai.com>` trailer.
No push, merge, tag, release, install, daemon restart or live DB/config edit occurred.

## Validation and evidence

Go: `go1.26.5 darwin/arm64`. Scratch build/test files use each worktree's ignored
output directory via TMPDIR; no retained work is under /tmp.

Broker, all PASS:

- `go build ./...`
- `go vet ./...`
- `go test ./...`
- `go test -race ./pkg/auth ./pkg/catalog ./pkg/policy ./pkg/adapters/upstream ./pkg/mcpfront ./pkg/memorycleanup ./test/e2e`
- New regressions cover denied prefix inheritance, byte-exact case/Unicode strings,
  nested list/container/type rejection, 30 competing reservations through three
  independent SQLite connections (exactly five admissions), owner raw SQL and
  WordPress arguments, owner-only upstream extensions, explicit tool exclusions,
  and changed cleanup-scope tags. Existing restart/UTC rollover, post-enrichment
  denial, effective argument digests and destructive cleanup fixtures also pass.
- `git diff --check`; staged and three-review-commit Gitleaks scans: PASS, no leaks.

Broker logs: `output/review/gates.json`, `{build,vet,test,race}.log`.
Red evidence: `broker-red.log`, `cleanup-red.log`, `owner-tool-red.log`.
Green HTTP owner proof: `owner-tool-green.log`; full-suite logs include the existing
steward receipt/digest and built cleanup-CLI proofs. The initial new HTTP fixture
used the wrong cleanup order and hung closing the upstream before its MCP client;
that test-only mistake was corrected and the full/e2e/race suites then passed.

AgentD, all PASS:

- `go build ./...`
- `go vet ./...`
- `go test ./...`
- `go test -race ./pkg/nativeprotocol ./pkg/eventstream ./pkg/api ./pkg/runtime`
- Additional final `go test -race ./pkg/eventstream -count=1`: PASS, including a stale
  failed replay racing a newer successful cache update.
- Cache checks cover snapshot tail/cursors, older snapshots, failure recovery,
  generation changes, process-cache restart, eviction, session separation, cancelled
  reads, unrelated sessions during blocked IO, and cached same-session reads during IO.
- Native checks cover blocked/queued cancellation, cancelled successful writes,
  partial/error and short/nil-error writes, refusal to reuse uncertain input, and
  steer boundary accounting. No race-detector report in either broad race run.
- `git diff --check`; staged and two-review-commit Gitleaks scans: PASS, no leaks.
  A broad directory scan flags the existing `README.md:508` and its ignored baseline
  copy; that README line is identical to pinned `ade7004`. No suppression was added.

AgentD logs: `.artifacts/review/gates.json`, `{build,vet,test,race-final}.log`,
`cache-final.log`, `native-red.log`, `cache-io-red.log`, `focused-final.log`.
The final 20-operation synthetic benchmark passed: **2,454,308 ns/op**,
**211,812 B/op**, **1,159 allocs/op** (`.artifacts/review/benchmark.log`).
This is parallel throughput, not live request latency or a controlled before/after
comparison with the original builder's machine-load conditions.
No paid-provider turn, Docker image rebuild, host-isolation reprobe, scheduled wake,
real memory deletion or live daemon exercise was performed by this review.

## Migrations and remaining notes

No reviewer migrations or dependencies. Broker's branch already adds the idempotent
`daily_counters` table; review tests verify resulting persisted admissions and the
existing close/reopen/rotation tests pass. Keep this history during rollback. AgentD
has no schema changes.

Broker notes: overlays replace embedded connector definitions; Apex must port and
review live operation rules rather than assume new embedded rules take effect.
Daily caps are agent/project/connector/operation/UTC-day admitted-attempt budgets,
not a shared parent/descendant spend pool. Row checks bound recognized response
collections, not provider scan cost. Owner grants intentionally bypass agent policy;
use connector exposure controls for owner exclusions. Cleanup revalidation and
provider deletion are not one transaction: changed data or provider failure can
leave an uncertain/partial batch. Re-inventory and review before any retry.

AgentD notes: cold/evicted views still replay full history; concurrent advancing
polls can duplicate replay IO to keep cache locks out of storage operations. Cache
publication is generation/cursor monotonic; no raw provider output is cached.
Cancelling/retiring stdin cannot prove that the provider received zero bytes, and
must not cause automatic retry or a fresh paid turn. Exact arbitrary Codex tool
inventories remain unsupported, as documented by the original builder.

## Apex installation and live verification — broker

1. Integrate the builder plus all three review fixes into the intended source
   target; rerun the gates. Verify the currently active binary path, service label,
   port and data/config root. Preserve binary and configuration snapshots. Do not
   infer the active target from installer defaults.
2. Review live `catalog.d` overlays and control-plane `operation_constraints`
   passthrough. Select the intended grant caps explicitly. This review changed no
   live overlay or grant.
3. From the integrated clean checkout, use the existing source installer with
   explicit verified `BIN_DIR`, `BROKERD_DATA_DIR`, `BROKERD_PORT`, selected
   `BROKERD_VERSION` and actual `BROKERD_COMMIT`: `./install.sh --source`.
   It builds `.new` binaries and renames them atomically. Apex alone performs the
   service replacement/restart; macOS user-service default label is
   `com.capability-broker.brokerd` (verify the actual target first).
4. Verify serving build identity. Through scratch grants/providers, repeat named
   query → namespace memory store → scoped recall and check actual effective args,
   counters, returned ID and paired receipt digests. Exercise owner WordPress/SQL
   transparency only against harmless fixtures or separately approved live actions.
   Test daily denial and a restart using the same scratch principal/project.
5. Cleanup CLI defaults to read-only inventory/revalidation. Keep execute withheld
   until the exact current IDs/content/scope are reviewed and deletion is authorized;
   it sends only `delete_memory(memory_id=...)`, never tags/bulk mode.

Broker rollback: atomically restore preserved `brokerd`/probe binaries and the
matching config snapshot, restart the same verified service and check build/health.
Leave daily counters and receipt history intact. An older binary ignores the new
counter table but lacks this enforcement. Source-only rollback of review fixes is
`0e32b31`, `d2cb8f9`, `ff88524` in reverse order; prefer the preserved known-good
runtime over removing fixes from an unqualified live build.

## Apex installation and live verification — AgentD

1. Integrate builder plus `5bce86f` and `a3c6624`; rerun default/race gates. Discover
   the active executable, service, data root, port and exact image references/digests.
   The builder noted port 38093; that is not a fresh live observation by this reviewer.
   Do not switch to the installer's default 8090. Preserve binaries, service config
   and image digests; do not prune images or live containers.
2. Select one exact semantic version and actual integrated commit identity. Build
   required images using `AGENTD_VERSION=<version> AGENTD_COMMIT=<commit>
   ./docker/build.sh all`, and inspect every agent/proxy image label/digest for that
   same identity. The restored old 2.6.0 image alone does not qualify a newly stamped
   binary. Run the scratch Docker/no-turn probes in `docs/transport-isolation.md`.
3. Build a stamped daemon into a staging path, never over the active executable:
   `go build -trimpath -ldflags="-X github.com/danieliser/agentruntime/pkg/buildinfo.Version=<version> -X github.com/danieliser/agentruntime/pkg/buildinfo.Commit=<commit>" -o .artifacts/review/agentd.new ./cmd/agentd`.
   Run that staged binary with `--require-build <version>@<commit>`. Stage beside the
   verified installed executable, then atomically rename it and restart only that
   service through Apex's established procedure. The generic `install.sh` builds
   directly to its target and rewrites service defaults; do not use it blindly for
   this active-target upgrade.
4. Verify serving build identity and capabilities. Poll 11 simultaneous scratch
   streams under the real client deadline; measure individual request tail latency.
   Check restart/resume/eviction and session separation with durable sequence cursors.
   Exercise approval deny/allow scope and blocked/partial-input cancellation using
   fixtures; confirm indeterminate delivery is never automatically resent and a
   cancelled steer has no durable dispatch/authorized superseded boundary.
5. Any real paid-provider canary remains an Apex decision using scratch workspaces
   and the existing no-send/no-spend boundaries. Local tests are not that proof.

AgentD rollback: atomically restore the preserved binary, service configuration and
matching image references; restart the same service and verify its build identity.
No database rollback is required. Retain all session events/receipts. Source-only
review rollback order is `a3c6624`, then `5bce86f`. Do not resume or resend uncertain
provider input during rollback.

## Disposition

Done: both independent reviews, five evidenced fixes, focused regressions, small
local commits, required build/vet/full/race gates, documentation and combined handoff.
Skipped by explicit brief: installs/restarts, live writes/deletions, pushes/merges
and release flips. No requested local deliverable is blocked.
