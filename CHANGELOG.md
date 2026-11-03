# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/); versions are semver tags on `main`.

## [Unreleased]

## [0.10.0] - 2026-11-03

- `on_demand: true` keeps a server stopped until a call needs it; its tools, prompts, resources and instructions are listed from the last discovery (`~/.brokerd/cache/`). `idle_stop` (default 10m for on-demand servers) stops a started server after a quiet period; the next call starts it again.
- Memory: a soft heap limit (48 MiB) and `GOGC=50` unless the environment sets its own; cached per-client servers idle for 30 minutes are dropped. Broker RSS 70–75 MB → 55–60 MB.
- `brokerd secret set|list|rm <NAME>`: keys into secrets.json from stdin, never argv or logs.
- Heap and CPU profiles on the admin socket (`/debug/pprof/heap`, `/debug/pprof/profile`).
- `skills/mcp-install`: install MCP servers through the broker, set up a machine, give fleet agents their own tools.
- Fix: a deadlock when an on-demand server saved its discovery cache during its first connect.

## [0.9.0] - 2026-11-03

- Owner clients get each upstream's prompts, resources and resource templates, as if connected directly (`strip: [prompts]` / `[resources]` hides them).
- Upstream list changes are followed: a server that adds or removes tools, prompts or resources at runtime announces it, the broker re-reads it, and connected clients get `list_changed`. Before, such changes were invisible until a restart.
- `cmd/mockmcp`, a stdio server exercising every passed-through feature (annotations, output schemas, structured, image and error results, instructions, prompts, resources, templates, runtime list changes), and `test/e2e` running the broker against it.
- WordPress drafts admit only the supported `core/create-post` and `gk-block-mcp/create-post` schemas, with draft status forced and post type/site/run limits checked. Update abilities no longer resolve as drafting: forcing draft on an update could unpublish a live post.
- Block MCP `resolve-url` maps to `content.read`; `get-page-blocks` forces `persist_refs: false` so a read cannot silently persist block references.
- `skills/brokerd-operations`: an agent skill for running the broker.
- `brokerd client install` defaults to `--mode servers`: one entry per server under its original name, so apps see no difference.
- `?ondemand=all` on the aggregate endpoint serves every connector through discovery (benchmarks, A/B tests).
- Benchmarks (`docs/benchmarks.md`, `bench/`, `test/e2e` benchmarks): memory 2,424 MB → 645 MB with every app open, ≈0.6 ms added per call, 1.05 MB of tool definitions → 1.6 KB on demand.
- README rewritten around what the broker does for an owner; the full feature list moved to `docs/reference.md`.

## [0.8.0] - 2026-11-02

- `expose: broker` keeps a connector inside the broker for owner clients: no client entry and no preloaded tools, found and run through three discovery tools on the broker entry (`search_tools`, `describe_tool`, `call_tool`). `expose: passthrough` (default) keeps its own entry. Measured: Haiku 5.5 low 3/3 and GPT-6 Luna low (Codex, which drops server instructions) 3/3 found and used an on-demand Metabase tool.
- `clients:` limits which owner clients get a connector (`[claude-code, codex]`, `[!cursor]`).
- `tools: {include, exclude}` globs filter a connector's tools for every caller; excluded tools are hidden and refused.
- `strip:` removes `outputSchema`, `annotations`, `title`, `icons`, `meta`, `instructions` or `structured` (structuredContent) from what clients see.
- `response:` filters results for every caller: `drop_keys`, `compact` (with `max_string`, `max_items`), `command` (an external filter: MCP result JSON on stdin, result or text on stdout; failure fails the call), `max_chars`, optionally limited to `tools` globs.
- `brokerd auth <prefix>` signs in every connector starting with it (`brokerd auth cloudflare`); `brokerd auth` lists connectors that need sign-in.

## [0.7.1] - 2026-11-02

- Per-server client entries (`--mode servers`) use the name the client knew a server by when it was adopted under a different connector name (`openaiDeveloperDocs` → connector `openai-docs`), so tool names and rules keyed to them are unchanged. Owner clients default to this mode for full transparency; agents keep the single `broker` server.

