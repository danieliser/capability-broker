# Independent review: n8n broker

Owner: Daniel. Reviewer: Codex (GPT-6), independent of the builder session.
Reviewed worktree: `/Users/danieliser/Toolkit/capability-broker-wt/n8n-connector`.
Branch: `feat/n8n-connector-20261104`.
Base: `c0c137b598777002c1688efe2df83b8a6c97498e` (brokerd 0.12.0).
Incoming feature: `34da349616b557e3cbb8956a990af378ea9a6c12` plus builder
handoff commit `09eae5b5b4d51ee421594e4852af5b2358ff2481`.
Builder handoff was read from the requested fleet directory and compared with
this checkout. The initial checkout was clean and matched its identity.

## Verdict

**PASS after two fixes, for local fixture qualification and Apex integration.**
No remaining reproduced blocker in the requested Go review scope. Do not deploy
the original feature alone: integrate both review fix commits below. This is
not live n8n, production workflow, provider-effect or PERSIST ledger acceptance.

Nothing was installed or restarted. No live broker DB/config/secret was read or edited,
no credentials were copied or printed, and no workflows/providers were called.
No push, merge, release, external message or other checkout mutation occurred.
The only outside-worktree write is the explicitly requested copy of this handoff.

## Findings and fixes

### P1 — repeated effect keys were dispatched again, including unknown outcomes

At incoming HEAD, `pkg/policy/flows.go` checked only that an effectful call had a
nonempty string key. Dispatch had no durable duplicate admission guard. The
builder disclosed workflow-side deduplication as a limitation; that did not
provide a broker gate for effect retries.

Reproduction: `TestN8nEffectReplayDeniedAfterUnknown` called a synthetic send
workflow whose response omitted execution metadata, then retried the same key
through a new grant/run. Before the fix, the test failed with `calls=2`; both
dispatches produced ambiguous results. Evidence:
`output/review-n8n/repro-replay.log`.

Fixed by `6362c1c` (`fix(n8n): reserve effect keys before workflow dispatch`):

- `pkg/auth/flows.go:15` atomically inserts a hashed key into `flow_run_keys`.
- `pkg/mcpfront/front.go:688` reserves only agent effectful calls, after final
  authorization/argument/budget checks and before intent/dispatch.
- Keys are scoped by connector/project/logical-flow, independent of agent,
  grant, run, tool name, revision and day. Duplicate requests fail closed with
  `BROKER_DUPLICATE_FLOW_RUN`; reservations survive unknown outcomes, restart,
  and even failure to write the subsequent intent.
- Owner calls bypass the guard and retain their arguments/results.
- Tests cover ten concurrent dispatch attempts reaching upstream once, 32
  reservation attempts through two independently opened SQLite connections
  admitting once, persistence after closing/reopening both connections,
  unavailable-store denial, unknown-outcome replay and owner repeat calls.

No successful result cache or exactly-once guarantee is implied. Workflow-side
deduplication remains necessary at the actual effect boundary. A crash between
reservation and intent may leave a reserved key without an intent receipt;
do not clear it or invent a replacement key without reconciliation. Duplicate
attempts can consume daily quota because quota reservation precedes key admission.

### P2 — an effectful workflow error was classified as a known failure

At incoming HEAD, a correlated `isError` result reached generic `adapter.Failed`,
so the receipt said `failed`. A later node error cannot establish that earlier
nodes performed no effects. The error receipt also lost the known execution ID.

Reproduction: `TestN8nEffectErrorRequiresReconciliation` returned valid workflow,
revision, run and execution metadata with `isError: true`. Before the fix the
receipt state was `failed`, with no reconciliation instruction. Evidence:
`output/review-n8n/repro-partial-error.log`.

Fixed by `9d292e8` (`fix(n8n): reconcile possible effects on workflow errors`):
`pkg/adapters/upstream/upstream.go:581` records effectful errors as unknown,
returns an explicit reconciliation message, and carries the already validated
execution ID through `adapter.UpstreamError` into the outcome receipt.
The regression asserts both `outcome_unknown` and `exec-fixture-1` in SQLite.
Owner raw errors and non-flow/read error handling retain their existing behavior.

Both commits carry `Co-Authored-By: Codex (GPT-6) <noreply@openai.com>`.

## Review coverage / per-item status

