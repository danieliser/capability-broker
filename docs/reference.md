# brokerd reference

The full behaviour, feature by feature. Start with the [README](../README.md).

## Features

- **Transparent MCP.** `GET|POST /mcp/clickhouse`, `/mcp/wp-mcp-router`, `/mcp/memory` are Streamable HTTP MCP
  endpoints. Sessionless Streamable HTTP (MCP 2026-07-28) by default, verified with Claude Code 2.1.316, Codex 0.166
  and the Go SDK client; `--stateless=false` keeps `Mcp-Session-Id` sessions for older clients. `tools/list` returns
  the upstream tool definitions filtered to what the caller's grant allows, plus one additive `verbose` flag.
- **Invisible to its owner.** Owner client tokens see each upstream exactly as if connected directly: tool
  definitions (annotations, output schemas, titles), results (all content blocks, `structuredContent`, `isError`)
  and the upstream's own instructions pass through untouched. Policy, compaction and the `verbose` flag apply to
  agent grants only. Prompts and resources are not passed through yet (logged when an upstream has any).
- **Grants, not credentials.** A caller holds a random bearer (`cbg_…` for agents, `cbc_…` for owner clients)
  bound to a capability set: named queries, sites, operations, namespaces, limits. Hashes at rest, revocation
  checked on every call, child grants can only narrow. Agent grants are minted by the control plane (PERSIST)
  over the admin socket and die with the session.
- **Operations as the policy key.** A tool call resolves to a stable operation id by tool name plus argument
  pattern (`run_query` with `@group.name` → `clickhouse.named_query`; `wp_run core/delete-post` → no operation,
  denied). The policy engine runs after argument enrichment, so an adapter cannot widen a call.
- **Tool-agnostic hosting.** Every connector is a stdio MCP server declared in `~/.brokerd/connectors.yaml`
  (command, args, env with `${secret:…}` references). The broker starts it once, discovers its tools and exports
  them verbatim; the child's environment is exactly what the connector declares plus `PATH` and `HOME`. Policy
  comes from the connector's catalog: `~/.brokerd/catalog.d/<name>.yaml` if you wrote one, else the catalog
  embedded for that name (`clickhouse`, `wp-mcp-router`, `memory`), else a synthesized one in which every tool is
  an operation of its own name that an agent grant must list. Catalogs refer to small named hooks (resolve a
  named query, force `status: draft`, force namespace tags, filter a recall by namespace, narrow a `site` enum)
  rather than carrying code.
- **Remote servers and sign-in.** A connector may be a remote Streamable HTTP server (`url:`, optional
  `headers:` with `${secret:…}`) instead of a command. `oauth: {}` signs the broker in with OAuth 2.1 (protected
  resource discovery, dynamic client registration, PKCE) once, with `brokerd auth <connector>`; the token and
  refresh token live in `~/.brokerd/oauth/<connector>.json` (0600) and every client and agent uses that one
  sign-in. A daemon never opens a browser by itself: without a token the connector reports
  `needs sign-in: brokerd auth <name>` and stops retrying. Remote calls use HTTP/1.1 and send a broker
  User-Agent, because Cloudflare bot protection challenges Go's default HTTP/2 client.
- **Shaping per connector.** `expose: broker` keeps a connector out of the client's preloaded tools: the broker
  entry offers `search_tools`, `describe_tool` and `call_tool` instead (progressive disclosure; Haiku 5.5 and GPT-6
  Luna at low effort found and used such a tool 3/3 each). `clients:` picks which owner clients get it;
  `tools: {include, exclude}` globs filter tools for every caller; `strip:` drops parts of tool definitions or
  results; `response:` reshapes results (`drop_keys`, `compact`, `max_chars`, or an external `command` filter).
  Client-side permission rules keyed to a tool name do not see calls made through `call_tool`; use
  `tools.exclude` for anything that must never run.
- **Owner-only connectors.** `owner_only: true` serves a connector to owner client tokens only; minting an agent
  grant that names it fails with `ADMIN_OWNER_ONLY`.
- **Hot reload.** `connectors.yaml`, `secrets.json` and `catalog.d/*.yaml` are polled every 2 seconds (also
  `SIGHUP`, `brokerd reload`). Added connectors start, changed ones restart, removed ones stop; a file that fails to
  parse changes nothing. Every connected MCP session gets `tools/list_changed`, so an open chat sees the change.
- **Aggregate endpoint.** `/mcp` carries every connector the grant reaches as `<connector>__<tool>`;
  `?exclude=a,b` leaves out connectors a client already has as their own servers. That is the entry that makes a
  newly added connector usable mid-chat. Verified with Claude Code 2.1.316 in both transport modes.
- **Pinned runners.** A connector run through `npx` or `uvx` is resolved to one version, installed under
  `~/.brokerd/runtimes/<connector>/`, and run from there, so a start never waits on the registry. The original
  command runs until the install exists. Pins are rechecked after `--pin-ttl` (24h); a newer version installs beside
  the old one (one previous kept) and swaps in by hot reload. `pin: false` on a connector opts out.
- **Relay.** Progress notifications from the child reach the calling client by token; sampling and elicitation
  requests are forwarded while exactly one call is in flight on that child (session mode only; the sessionless
  transport rejects server-initiated requests by design).
- **Receipts.** An append-only SQLite table with an `intent` row before every upstream call and an `outcome` row
  after, including denials. A failed intent write denies the call.
- **Compact by default.** Every response is trimmed rtk-style (long strings cut, arrays capped, deep trees pruned,
  hypermedia keys dropped, JSON-in-string documents opened) unless the operation declares explicit fields;
  `verbose: true` returns the upstream payload untouched.


## Operation constraints and acceptance

See [the operation map and runbook](operation-constraints.md) for all 17 embedded operations, declarative
argument rules, operation-specific grant values, daily counter semantics, row/deadline limits, the isolated
steward acceptance test, and exact smoke-memory cleanup (read-only by default).
