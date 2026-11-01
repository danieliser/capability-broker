# Capability broker (`brokerd`) — design

Date: 2026-11-01. Owner: the broker builder. Inputs: `../2026-10-31/mcp-gateway-strategy.md` (verdict + owner
corrections, binding), research beats A/B/C in `../2026-10-31/mcp-gateway-research/`, beat D
(`capability-broker-research/D-go-mcp-transport.md`, Go transport + client behaviour), `../2026-10-31/context-audit.md`.

## Decisions already made by Daniel (2026-10-31), restated so nobody re-litigates them

| # | Decision | Consequence here |
|---|---|---|
| D1 | **Transparent to agents.** Agents see ordinary MCP servers; tool names and schemas pass through verbatim. | The broker speaks Streamable HTTP MCP at one URL per connector; `run_query`, `wp_run`, `store_memory` keep their names. Operations are an internal policy key. |
| D2 | **One system at every level.** Daniel's interactive Claude Code / Codex / Cursor sessions use the same broker with owner-tier client tokens. | The broker is always-on and independent of PERSIST's process; it replaces the 19 per-session stdio servers over time. |
| D3 | **Per-operation argument constraints** (recipient patterns, row limits, rates, templates) enforced after enrichment. | Grants carry constraints per operation; the policy engine runs after the adapter normalises arguments. |
| D4 | **Compact by default, `verbose: true` for the full upstream payload**; rtk-style lossless compression. | Every tool's input schema gains one optional `verbose` boolean. Compact shapes are declared per operation. |
| D5 | **Build in Go, do not fork 1mcp.** Mirror AgentD's layout, build and deploy mechanics. | New repo, `cmd/brokerd`, `pkg/…`, ldflags stamping, `--require-build`, launchd, atomic binary swap. |
| D6 | **PERSIST stays the owner of identity, permissions, approvals (judgement + inbox) and the audit of record.** | The broker has no users, no approval UI and no policy editor. It stores grant *projections* PERSIST pushes, writes receipts PERSIST pulls, and hands every approval to judgement. |
| D7 | Phase 1 = ClickHouse named queries, WordPress read + draft via the existing router abilities, memory namespace. | Gmail, Metabase, publish, spend and code mode are P2/P3 and only get their record shapes here. |

Note on the strategy doc: its "Build, do not fork" paragraph still says "Phase 1 is a TypeScript plugin in the
daemon". Daniel's later instruction (the brief for this work) overrides it: Go, standalone, because of D2.
That is the one place this design departs from the written strategy, and it is Daniel's call, not mine.

## 1. Where it lives

- Repo `~/Toolkit/capability-broker`, module `github.com/danieliser/capability-broker`, binary `brokerd`,
  installed at `~/.local/bin/brokerd`, data root `~/.brokerd` (0700), launchd `com.capability-broker.brokerd`,
  listen `127.0.0.1:8890` (free on this Mac; AgentD is 38093, PERSIST 8803).
- Layout mirrors AgentD: `cmd/brokerd/` (daemon + `client`, `grant`, `catalog`, `receipts` subcommands),
  `pkg/buildinfo`, `pkg/config`, `pkg/api` (HTTP front + admin), `pkg/auth` (tokens, grants), `pkg/catalog`
  (operations, named queries), `pkg/policy` (evaluate), `pkg/mcpfront` (per-connector MCP servers),
  `pkg/adapters/{clickhouse,wordpress,memory}`, `pkg/audit` (receipts, append-only), `pkg/approval` (action
  records, judgement hand-off), `pkg/durable/sqlite` (migrations). `install.sh`, `scripts/reinstall.sh`,
  `scripts/verify-release.sh` follow AgentD's; the version lives in `pkg/buildinfo` + CHANGELOG heading.
- Not inside PERSIST (D2, D5) and not inside agentruntime: AgentD's invariant is "secrets are launch-only,
  never persisted"; the broker's job is custody. Different trust domain, different process.
- Two listeners. **Front** (HTTP, loopback): `/mcp/<connector>` for agents and owner clients, `/healthz`
  (credential-free). **Admin** (Unix socket `~/.brokerd/admin.sock`, 0600, plus the admin token in
  `~/.brokerd/admin.token`): grants, catalog push, approval decisions, receipt export. Named agents run under
  Claude Code's OS sandbox with `~/.brokerd` denied, and the socket is inside it; owner-UID agents without the
  sandbox are explicitly *not* isolated from the admin surface (beat C §1; strategy rule 8). The audit's L1
  finding applies to the broker exactly as it applies to PERSIST.

## 2. Principals, tokens and how PERSIST mints them

Three principals, two token kinds, one table.