## [0.7.0] - 2026-11-02

- Owner clients see upstream servers as if connected directly: tool definitions verbatim (title, annotations such as `readOnlyHint`/`destructiveHint`, output schema, icons, `_meta`), results verbatim (every content block, `structuredContent`, upstream `isError`), arguments passed through untouched, and each upstream server's own MCP instructions instead of the broker's. Agents keep policy, compaction and the `verbose` flag.
- A server's instructions follow its connectors: when an upstream finishes starting (or is added), new sessions get the updated instructions.
- `brokerd-probe -raw` prints instructions, tool definitions and results as JSON.
- The broker logs upstreams that offer prompts or resources, which it does not pass through yet.

- WordPress drafts admit only the supported `core/create-post` and `gk-block-mcp/create-post` schemas, with draft status forced and post type/site/run limits checked. Update abilities no longer resolve as drafting: forcing draft on an update could unpublish a live post.
- Block MCP `resolve-url` maps to `content.read`; `get-page-blocks` forces `persist_refs: false` so a read cannot silently persist block references.

## [0.6.2] - 2026-11-02

- Codex per-tool settings under the broker entry (`[mcp_servers.broker.tools."<tool>"] approval_mode`) survive client syncs; they were rewritten away with the entry.
- `brokerd client adopt` warns when Claude Code permission rules or Codex approval settings name the adopted server: those rules stop matching once its tools arrive through the broker.

## [0.6.1] - 2026-11-02

- OAuth token refresh uses the broker's HTTP/1.1 client; Cloudflare-fronted token endpoints refused the default client, so a sign-in died at its first access-token expiry.

## [0.6.0] - 2026-11-02

- Remote connectors: `url:` plus `headers:` serves any Streamable HTTP MCP server through the broker.
- OAuth sign-in for remote connectors: `oauth: {}` and `brokerd auth <connector>` (discovery, dynamic client registration, PKCE, refresh). One sign-in serves every client and agent; tokens live in `~/.brokerd/oauth/` (0600). A connector without a token reports `needs sign-in` and stops retrying instead of opening a browser.
- `brokerd client adopt <server>...` moves servers out of every client config into the broker (secrets to `secrets.json`, mcp-remote wrappers to native url connectors, 401 servers to oauth), leaving a restorable copy in each client; `brokerd client restore <server>...` puts them back.
- Claude Desktop is a client (`brokerd client install claude-desktop`), reaching the broker through mcp-remote.
- `owner_only: true` keeps a connector out of agent grants (`ADMIN_OWNER_ONLY`).
- Remote calls use HTTP/1.1, a broker User-Agent, and an `Mcp-Protocol-Version` header on `initialize`; Cloudflare-fronted servers refused Go's defaults with 403.

## [0.5.0] - 2026-11-01

- Agent evaluation in `docs/eval.md`: Haiku 5.5 and GPT-6 Luna at low effort, three client setups, six tasks. Setup and prefix do not matter; Codex agents need one sentence saying the tools are live.
- A grant mints one `broker` server with upstream tool names; per-connector URLs are returned in `connector_servers`.

- Default install is one `broker` server with upstream tool names (`--prefix none`); measured equal to per-connector servers for Haiku 5.5 and GPT-6 Luna at low effort.
- MCP server instructions name the live connectors behind each server (Claude Code shows them to the model; Codex drops them). `?instructions=none` and `?hint=live` exist for A/B tests.

- `brokerd client install --mode single|servers --prefix connector|none`. Single mode (default) gives each client one `broker` server whose tools pass straight through; `/mcp?prefix=none` keeps upstream tool names and prefixes only names two connectors share.

## [0.4.0] - 2026-11-01

- Installed clients are kept in step: adding or removing a connector updates every managed client's config (its own entry plus the `broker` exclude list), so a connector added mid-chat is its own server from the next chat on. Unchanged configs are not rewritten; a config that changes while being edited is retried.

## [0.3.0] - 2026-11-01