1. **Done — instance isolation.** Empty embedded `n8n` and `n8n-flows` catalogs
   prevent synthesized operations. Written n8n instance operations are rejected;
   flow operations require reviewed bindings. Runtime owner-only checks protect
   old grants. Added `TestN8nReviewManagementPaths` denies create/update/delete/
   execute, renamed management and never-discovered names through internal
   dispatch and forced discovery calls, even with broad operation lists. Actual
   HTTP aggregate checks cover default prefixes, `prefix=none`, and
   `ondemand=all`, with no upstream management calls. Removing the fixture's
   owner-only flag still cannot synthesize instance authority. Existing grant
   minting rejects owner profiles for agents and owner-only connectors; child
   intersection tests plus `TestFlowInheritedAuthorityAndCorrelation` cover
   inherited allowlists and effect authority.
2. **Done — owner transparency.** `TestN8nOwnerVerbatimHTTP` compares direct MCP
   and broker HTTP tool definitions, instructions, arbitrary arguments, content,
   structured content, metadata and `isError`. Additional repeat-key owner tests
   prove the new guard does not constrain owners. Existing other-connector owner
   transparency/exposure tests passed.
3. **Done — flow gates.** Reviewed catalog identity/effects, grant allowlists,
   inherited restrictions, missing run/key/budget denial, write/send/spend
   capabilities, injected correlation and cross-grant quota exhaustion were
   exercised. Existing daily ceiling tests admit exactly five of 30 attempts
   through three independent SQLite connections; persistence/UTC tests passed.
   Missing metadata and lost dispatch responses remain unknown; effect error
   and replay gaps are fixed above.
4. **Done — secrets.** Tested example uses `${secret:N8N_MCP_TOKEN}` and fails
   closed without the referenced secret. It has no literal credential, strip,
   tool filter or response rewrite. No credential inspection was needed. The
   existing broker secrets file is plaintext protected by filesystem permissions,
   not encrypted storage; this review does not claim encryption at rest.
5. **Done — regression gates.** All required gates passed below. New branches
   activate only for agent effectful flow bindings. No other connector policy or
   transport behavior was changed.
6. **Done — handoff.** This file is committed at the worktree root and copied
   byte-for-byte to the requested fleet builders directory.

## Tests and results

All results were read; exits were 0 except the two intentionally failing
pre-fix reproductions. Scratch files live under the ignored worktree output
directory, with `TMPDIR="$PWD/output/review-n8n/test-tmp"` for Go checks.

- Baseline `go test ./...`: passed before review changes.
- Final `go build ./...`: passed (`output/review-n8n/go-build.log`).
- Final `go vet ./...`: passed (`output/review-n8n/go-vet.log`).
- Final `go test ./...`: passed, including `test/e2e`
  (`output/review-n8n/go-test.log`).
- Final `go test -race ./pkg/adapter ./pkg/auth ./pkg/durable ./pkg/catalog ./pkg/connectors ./pkg/policy ./pkg/adapters/upstream ./pkg/mcpfront`:
  passed (`output/review-n8n/go-race.log`). Adapter/durable have no standalone
  tests; their paths are exercised by the auth/front packages.
- Formatting, `git diff --check`, and redacted gitleaks directory scan passed.
- Built a separate stamped binary under `output/review-n8n/brokerd` and checked
  `--require-build 0.12.0-n8n-review.9d292e8@9d292e8`. This starts no daemon.

No remote review, SOFA lookup or external implementation pattern was required:
the defects were reproduced locally and use the existing SQLite admission
pattern. Existing SDK transport and source were inspected locally.

## Schema / changed files

Additive startup schema only: `flow_run_keys(scope, key_hash, created_at)` with
composite primary key. No backfill, deletion, existing-row update, new secret
storage or live migration was performed. Local reopen/concurrency tests verified
the resulting table behavior. Older binaries ignore the extra table.

Review changes: `pkg/auth/flows.go`, `pkg/auth/flows_test.go`,
`pkg/durable/sqlite.go`, `pkg/adapter/adapter.go`,
`pkg/adapters/upstream/upstream.go`, `pkg/mcpfront/front.go`,
`pkg/mcpfront/n8n_test.go`, `docs/n8n.md`, `CHANGELOG.md`, and this handoff.

## Exact installation steps — Apex executes, reviewer did not

1. Integrate the builder commits plus **both** fixes `6362c1c` and `9d292e8`.
   Keep the working tree clean. If integrating elsewhere, use that integration
   commit as the build identity and rerun gates there.
