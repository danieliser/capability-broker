# n8n-broker handoff

Owner: Daniel. Builder: Codex (GPT-6). Prepared 2026-11-04 UTC.
Worktree: `/Users/danieliser/Toolkit/capability-broker-wt/n8n-connector`.
Branch: `feat/n8n-connector-20261104`.
Base verified: `c0c137b598777002c1688efe2df83b8a6c97498e` (Go broker 0.12.0).
Implementation commit: `34da349616b557e3cbb8956a990af378ea9a6c12`
(`feat(n8n): isolate owner MCP and scope agent workflow tools`).
This handoff is committed separately; `git log -2 --oneline` identifies both commits.
No pushes, merges, live installs/restarts, DB/config edits, credential reads or
provider effects were performed. Only fixture effects ran in synthetic servers.

## Per-item delivery

1. **Done — owner connector.** Exact `n8n` entry uses the recorded instance
   `http://127.0.0.1:5678/mcp-server/http`, Streamable HTTP, `owner_only: true`,
   passthrough exposure and original server name. Existing header secret
   interpolation resolves `${secret:N8N_MCP_TOKEN}` from the broker secret store.
   The runbook pipes Keychain service `n8n-mcp-token` / account `persist` into
   `brokerd secret set N8N_MCP_TOKEN`; no plaintext credential in YAML or git.
   Original schemas, title, annotations, metadata, content blocks, structured
   results and isError were compared over HTTP against a direct fixture client.
   Owner arguments (including verbose), management tool names and instructions
   remain unchanged. Added runtime owner-only enforcement for existing grants.
2. **Done — scoped agent workflow tools.** Selected broker -> MCP Server Trigger
   workflow tools, using the existing adapter rather than inventing an unverified
   PERSIST HTTP flow-run API. `n8n` and `n8n-flows` embedded agent catalogs are
   empty, so management/newly discovered tools cannot synthesize authority.
   Trusted `n8n-flows` catalog entries bind actual tool names to reviewed logical
   IDs, effect classes, full git revisions, workflow IDs and versions. All tools
   use `flows.run`. Grants require logical flow allowlists, a positive daily
   ceiling, and extra `flows.write` / `flows.send` / `flows.spend` capabilities
   for those respective effects. Parent restrictions survive child grants.
   Budgets persist using existing atomic UTC-day counters by agent/project and
   shared flows.run operation across tools/grants. Reserved run correlation
   comes from grant/catalog; effectful runs require an idempotency key. The
   normal intent/outcome/denial receipts include the run and execution ID on
   successful calls. Ambiguous metadata or lost dispatch responses record
   outcome_unknown and instruct reconciliation.
3. **Done — checks and docs.** Full tests, touched-package race checks, vet,
   build, formatting, diff checks and redacted secret scan passed. CHANGELOG
   [Unreleased], README link, exact owner recipe, agent catalog template,
   installation/grant/output contracts, live acceptance and rollback documented.

Nothing in the requested Go deliverable is skipped. Live activation and a real
fixture trigger deployment are intentionally left to Apex as instructed.

## Files

- `docs/n8n.md`: exact Keychain import, connectors entry, `brokerd client install`
  steps, workflow schema/result/grant contract, live verification and rollback.
- `docs/examples/n8n-connectors.yaml`: tested owner configuration example.
- `docs/examples/n8n-flows.yaml`: deployment-specific catalog template; replace
  fixture metadata before installation.
- `connectors.example.yaml`, `README.md`, `CHANGELOG.md`: discovery and release docs.
- `pkg/catalog/connectors/{n8n,n8n-flows}.yaml`: deny-by-default embedded catalogs.
- `pkg/catalog/{catalog.go,flows.go,flows_test.go}`: reviewed flow binding schema,
  shared operation key, validation and fail-closed tests.
- `pkg/policy/{policy.go,arguments.go,flows.go,flows_test.go}`: ID/effect/run gates,
  injected correlation and inherited grant checks before execution.
- `pkg/adapters/upstream/{upstream.go,flows.go}`: flow-aware discovery, runtime
  owner-only guards, execution metadata validation and ambiguous result handling.
- `pkg/mcpfront/{front.go,n8n_test.go}`: owner-only HTTP/dispatch guards and real
  HTTP + scratch SQLite fixture coverage.
- `pkg/connectors/n8n_test.go`: exact owner recipe and missing-secret denial.

## Checks and evidence

All commands exited 0 and their results were read. Test scratch roots were under
this worktree's ignored `output/test-tmp` for final runs.

- `go test ./...`: passed, including `test/e2e`.
- `go test -race ./pkg/catalog ./pkg/connectors ./pkg/policy ./pkg/adapters/upstream ./pkg/mcpfront`: passed.
- `go vet ./...`, `go build ./...`: passed.
- `gofmt` check of `pkg` and `cmd`, `git diff --check`: passed.
- `gitleaks dir --redact --no-banner .`: no leaks; final scan ran after test
  scratch cleanup to avoid transient directory races.