| Principal | Token | Minted by | Lifetime | Policy |
|---|---|---|---|---|
| Named agent run | **grant token** `cbg_…` (32 random bytes, hash stored) | PERSIST, through the admin socket, at spawn | the session; PERSIST revokes at terminal state; hard cap 24 h; renewable by PERSIST only | the agent file's `capabilities:` block, intersected with the project's |
| Owner client (Claude Code, Codex, Cursor) | **client token** `cbc_…` | `brokerd client mint --name claude-code` (admin) | until revoked | `owner` profile: every operation, no argument constraints, approvals auto-recorded as owner-present, still receipted |
| PERSIST / CLI | admin token | first boot (AgentD's `LoadOrCreateAuthToken` pattern, 0600, hard-link publication) | until rotated | admin routes only |

`grants` row: `id, token_hash, kind (agent|client), agent_id, project_key, session_id, run_id, parent_grant_id,
capabilities_json, policy_version, issued_by, expires_at, revoked_at, created_at`. Every call: hash the bearer,
load the grant, check `revoked_at IS NULL AND expires_at > now`, then the connector, then the operation, then
the constraints. Child delegation (P2) creates a grant whose capabilities are the intersection with the parent and
is revoked with it.

**PERSIST side (small TypeScript plugin `capability-broker`, ~200 lines):**
1. Agent file schema gains `capabilities:` (sibling of `mcp_allowlist`; a connector named in `capabilities`
   must also be in `mcp_allowlist`, otherwise `project sync` rejects the file). Example for the steward:
   ```yaml
   capabilities:
     clickhouse:
       queries: [scheduler.freshness, scheduler.row_counts, scheduler.sentinels]
       limits: { max_rows: 2000, max_execution_seconds: 60, calls_per_run: 40 }
     wp-mcp-router:
       sites: [wppopupmaker]
       operations: [content.read, content.draft]
       constraints: { post_types: [post, guides], max_drafts_per_run: 3 }
     memory:
       namespaces: [agent:companykit.data-sync-steward, project:companykit]
       operations: [recall, store]
       constraints: { store_per_run: 20, max_importance: 0.9 }
   ```
2. At spawn, next to the existing `session.credentials.issue` filter in `session-manager.ts`, the plugin
   `POST`s `/v1/grants` on the admin socket with the principal and capabilities and gets back `{token,
   expires_at, servers: {clickhouse: url, …}}`. It injects those as `grantedServers` into the session
   `.mcp.json` (the mechanism that already writes the kernel-built `persist` entry and wins name collisions),
   one `type: http` entry per connector with `Authorization: Bearer`. For Codex it adds
   `-c mcp_servers.<name>.url` plus `bearer_token_env_var=BROKER_GRANT` (same flag family the Codex isolation
   already uses for `persist`).
3. On the session's terminal state it `DELETE`s the grant. On daemon restart it re-syncs: every grant for a
   session PERSIST no longer considers live is revoked.
4. `project sync` also pushes the project's named-query file (`<repo>/.persist/broker/queries.yaml`) to
   `/v1/catalog/queries` so named queries are versioned with the repo and `policy_version` is their digest.

**Owner side:** `brokerd client mint --name claude-code` writes the token to the macOS Keychain
(`capability-broker/<name>`), and `brokerd client install claude-code|codex|cursor` rewrites the matching
`mcpServers` entries in `~/.claude.json` / `~/.codex/config.toml` into http entries pointing at the broker
(original entries backed up alongside, `--revert` restores). Server names are kept exactly as they are today
(`clickhouse`, `wp-mcp-router`, `memory`) so tool prefixes and model knowledge do not change.

## 3. The transparent MCP front

`GET|POST|DELETE /mcp/<connector>` is a Streamable HTTP MCP endpoint (go-sdk v1.8; beat D confirms the
handler/session pattern). Per authenticated bearer the broker builds one `mcp.Server`:

- `tools/list` returns the upstream tool definitions **verbatim** (name, description, inputSchema), filtered to
  the tools the grant can reach at all, with two additive edits: an optional `verbose` boolean property, and for
  agent grants a one-line suffix on `run_query`'s description listing the named queries the grant allows (so no
  discovery round-trip). Owner clients get the untouched upstream list.
- `tools/call` → `Resolve` (tool + argument pattern → operation id + normalised args) → schema validation with
  `additionalProperties: false` → `policy.Evaluate(grant, op, args)` → `allow | deny(reason) |
  approval_required(action)` → intent receipt → adapter → outcome receipt → compact or verbose response.
- Denials return an MCP tool error with a stable code (`BROKER_DENIED_OPERATION`, `BROKER_DENIED_ARGUMENT`,
  `BROKER_GRANT_REVOKED`, `BROKER_APPROVAL_REQUIRED`, `BROKER_UPSTREAM_UNAVAILABLE`), never a transport 5xx, so
  the model sees why and does not retry blindly.
