# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/); versions are semver tags on `main`.

## [Unreleased]

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
