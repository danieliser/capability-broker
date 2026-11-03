---
name: brokerd-operations
description: Operate the capability broker (brokerd) that hosts MCP servers for every client and agent. Use when adding, moving, signing in, shaping or restoring an MCP server, when a tool is missing or behaves differently through the broker, or when deciding which agent may reach which server.
agents: [claude-code, codex]
license: MIT
tags: [mcp, brokerd, agents, permissions]
category: infrastructure
metadata:
  version: "1.0.0"
capabilities:
  network: true
  filesystem: readwrite
  tools: [Bash, Read, Edit]
---

# brokerd operations

brokerd is one local daemon (`127.0.0.1:8890`, admin socket in `~/.brokerd/`) that runs every MCP server once and
serves it to Claude Code, Codex, Cursor, OpenCode and Claude Desktop (owner tokens) and to named agents (per-wake
grants). Servers are configured in one place: `~/.brokerd/connectors.yaml` (hot-reloaded within 2s); credentials in
`~/.brokerd/secrets.json` (set with `brokerd secret set NAME`, value on stdin), referenced as `${secret:NAME}`.
To add a new server, use the `mcp-install` skill.

## When to use

- "Add this MCP server", "move X into the broker", "why don't I see tool Y", "sign the broker in to Z".
- A tool name, result or permission rule changed after a server moved into the broker.
- Giving a named agent access to a server, or limiting what a server exposes.

## Rules that matter

1. **Invisible for the owner.** Owner clients get one entry per server under its original name, pointed at the
   broker, and see tools, results, errors, instructions, prompts and resources exactly as the server sends them.
   Anything visibly different is a bug to fix in brokerd, not to work around.
2. **Agents get grants, not servers.** A named agent reaches only the servers listed under `capabilities:` in its
   agent YAML; PERSIST mints the grant at each wake and revokes it at session end. A new server never reaches an
   agent until it is added there. `owner_only: true` servers can never be granted.
3. **Carry client rules over.** Claude Code permission rules and Codex `approval_mode` are keyed by server name.
   `brokerd client adopt` warns when they exist; keep them matching.

## Workflow

### 1. See the state
```sh
brokerd status            # every connector: ok | needs sign-in | degraded (reason)
brokerd auth              # connectors waiting for an OAuth sign-in
tail -f ~/Library/Logs/capability-broker/brokerd.log
```

### 2. Add or move a server
- Already in a client config: `brokerd client adopt <name>[=<connector>] [--owner-only] [--dry-run]`. Secrets move
  to secrets.json, each client keeps a restorable copy, originals go to `~/.brokerd/clients/adopted.json`.
- New: add a block to `connectors.yaml` (`command`/`args`/`env`, or `url` + `headers`, or `url` + `oauth: {}`).
- Undo: `brokerd client restore <name>`.

### 3. Sign in (OAuth servers)
`brokerd auth <connector-or-prefix>` opens the browser once; the broker keeps and refreshes the token for every
client. `--force` signs in again, `--sign-out` forgets it.

### 4. Shape a server (all optional, per connector)
```yaml
expose: broker            # not preloaded; reached via search_tools / describe_tool / call_tool on the broker entry
on_demand: true           # not started until called (tools listed from cache); idle_stop: 10m stops it again
clients: [claude-code]    # or [!cursor]
tools: { exclude: ["delete_*"] }   # hidden and refused for every caller (the hard stop)
strip: [outputSchema, annotations, meta, prompts, resources, instructions, structured]
response: { drop_keys: [debug], compact: true, max_chars: 20000, command: [jq, -c, .], tools: ["get_*"] }
```
`expose: broker` saves context; low-effort Haiku and Luna found and used on-demand tools 3/3. Calls made through
`call_tool` are not matched by client per-tool permission rules, so use `tools.exclude` for anything that must
never run.

### 5. Verify
```sh
brokerd-probe -raw -url http://127.0.0.1:8890/mcp/<connector> -token "$(cat ~/.brokerd/clients/claude-code.token)"
brokerd-probe -url http://127.0.0.1:8890/mcp/<connector> -token ... -tool <tool> -args '{...}'
```

## Anti-patterns

- Editing a client's MCP config by hand for a brokered server: the broker rewrites managed entries.
- Granting an agent `*` or adding a server to every agent "to be safe".
- Restarting the broker to apply config: `connectors.yaml` and `secrets.json` hot-reload.
- Printing tokens or secrets; read them from files into variables.