- Unknown connector or a connector outside the grant: 404 before any MCP handshake. Discovery is not
  authorization; every call re-checks.

## 4. Operation catalog

Embedded YAML per connector (`catalog/<connector>.yaml`, go:embed) plus a pushed overlay for named queries.

```yaml
connector: clickhouse
version: 1
tools:            # verbatim upstream defs (from mcp-clickhouse), the agent-facing surface
  - name: run_query
    description: "…upstream text…"
    inputSchema: { … }
operations:
  - id: clickhouse.named_query
    tool: run_query
    match: { query: "^@[a-z0-9_.-]+$" }      # argument pattern that selects this operation
    risk: read
    enrich: resolve_named_query               # adapter step before policy
    constraints: [queries, max_rows, max_execution_seconds, calls_per_run]
    receipt: [query_id, rows, bytes_read, elapsed_ms]
    compact: { rows_preview: 20, totals: true }
  - id: clickhouse.sql.select
    tool: run_query
    risk: read
    requires_capability: sql.select            # owner profile, or an agent explicitly granted it
    guards: [single_statement, select_or_describe_only, no_settings_clause, no_into_outfile]
queries:          # overlay, pushed per project; `@scheduler.freshness` resolves here
  - id: scheduler.freshness
    project: companykit
    sql: "SELECT source, max(loaded_at) AS last FROM companykit.sync_runs WHERE loaded_at > {since:DateTime} GROUP BY source"
    params: { since: { type: DateTime, required: true } }
    limits: { max_rows: 500 }
```

Operation ids are stable; `policy_version` is the digest of the catalog plus overlay in force at call time
and is written on every receipt. WordPress operations map `wp_run{ability_name}` onto `content.read`
(`core/get-post`, `core/list-posts`, …), `content.draft` (`core/create-post` with `status=draft` forced),
`site.read`; anything else on `wp_run` is denied by name. Memory operations map `recall_memory` →
`memory.recall` and `store_memory` → `memory.store`, both namespace-enforced on the way in (tags forced) and on
the way out (results outside the namespace dropped, including graph expansion).

## 5. Adapter interface

```go
type Adapter interface {
    Connector() string
    Tools(ctx context.Context, g *auth.Grant) ([]mcp.Tool, error)                 // verbatim + additive edits
    Resolve(tool string, args json.RawMessage) (catalog.OperationID, Args, error) // pattern match + normalise
    Execute(ctx context.Context, call Call) (Result, error)                       // credential injected here only
}
type Call struct { Grant *auth.Grant; Op *catalog.Operation; Args Args; Account string; Verbose bool; IdempotencyKey string }
type Result struct { Compact any; Verbose any; Receipt map[string]any; Outcome Outcome } // completed|failed|outcome_unknown
```

Rules every adapter obeys (beat C §6): upstream credential loaded from the broker's secret file at the outbound
request only; host, scheme and port pinned; redirects refused; response shape allowlisted; auth headers, cookies
and provider error bodies redacted before they reach a receipt or the agent. Phase 1 adapters:

| Connector | Upstream | Credential custody | Hard limits |
|---|---|---|---|
| `clickhouse` | HTTP `localhost:18123`, `FORMAT JSONCompact`, `param_<name>` binding | a dedicated `broker_reader` user with a `readonly=1` settings profile and `GRANT SELECT` on the allowed databases (the audit found the steward's script defaulting to the admin password in code; this removes the need) | `max_result_rows`, `max_execution_time`, `max_memory_usage` set per user profile, not per request |
| `wp-mcp-router` | the Node router as a supervised stdio child of the broker (go-sdk client over CommandTransport), one process shared by every caller; the router's `sites.json` is read by the broker's user only | one child, restarted on exit, calls serialised per site | `site` must be in the grant; `ability_name` must map to an allowed operation; draft forced |
| `memory` | AutoMem HTTP (`POST /memory`, `POST /recall`, `/memory/by-tag`, `/health`) | `AUTOMEM_API_KEY` in the broker secret file | namespace tags forced on store, filtered on recall |

Beat D's answer on whether WordPress core exposes a REST route to run abilities directly decides whether P2
drops the Node child; P1 keeps the router because its catalog, per-site guard and `compact` flag already exist.

## 6. Audit receipts

SQLite `receipts`, append-only (insert-only triggers, AgentD/PERSIST style). Two rows per call: `intent`
written before the upstream request, `outcome` appended after. Columns follow beat C §8: `ts, grant_id,
agent_id, project_key, session_id, run_id, parent_grant_id, connector, operation, adapter_version, account,
args_digest, args_summary (redacted), policy_version, decision, deny_reason, approval_id, idempotency_key,
state (intent|completed|failed|outcome_unknown|denied), upstream_request_id, outcome_class, duration_ms,
request_bytes, response_bytes`. If the intent write fails the call is denied (all risk classes, not only
sensitive ones; the broker is small enough that an unwritable disk is an outage). PERSIST pulls
`GET /v1/receipts?since=<cursor>` on its tick into its own audit (the existing `mcp_tool_invocations` shape
plus the broker fields) and shows them on the Fleet agent page; the broker's table is the authoritative copy,
PERSIST's is the audit of record for the owner. No "immutable" claim; export anchor later.

## 7. Approvals (hand-off to judgement)

The broker never decides. An operation with `approval: exact_action` (none in P1; P2 `wp.publish`,
`gmail.send`, spend) does this:
1. Prepare the **action record**: provider, account, verb, exact target, recipients incl. BCC, amount/currency,
   content digest, diff summary, expiry, policy version. Store it `pending` with a single-use nonce.
2. Call PERSIST judgement with the broker's own application token (tier-2 application identity, minted once
   via `POST /api/v1/auth/application-tokens`, stored in the broker secret file): `judge({kind:
   'tool_approval', actor: {agent_id, session_id, project_key}, subject, payload: action record})`.
   Judgement decides with a cited precedent or holds to the inbox, exactly as it does for `ask_decision`.