2. Inspect the active service before activation; do not assume a different
   daemon or data directory. For the documented macOS installation:

   ```sh
   launchctl print "gui/$(id -u)/com.capability-broker.brokerd"
   plutil -extract ProgramArguments json -o - "$HOME/Library/LaunchAgents/com.capability-broker.brokerd.plist"
   ```

   The recipe below applies only if this confirms the documented
   `~/.local/bin/brokerd`, `~/.brokerd`, port 8890 target. Preserve actual target
   overrides/flags if different; do not switch environments. Quiesce agent flow
   dispatch and retain the prior binary before activation. Keep receipts and
   counters; never restore an old database over new effect reservations.
3. Build, verify and install from the reviewed clean checkout:

   ```sh
   review_revision="$(git rev-parse HEAD)"
   review_version="0.12.0-n8n-review.$(git rev-parse --short HEAD)"
   mkdir -p output/review-n8n/rollback
   chmod 700 output/review-n8n/rollback
   cp -p "$HOME/.local/bin/brokerd" output/review-n8n/rollback/brokerd
   ./scripts/build.sh "$review_version" "$review_revision" output/review-n8n/brokerd
   output/review-n8n/brokerd --require-build "$review_version@$review_revision"
   BROKERD_VERSION="$review_version" BROKERD_COMMIT="$review_revision" ./install.sh --source
   "$HOME/.local/bin/brokerd" --require-build "$review_version@$review_revision"
   brokerd status
   ```

   `install.sh --source` replaces the binary atomically and reloads the user
   service. Verify `brokerd status` reports the same executing commit, not just
   the on-disk binary. Do not install an unstamped `go build` artifact or the
   original unreviewed feature revision.
4. Apply `docs/n8n.md` owner setup: pipe the existing Keychain service
   `n8n-mcp-token` / account `persist` into `brokerd secret set N8N_MCP_TOKEN`,
   merge the exact owner-only entry, then run:

   ```sh
   brokerd client install claude-code --mode servers
   brokerd client install codex --mode servers
   ```

   Never print the token or place it in YAML/arguments. Preserve other connectors.
5. Keep `n8n-flows` disabled until the trusted deployer provides a separate
   narrow trigger credential/URL and actual reviewed tool/workflow/revision/
   version bindings. Never point it at the instance management endpoint. Replace
   the template metadata in `docs/examples/n8n-flows.yaml` before enabling.

## Apex live verification and rollback

Compare owner initialize/tools/list with direct n8n, and use one approved
read-only tool. Do not mutate instance workflows for acceptance. Confirm agent
grant minting for `n8n` is refused and management guesses are denied through
direct/aggregate/discovery paths, with no additional n8n executions.

Use only a reviewed fixture flow with a real PERSIST run ID. Verify allowlist,
missing capability/key and daily budget denials before any provider effects.
Reconcile workflow/version/execution IDs with actual n8n execution records and
the PERSIST ledger. Repeat one fixture effect key concurrently and across a
fresh grant: exactly one broker dispatch, all repeats denied. Check unknown
outcomes block reuse and keep their execution ID when one was validated. Confirm
representative existing connectors still list/call under owner and agent grants.

For rollback, disable only n8n/n8n-flows, revoke fixture grants, and let hot reload
remove them. If a binary rollback is needed, atomically copy the saved binary
beside `~/.local/bin/brokerd`, rename it into place, and kickstart the verified
same service. Check its executing identity and existing connector health. Keep
the new `flow_run_keys` table, receipts and counters. Do not enable agent effects
on the older binary, which lacks the replay guard. Do not globally revert client
configuration or delete n8n data.

Remaining trust limits: owner-controlled configuration/catalog and workflow
deployments are authoritative; renaming a tool does not infer safe effects.
Returned deployment metadata is checked after dispatch, not a pre-dispatch
version fence or independent API readback. Credentials and workflow graph
governance remain deployer responsibilities. New arbitrary idempotency keys
represent new effects and still require workflow-side validation/deduplication.
Local green tests do not establish any live/provider/PERSIST outcome.

PERSIST_TASK_OUTCOME: {"version":1,"status":"completed","reason":"Independent review completed; two reproduced defects fixed in small attributed commits, required Go gates and race checks passed, and installation/live verification/rollback handoff delivered without touching the live broker."}
