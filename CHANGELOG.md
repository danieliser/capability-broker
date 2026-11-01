# Changelog

## 0.1.0 — 2026-11-01

Phase 1 of the capability broker (design: `~/Projects/skill-workspaces/agent-fleet/2026-11-01/capability-broker-design.md`).

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