- Local logs: `output/go-test.txt`, `output/go-race.txt`, `output/gitleaks.txt`.
- HTTP fixtures exercised direct-vs-broker owner parity, management denial even
  when bypassing discovery, owner-only direct/aggregate access, read-only tool
  discovery, allowlist absence/foreign IDs, caller correlation spoofing, missing
  run IDs, missing effect capability, inherited constraints, missing/zero daily
  budgets, authorized write/send/spend, quota exhaustion across grants, missing
  execution correlation and a lost dispatch response. Denied calls did not reach
  the synthetic upstream. Real SQLite receipts were inspected.
- Current official MCP Server Trigger docs confirm workflow tools and Streamable
  HTTP support: https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-langchain.mcptrigger
- SOFA lookup for n8n MCP Server Trigger/Streamable HTTP returned unrelated
  generic MCP posts, no applicable n8n implementation evidence. No SOFA writes
  or contributions. Decisions rely on repository code, official docs and local
  fixtures.

These are local fixture/transport proofs, not live n8n execution, provider,
production, PERSIST ledger or deployment acceptance.

## Migrations

None. Reuses existing grants, receipt tables and persistent daily counters.

## Apex integration and live acceptance

1. Review/integrate the implementation commit and this handoff. Binary activation
   belongs to Apex; the builder did not install or restart the live broker.
2. Follow `docs/n8n.md` to import the existing Keychain token through stdin,
   merge the exact n8n owner entry, inspect broker status and run:
   `brokerd client install claude-code --mode servers` and
   `brokerd client install codex --mode servers`.
   These retain other connectors and create per-server n8n entries.
3. Compare owner initialize/tools/list with instance MCP discovery; run only an
   explicitly reviewed read-only instance tool. Do not create/delete workflows
   merely to test owner access.
4. Trusted deployer publishes a fixture-only MCP Server Trigger and reviewed
   workflow tool, with a separate narrow trigger bearer stored in Keychain as
   `n8n-flow-mcp-token` / `persist`. Import as `N8N_FLOW_MCP_TOKEN`.
   Use its actual Streamable HTTP URL, never the instance MCP URL. Replace every
   template tool/deployment value and install the catalog before enabling the
   separate n8n-flows connector.
5. Mint the fixture grant from the docs with an actual PERSIST run_id. Confirm
   agent management denial, effect denial, allowlist denial and daily exhaustion
   before any production capability. Run the fixture and reconcile actual n8n
   workflow/version/execution IDs with broker receipts and PERSIST's run ledger.
   PERSIST must retain the returned IDs; this Go worktree does not change the
   PERSIST ledger/UI. No production writes, sends or spend are authorized here.

## Rollback

Disable/remove only n8n and n8n-flows entries and let configuration hot-reload;
revoke fixture grants. Remove the flow catalog/unused broker secret names after
connector removal. Restore prior adopted n8n client entries if applicable with
`brokerd client restore n8n`; do not globally revert unrelated client servers.
Keep audit receipts, daily counters and n8n data. Code rollback before deployment
is simply declining/reverting the feature commit; no schema rollback is needed.

## Risks and remaining integration limits

- Runtime uses trusted, owner-reviewed workflow deployments. Catalog effect
  labels are not inferred by inspecting arbitrary node graphs. Agents cannot
  edit those deployments through this connector.
- Output validation detects correlation/deployment mismatch after execution; it
  is not a pre-dispatch version fence or independent execution API readback.
  Disable/review/rebind tools before changing workflow code, destinations or
  credentials. Reconcile actual execution records through the trusted deployer.
- An idempotency key is required/recorded for effects; workflow-side deduplication
  at the effect boundary is still required. No exactly-once claim. Daily call
  limits are not monetary ceilings. Validate recipients/amounts/targets in the
  reviewed flow and declarative argument rules before real sends/spend/writes.
- Each flow binding has one effect class. Split mixed-effect flows or keep them
  unavailable until all required authority is represented; never underlabel them.
- Workflow tool schemas must accept the injected correlation fields as optional
  top-level inputs. Return the documented metadata envelope with actual n8n
  execution context. Stock arbitrary workflows may need this fixture wrapper.
- Owner credentials, same-UID host isolation, Community credential governance and
  n8n availability remain the foundation/deployer boundary. No EE code or license
  mechanisms changed, no new mounts/network exposure, and n8n receives no owner
  broker/PERSIST tokens, browser profiles, home directory or Docker socket.

PERSIST_TASK_OUTCOME: {"version":1,"status":"completed","reason":"Delivered isolated owner n8n MCP recipe and grant-scoped workflow catalog implementation, docs, fixture evidence, full tests and touched-package race checks; live activation reserved for Apex."}
