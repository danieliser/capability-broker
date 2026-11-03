# Security

## Reporting

Report a vulnerability privately through GitHub: **Security → Report a vulnerability** on this repository. Please
do not open a public issue for it. You will get an answer within a few days.

## What brokerd protects, and what it does not

brokerd holds the credentials for every MCP server on the machine so that apps and agents never need them. Its
guarantees are scoped to that job.

**It does:**

- **Listen on loopback only.** The front refuses to start on a non-loopback address. Every MCP request needs a
  bearer token (`cbc_…` for your own apps, `cbg_…` for agent grants). The broker's database stores only token
  hashes; your apps' tokens are also kept in `~/.brokerd/clients/` (0600) so the broker can write them into app
  configs, which therefore contain them too.
- **Refuse requests from web pages on other sites** (an `Origin` header from an http/https page that is not
  localhost), so a page cannot reach the broker through DNS rebinding.
- **Keep the control plane off the network.** Grants, catalogs, receipts, sign-in and profiles are only on a Unix
  socket (`~/.brokerd/admin.sock`, mode 0600) that also requires an admin token (`admin.token`, 0600).
- **Keep keys out of configs and processes that do not need them.** `~/.brokerd` is 0700; `secrets.json`, OAuth
  tokens (`oauth/`), client tokens and the records of adopted servers are 0600. Each MCP server's process gets
  only the environment its connector declares, plus `PATH` and `HOME`.
- **Narrow what agents reach.** An agent grant names its servers, queries, sites and operations; it can only
  narrow when delegated, is checked on every call, is revoked when the agent's session ends, and can never name
  an `owner_only` server. A receipt row is written before every upstream call (a failed write refuses the call)
  and another after it.
- **Sign in to remote servers with OAuth 2.1 + PKCE**, accepting the browser's callback only for a sign-in that
  `brokerd auth` started (the `state` must match), and never opening a browser on its own.

**It does not:**

- **Protect against your own user account.** Any unsandboxed process running as you can read `~/.brokerd`. The
  broker keeps credentials away from agents that run inside an OS sandbox or a container, not from an
  unrestricted shell under your user, and not from root.
- **Apply your apps' per-tool permission rules to on-demand calls.** For a server with `expose: broker`, an app
  sees one `call_tool` tool; its permission rules match tool names, not the arguments that pick the real tool.
  Use `tools: { exclude: [...] }` for anything that must never run: it is hidden and refused for every caller.
- **Vet the MCP servers you add.** A server runs with the environment you give it. `response.command` runs the
  command you configure on every matching result.
- **Keep receipts free of arguments.** Receipts store a hash of each call's arguments and a 400-character
  summary; a query or search text can appear there. `~/.brokerd/broker.db` is 0600.

## Supported versions

Only the latest release gets fixes.