3. Return `approval_required {approval_id}` to the agent. The agent re-calls with the same `idempotency_key`
   to poll; it sees only `pending | approved | denied | expired`.
4. PERSIST posts the decision to `/v1/approvals/:id` on the admin socket. The broker revalidates grant, expiry
   and policy, checks the payload digest still matches, consumes the nonce atomically, executes, receipts.
A model's statement that Daniel approved, text inside a retrieved document, or possession of the bearer never
creates an approval. Owner-client calls record `approval: owner_present` and proceed.

## 8. Compact and verbose responses

Default response per operation is the declared compact shape; `verbose: true` returns the upstream payload
untouched (still redacted of auth material). Compact shapes in P1:

- ClickHouse: `{query_id, rows, truncated, columns, preview: first 20 rows, elapsed_ms, bytes_read}`; `verbose`
  returns all rows up to the grant's `max_rows`.
- WordPress: `{site, ability, id, status, type, title, link, modified, content_sha256}`; `verbose` returns the
  router's result with `compact: true` (HAL noise stripped) unless `verbose` was asked for explicitly.
- Memory: `{ids, count, previews: 200 chars, scores}`; `verbose` returns full records.
- Upstream errors: class + first 300 chars, redacted. Receipts always carry sizes so the per-wake token effect
  (strategy "evidence nobody publishes") can be read from the table.

## 9. Acceptance (beat A's checklist mapped to tests) and phase-1 evidence

| Check | Test |
|---|---|
| Two agents, one backend | two grants call `clickhouse`; one broker process, one ClickHouse connection pool, one router child; revoking one leaves the other working |
| Distinct backend identities | grant for site A cannot `wp_run` on site B; error names the denial, not the sibling's catalog |
| Denied tool by exact name | `run_query` with raw SQL on a named-query grant; `wp_run core/update-post`; `store_memory` with a foreign namespace tag; all fail before any upstream call, receipted as `denied` |
| No control plane | front listener answers 404 on `/v1/*`; no route creates grants, edits the catalog, or touches secrets |
| Revoke mid-session | revoke → next call `BROKER_GRANT_REVOKED`; a new MCP session with the old bearer gets 401 |
| Dead upstream | ClickHouse stopped → `BROKER_UPSTREAM_UNAVAILABLE` on that connector; `memory` still answers |
| Audit completeness | every call has an intent and an outcome row, including denials and timeouts (`outcome_unknown`) |
| Steward end to end | a real wake with `.mcp.json` = `persist` + broker `clickhouse` + `memory`, zero other MCP processes, named queries answering the validity checks |
| Owner session | Daniel's Claude Code `clickhouse` entry pointed at the broker; same tool names; first-turn token count unchanged within noise |

Evidence to record in the fleet ledger: process count per agent and per backend, prompt tokens per wake with the
broker vs. before, denied-action attempts, revocation latency, receipt completeness under a forced failure.

## 10. Honest boundaries

- Same-UID agents without Claude Code's sandbox can read `~/.brokerd` and are not isolated from the admin
  socket or the secret file. The sandbox + the deny list cover Claude named agents today; Codex local agents are
  not covered (audit §5); Docker/AgentD remains the real boundary.
- Transparent pass-through means an agent still spends tokens on upstream tool schemas; the win is custody,
  policy and audit, plus compact responses. Discovery-style savings come later and must be measured (D4, beat B).
- The broker is one more always-on process holding credentials. It is smaller than nineteen, and it is the only
  one with receipts.
