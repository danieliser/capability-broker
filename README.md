# capability-broker (`brokerd`)

One always-on Go daemon that holds the credentials your AI agents must never see, and exposes each upstream
service to them as an ordinary MCP server, with the upstream tool names and schemas unchanged. Policy,
per-agent grants, approvals and receipts sit behind that URL, invisible to the agent.

```
agent / Claude Code / Codex  ──HTTP MCP──▶  brokerd :8890/mcp/<connector>  ──credential injected here──▶  upstream
                                              │ grant → operation → policy → receipt
                              PERSIST ──unix socket admin──▶ grants, catalog, receipts
```

Built for a single-owner Mac where several headless agents (PERSIST named agents) and the owner's own
interactive sessions share ClickHouse, a WordPress fleet and a memory service. The design and evidence live in
`docs/`.

## What it does

- **Transparent MCP.** `GET|POST /mcp/clickhouse`, `/mcp/wp-mcp-router`, `/mcp/memory` are Streamable HTTP MCP
  endpoints. Sessionless Streamable HTTP (MCP 2026-07-28) by default, verified with Claude Code 2.1.316, Codex 0.166
  and the Go SDK client; `--stateless=false` keeps `Mcp-Session-Id` sessions for older clients. `tools/list` returns
  the upstream tool definitions filtered to what the caller's grant allows, plus one additive `verbose` flag.
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
- **Hot reload.** `connectors.yaml`, `secrets.json` and `catalog.d/*.yaml` are polled every 2 seconds (also
  `SIGHUP`, `brokerd reload`). Added connectors start, changed ones restart, removed ones stop; a file that fails to
  parse changes nothing. Every connected MCP session gets `tools/list_changed`, so an open chat sees the change.
- **Aggregate endpoint.** `/mcp` carries every connector the grant reaches as `<connector>__<tool>`;
  `?exclude=a,b` leaves out connectors a client already has as their own servers. That is the entry that makes a
  newly added connector usable mid-chat. Verified with Claude Code 2.1.316 in both transport modes.
- **Relay.** Progress notifications from the child reach the calling client by token; sampling and elicitation
  requests are forwarded while exactly one call is in flight on that child (session mode only; the sessionless
  transport rejects server-initiated requests by design).
- **Receipts.** An append-only SQLite table with an `intent` row before every upstream call and an `outcome` row
  after, including denials. A failed intent write denies the call.
- **Compact by default.** Every response is trimmed rtk-style (long strings cut, arrays capped, deep trees pruned,
  hypermedia keys dropped, JSON-in-string documents opened) unless the operation declares explicit fields;
  `verbose: true` returns the upstream payload untouched.

## Install (macOS)

```sh
git clone https://github.com/danieliser/capability-broker ~/Toolkit/capability-broker
cd ~/Toolkit/capability-broker
./install.sh            # builds with a stamped identity, installs ~/.local/bin/brokerd, loads launchd
brokerd status          # admin socket + token under ~/.brokerd
```

Secrets go in `~/.brokerd/secrets.json` (mode 0600), a flat object the connectors reference as `${secret:key}`:

```json
{ "clickhouse_password": "…", "automem_api_key": "…" }
```

The installer seeds `~/.brokerd/connectors.yaml` from `connectors.example.yaml`; edit commands and env for your
upstreams. Daemon settings are `BROKERD_PORT`, `BROKERD_HOST` (loopback only) and `BROKERD_DATA_DIR`.

## Use

```sh
# a project's named ClickHouse queries (versioned with the repo)
brokerd queries push --project companykit --file .persist/broker/queries.yaml

# an agent grant (PERSIST does this per wake; the CLI is for tests)
brokerd grant mint --agent my.agent --session s1 --project companykit --capabilities caps.json --ttl 1h
brokerd grant list
brokerd grant revoke <grant-id>

# your own sessions: Claude Code, Codex, Cursor and OpenCode, each with its own owner token
brokerd client install all                # or claude-code | codex | cursor | opencode
brokerd client install all --revert       # restore the original entries

# apply config changes now instead of within 2s
brokerd reload

# receipts
brokerd receipts --limit 50

# ad-hoc MCP calls from the shell
brokerd-probe -url http://127.0.0.1:8890/mcp/clickhouse -token "$TOKEN" -tool run_query -args '{"query":"@scheduler.freshness"}'
```

A capabilities file:

```json
{
  "clickhouse": { "queries": ["scheduler.freshness"], "limits": { "max_rows": 2000, "calls_per_run": 40 } },
  "wp-mcp-router": { "sites": ["wppopupmaker"], "operations": ["content.read", "content.draft"],
                     "constraints": { "post_types": ["post"] }, "limits": { "max_drafts_per_run": 3 } },
  "memory": { "namespaces": ["agent:my.agent", "project:companykit"], "operations": ["recall", "store"] }
}
```

## Layout

```
cmd/brokerd            daemon + CLI subcommands        pkg/mcpfront      per-connector MCP servers
cmd/brokerd-probe      shell MCP client                pkg/api           front mux, admin socket, admin client
pkg/auth               tokens, grants, capabilities    pkg/adapters/*    clickhouse, wordpress, memory
pkg/catalog            operations, embedded catalogs   pkg/audit         receipts
pkg/connectors         connectors.yaml                 pkg/compact       response trimming
pkg/adapters/upstream  stdio child hosting, hooks, relay
pkg/policy             the single authorization point  pkg/durable       SQLite schema
```

## Development

```sh
go test ./...
go test -race ./pkg/mcpfront ./pkg/auth
go vet ./... && gofmt -l .
```

Releases: open a `release/X.Y.Z` branch whose `CHANGELOG.md` top entry is `## [X.Y.Z] - date`, raise a PR into
`main`; the gate checks branch, changelog and build identity; merging publishes the tag and a GitHub release built
from the merge commit.

## Boundaries

Same-UID processes that are not sandboxed can read `~/.brokerd`; the broker protects credentials from agents that
run inside Claude Code's OS sandbox or a container, not from an unrestricted owner shell. It does not claim
resistance to owner or root compromise.

## License

MIT