- npx/uvx connectors are pinned: the version is resolved once, installed under `~/.brokerd/runtimes`, and run directly; rechecked after `--pin-ttl` (24h); `pin: false` opts out. `pins.json` records what runs.
- launchd `ProcessType` is `Standard`; `Background` throttled every MCP child (starts were 10 to 35 times slower).
- Restart: health answers about 0.25s after a kill and pinned children connect within about 1.5s. Shutdown waits only for in-flight tool calls; children get a 1s grace.
- A child that fails to start is retried with backoff until it connects; replaced children are closed in the background so a reload never waits on them.

## [0.2.0] - 2026-11-01

- Hot reload of `connectors.yaml`, `secrets.json` and `catalog.d` (2s poll, `SIGHUP`, `brokerd reload` / `POST /v1/reload`); live sessions receive `tools/list_changed`. A connector added while a Claude Code session is running is usable in that session, verified in sessionless and session modes.
- `/mcp` aggregate endpoint (`<connector>__<tool>`, `?exclude=`); every server advertises `tools{listChanged}` even when empty.
- `brokerd client install <claude-code|codex|cursor|opencode|all>` with per-client owner tokens, per-connector entries plus one `broker` aggregate entry, backups under `~/.brokerd/clients`, lossless `--revert`; Codex `config.toml` is edited table by table.
- Owners reach `update_memory`, `delete_memory` and `wp_run_across`; agents need the explicit `update`, `delete` or `run_across` operation, with namespace checks.
- A client connect waits at most 5s on a cold child; tools arrive by `tools/list_changed`. Shutdown is bounded (2s drain, children closed in parallel): a restart takes about 4s instead of up to 70s.

## [0.1.0] - 2026-11-01

- Tool-agnostic hosting: every connector is a stdio MCP server from `~/.brokerd/connectors.yaml`; tools are discovered and exported verbatim; policy from `~/.brokerd/catalog.d/<name>.yaml`, else the embedded catalog, else a synthesized one. The native ClickHouse, WordPress and AutoMem adapters are gone; the stock `mcp-clickhouse`, `wp-mcp-router` and `mcp-automem` servers run as children with only their declared env.
- Relay of progress (by token), sampling and elicitation (single in-flight call) from a child to the calling client.
- rtk-style trimming of every compact response; JSON documents nested in string values are opened first.
- `delete_memory` and `update_memory` are never exported; batch and supersede stores and id-fetch recalls are blocked for agent grants.
- Health checks use tools/list (FastMCP servers reject the SDK ping) and never wait on a starting child.
- Installer survives a changelog with no released entry and seeds `connectors.yaml`.

- Sessionless Streamable HTTP (MCP 2026-07-28) is the default transport, verified with Claude Code 2.1.316, Codex 0.166 and the Go SDK client; `--stateless=false` keeps `Mcp-Session-Id` sessions for older clients.

Phase 1 of the capability broker (design: `docs/design.md`).

- `brokerd` daemon: loopback Streamable HTTP MCP endpoint per connector (`/mcp/clickhouse`, `/mcp/wp-mcp-router`,
  `/mcp/memory`), admin surface on a Unix socket, SQLite grants + append-only receipts, launchd install.
- Grants: agent grants (PERSIST-minted, session-bound, ≤24 h, child grants only narrow) and owner client tokens.
- Catalog: verbatim upstream tools, operations as policy keys, per-project named ClickHouse queries pushed as an overlay.
- Policy: capability requirements, named-query allowlists, site/post-type/namespace constraints, per-run counters,
  importance clamps, enrichment before evaluation.
- Adapters: ClickHouse (HTTP, dedicated read-only user, `param_` binding), WordPress (supervised `wp-mcp-router`
  child, draft forced), AutoMem (namespace tags forced on store, scoped recall, ownership checks on associate).
- Responses compact by default; `verbose: true` returns the upstream payload.
- CLI: `brokerd status | client mint/install | grant mint/list/revoke | queries push/list | receipts`; `brokerd-probe`.
