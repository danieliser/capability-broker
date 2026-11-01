# Agent evaluation: does tool passthrough hold up with low-effort models?

Run 2026-11-01 against the live broker (ClickHouse warehouse, AutoMem, WordPress fleet). Harness:
`eval.py` in the operator's workspace (not shipped). Each run is a fresh headless session with only broker MCP
servers and no built-in tools (Claude) or an isolated Codex home with skills, plugins, apps and memories off.
Answers are graded against live data; memory writes are graded from the broker's own receipts.

**Models:** Claude Haiku 5.5 (`--effort low`) and GPT-6 Luna (`model_reasoning_effort=low`).
**Setups:** `servers` (one MCP server per connector), `single_prefixed` (one `broker` server, `clickhouse__run_query`),
`single_plain` (one `broker` server, upstream names, `?prefix=none`).
**Tasks (no tool hints):** EDD order count; the table holding Stripe charges; recall a stored memory; store a
memory; list WordPress sites; count Stripe charges then store the number with a marker.

## Results

| Model | Condition | servers | single_prefixed | single_plain |
|---|---|---|---|---|
| Haiku 5.5 low | default (two rounds, 90 runs) | 30/30 | 30/30 | 30/30 |
| Luna low | clean, no help | 14/18 | 13/18 | 14/18 |
| Luna low | + live hint in every tool description | | | 13/18 |
| Luna low | + one sentence of standing instructions | | | **18/18** |

- **Setup and prefix do not matter.** Within noise, both models do as well through one `broker` server, with or
  without connector prefixes, as through per-connector servers. Single mode with plain names is the default.
- **Luna's failures are all "answered without trying a tool"** ("read-only workspace", "no memory of that in this
  conversation"); none were wrong tool choices or bad arguments. Recall of a stored memory was 0/9 without help.
- **Claude Code shows MCP server instructions to the model; Codex does not.** The broker's server instructions
  therefore help Claude clients only. A live hint in tool descriptions did not help Luna.
- **One sentence fixed it:** "You have live tools for the data warehouse, long-term memory and WordPress sites; use
  them when a task needs data, remembered facts or site information." PERSIST now prepends this to every agent that
  holds broker grants.
- With a shell available, Haiku used `date` instead of a newly added time tool in 6/6 runs: models prefer familiar
  commands over new MCP tools when both work.

## What invalidated earlier rounds (kept for the record)

1. Claude's permission rule `mcp__*` does not match MCP tools; every call was blocked. Use `mcp__<server>`.
2. Codex's apps feature added a ChatGPT-account memory connector that demanded approval, even with
   `--ignore-user-config`. Disable `apps` (and `plugins`, `memories`).
3. Codex loaded the operator's personal skills from `~/.codex/skills`, which nudged Luna to use memory tools. Use an
   isolated `CODEX_HOME` with skills off, as PERSIST does for named agents.

## Limits

Two to three runs per cell, one task suite, two models, owner-scope grants. 107 eval memories were deleted after the
runs (found through receipts).
