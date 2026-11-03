---
name: mcp-install
description: Install or set up MCP servers through brokerd so every app (Claude Code, Codex, Cursor, OpenCode, Claude Desktop) and every agent gets them from one place. Use when asked to install, add, connect or configure an MCP server, when setting up brokerd on a new machine, or when giving a fleet of agents their own tool sets.
agents: [claude-code, codex]
license: MIT
tags: [mcp, brokerd, install, agents]
category: infrastructure
metadata:
  version: "1.0.0"
capabilities:
  network: true
  filesystem: readwrite
  tools: [Bash, Read, Edit]
---

# Install MCP servers through brokerd

Never add an MCP server to an app's own config (`~/.claude.json`, `~/.codex/config.toml`, Cursor, OpenCode, Claude
Desktop). Add it once to the broker; the broker writes every app's entry and serves every agent.

## When to use

- "Install / add / connect the X MCP", "set up MCP on this machine", "give agent Y access to Z".
- An MCP server's README tells you to edit a client config: translate that into a broker connector instead.

## 0. Is brokerd installed?

```sh
brokerd status || (git clone https://github.com/danieliser/capability-broker ~/Toolkit/capability-broker \
  && cd ~/Toolkit/capability-broker && ./install.sh && brokerd client install all)
```
`client install all` gives every app one entry per server under its original name. If apps already have MCP
servers, move them in: `brokerd client adopt <name>... --dry-run`, then without `--dry-run`.

## 1. Find how the server runs

From its README: a command (`npx -y pkg`, `uvx pkg`, a binary) or a URL (remote MCP). Note the env vars or headers
it needs and whether the URL uses OAuth (a 401 without a token means OAuth).

## 2. Put secrets in the broker, not in config

Ask the user to run this in their own terminal and paste the key when prompted (the agent never sees it):
```sh
brokerd secret set NEWTOOL_API_KEY
```
A key already in the Keychain can be piped without the agent reading it:
`security find-generic-password -s <item> -w | brokerd secret set NEWTOOL_API_KEY`. `brokerd secret list` shows
names only.
Never echo a secret, never write one into connectors.yaml or an app config.

## 3. Add the connector to `~/.brokerd/connectors.yaml`

```yaml
connectors:
  newtool:                         # lowercase; becomes the server name apps see
    command: npx                   # npx/uvx are pinned to one installed version automatically
    args: [-y, newtool-mcp]
    env: { NEWTOOL_API_KEY: "${secret:NEWTOOL_API_KEY}" }
  remotetool:
    url: https://mcp.example.com/mcp
    oauth: {}                      # then: brokerd auth remotetool (opens the browser once)
```
Choose how it loads:
- Used often: leave the defaults (started at boot, tools preloaded in apps).
- Used rarely: `on_demand: true` (not started until called; tools listed from cache; stops after `idle_stop`, 10m).
- Large tool list: `expose: broker` (not preloaded in apps; reached through `search_tools` / `call_tool`).
- Personal data (mail, banking): `owner_only: true` (never granted to agents).
- Only some apps: `clients: [claude-code, codex]`. Dangerous tools: `tools: { exclude: ["delete_*"] }`.

The file hot-reloads within 2 s; open chats get the new tools through their `broker` entry, new chats as their
own server.

## 4. Verify

```sh
brokerd status                                   # newtool: ok (or "needs sign-in: brokerd auth newtool")
brokerd-probe -url http://127.0.0.1:8890/mcp/newtool -token "$(cat ~/.brokerd/clients/claude-code.token)"
```
Call one read-only tool and check the result before saying it is installed.

## 5. Give agents their own tools (fleets)

Agents do not get servers; they get grants. Add the connector to each agent's `capabilities:` (PERSIST agent YAML),
listing only what that agent needs:
```yaml
capabilities:
  newtool: { operations: [newtool_search, newtool_get] }
  clickhouse: { queries: [scheduler.freshness] }
```
The grant is minted at the agent's next wake and revoked when it ends. No per-agent MCP config exists to maintain.

## Anti-patterns

- Editing an app's MCP config directly, or running a second copy of a server per app.
- Putting a key in `args` or a URL query string.
- Adding a new server to every agent "just in case"; grants are per agent, least privilege.
- Restarting the broker to apply config (it hot-reloads).
