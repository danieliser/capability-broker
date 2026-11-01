# Changelog

Format follows [Keep a Changelog](https://keepachangelog.com/); versions are semver tags on `main`.

## [Unreleased]

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
