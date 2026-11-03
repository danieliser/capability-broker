# Rust full-port attempt — incomplete

This branch is **not at 1:1 parity and is not a drop-in replacement**. It extends
71eb685's experiment after merging Go main f9fda6d (v0.10.0), then refreshed to c97bbfa (v0.10.1). The measurements below
compare the current Rust subset with the complete Go v0.10.1 daemon; they cannot determine
how a full Rust port would stack up. In particular Rust does not perform Go's grant
lookup, policy/catalog processing or full receipt serialization. Do not deploy it.

## Parity inventory

| Area | Current Rust behavior | Gaps against Go main |
|---|---|---|
| Stdio | Same command/args/cwd, PATH/HOME plus declared env; secret/env substitutions; startup and call timeouts | Sequential startup; no retry/backoff supervision; calls serialized per connector; no progress/sampling/elicitation relay |
| Remote HTTP | URL + resolved headers, HTTP/1.1, broker User-Agent, MCP protocol header including initialize; tested against local HTTP fixture | Header protocol version is fixed at 2025-11-25; no live provider verification |
| OAuth | Explicitly rejects OAuth config rather than silently skipping it | Discovery, DCR, PKCE, sign-in CLI, refresh, Go-compatible token store all missing; rejection currently rejects the entire config |
| npx/uvx pins | Runs original command | No version resolution/install/update/TTL/rollback or pins.json |
| Reload | Two-second poll and SIGHUP; parse/validate before swapping; unchanged connectors reused; additions/removals reflected on next list | No admin reload, catalog.d overlays, managed client sync, downstream list_changed broadcast; changed connector startup can delay swap |
| Lifecycle | on_demand, default 10m idle_stop, disk cache under cache/name.json; verified child exit and wake | No saved_at timestamp in cache; cache/startup edge cases not fully ported; no failed-child backoff |
| Upstream discovery changes | MCP 2026 subscriptions/listen; legacy callbacks; refresh tools/prompts/resources on notifications | Refresh maintenance polls every 2s; no automatic subscription recovery; downstream push notifications not implemented |
| Owner tools/results | Go SDK smoke compares all six definitions and five result shapes against direct mock (structured, image, markdown, upstream isError) | SDK representation, not byte-identical JSON; protocol errors wrapped as internal errors |
| Prompts/resources/templates | Per-connector list/get/read and instructions pass smoke | Aggregate lists do not namespace collisions; get/read requires exactly one selected connector; no full aggregate routing |
| Shaping | Tool include/exclude enforced on calls; strip tool fields/instructions/structured/prompts/resources; response drop_keys/compact/command/max_chars/tools | Compaction edge/default behavior not fully ported; no complete differential corpus; command path not in e2e smoke |
| Discovery tools | expose: broker; search_tools, describe_tool, call_tool; ondemand=all | Search/ranking/results/schema/instruction text differ from Go; describe_tool returns unstripped schema; no client targeting identity |
| URL options | prefix=connector (default) / none, exclude, ondemand=all, instructions=none | hint=live missing; collision edge cases and full instruction construction differ |
| Clients | Connectors with clients: are hidden from fixture caller (fails closed) | No owner-client identities or actual clients allow/exclude behavior |
| Front transport | Sessionless MCP only, explicit loopback port | --stateless=false/session mode, subscriptions to downstream clients and CLI flags missing |
| Grants/policy | One static fixture owner token only | Hashed grant tokens, owner/agent kinds, capabilities, narrowing, revocation, TTL, owner_only grant rejection, embedded/overlay/synthesized catalogs, WP/memory constraints, counters and named ClickHouse queries missing |
| Agent compaction | Connector response compaction only | No agent grant front, operation-specific fields or verbose semantics |
| Receipts | WAL/FULL SQLite intent/outcome (Go v0.10.1 uses NORMAL + batched outcomes), append-only triggers; failed intent denies call | Incompatible MVP schema; no Go grant identity, argument hashes/summaries, sizes, policy decisions or denial receipts; do not point it at Go's database |
| Admin / CLI / clients | Explicit DATA_DIR OWNER_TOKEN_FILE PORT launch retained | Unix admin API and every requested CLI command (status/grant/queries/receipts/reload/auth/secret/client install/adopt/restore), Codex TOML, Claude Desktop and managed sync missing |
| Tests | Rust config/filter units, owner/lifecycle and local HTTP smoke against Go mock; original Go suite passes | Not a port of the complete Go e2e suite; no Rust policy/grants/client editing tests because those modules are absent |

## Methods

Measured 2026-11-03 on Apple M2 Max, macOS 26.5.2 arm64; rustc/cargo 1.97.1, Go 1.26.5.

- `cargo build --release --locked --manifest-path rust/Cargo.toml`; stripped thin-LTO
  Rust binary with bundled SQLite. Go: `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w'`.
  Go source under cmd/ and pkg/ is identical to main c97bbfa.
- `python3 rust/bench/run.py`: Go port 8990, Rust 8991, each with a separate private
  data directory and HOME. Both serve the same freshly built Go cmd/mockmcp.
  One shared Go SDK client/HTTP transport calls echo sequentially: three batches
  of 2,000 calls. Latency is median of the three per-batch medians. RSS is daemon
  `ps rss` only, excludes all child/server/client processes. After-call RSS sampled
  immediately after first 2,000 calls; no forced GC. All 6,000 calls are checked for
  success, and receipt counts are checked. Startup is launch to successful SDK
  discovery (three trials), includes subprocess polling/client overhead.
- `python3 rust/bench/shape.py`: 50 independent cmd/mockmcp connectors, six tools
  each = 300 definitions. This tests connector-count overhead with repeated small
  mock schemas, **not** 300 unique production schemas or the documented 1.05 MB
  production schema corpus. RSS is measured immediately after all 300 are listed.
- `python3 rust/bench/full_smoke.py`: per-connector prompts/resources/templates,
  runtime additions, failed-intent denial, process idle exit/restart, cached listing,
  invalid reload preservation, SIGHUP/poll reload, discovery and shaping.
- `python3 rust/bench/remote_smoke.py`: local fake remote checks HTTP/1.1, broker UA,
  secret header substitution and protocol header on initialize. No real sign-in or
  provider credentials used.
- `python3 rust/bench/cross.py`: actually links release targets, saves full errors.
  cargo-zigbuild, Zig and cross were absent (`cargo zigbuild --version` fails).
  Linux builds require a target C compiler/sysroot for bundled SQLite plus linker,
  or installation of cargo-zigbuild **and** Zig (or cross + Docker/target images).
  Windows additionally needs Unix-only permissions/SIGHUP code ported. These tools
  were not installed and no containers were started.
- Raw results and logs are committed under `rust/evidence/full/`. Scratch process
  data and binaries are ignored under `rust/.bench/`. No live install, client
  configuration, launchd or port 8890 was used. Uncontrolled workstation load and
  single-machine allocator effects limit generalization to a small Linux box.

## Measurements

| Metric | Go main v0.10.1 (full daemon) | Rust subset |
|---|---:|---:|
| Echo latency, median of 3 × 2,000 batch medians (ms) | 0.752 | 0.373 |
| Idle daemon RSS (MiB) | 24.58 | 14.31 |
| Daemon RSS after 2,000 calls (MiB) | 34.52 | 15.50 |
| Daemon RSS with 300 mock definitions (MiB) | 32.94 | 19.33 |
| Release binary size (MiB) | 13.67 | 15.08 |
| Startup, median of 3 (ms) | 27.02 | 26.83 |
| 300-tool startup, one trial (ms) | 138.89 | 474.30 |
| Physical source lines (different scope) | 8,592 production + 1,878 tests | 1,134 incl. 3 unit tests |

| Release cross-link | Go | Rust |
|---|---|---|
| Linux amd64 | pass | fails: x86_64-linux-gnu-gcc missing (SQLite/TLS C dependency) |
| Linux arm64 | pass | fails: aarch64-linux-gnu-gcc missing |
| Windows amd64 | pass | fails: MinGW gcc/dlltool missing; Unix-only source also needs porting |
| macOS amd64 | pass | pass |

Native macOS arm64 release build passes. Cross-link attempts were repeated after the final subscription correction. No cross-target binaries were executed.

The baseline was refreshed when main advanced during the experiment. The earlier
v0.10.0 run is preserved in commit 0c3da0a; the table and current raw results use
v0.10.1. Rust still synchronously commits both phases with FULL durability; Go now
uses NORMAL and queues outcomes. Different authorization, audit, serialization and
durability paths prevent attribution of the measured differences to language alone.
The headline is lower RSS in a smaller Rust implementation, **not** full-parity performance.

## Verification and remaining work

`go test ./...`, `cargo test --locked`, clippy with `-D warnings`, native release build, direct-vs-broker SDK smoke, lifecycle smoke and local HTTP smoke passed. Three Rust unit tests cover duration/glob/config validation and shaping; no Rust policy/grants/client tests exist.

SOFA lookup for `rmcp subscriptions listen tools list changed 2026` returned no posts. The subscription fix follows local rmcp 3.8.0 and Go SDK 1.8.0 source and was checked against the real Go mock.

Next implementation work must finish priority 1 (OAuth, pins, supervision, cache/reload edge cases and notification recovery), then complete front parity, port grants/policy/audit and admin/client CLI, and port the full differential suite. Rerun these comparisons only after those features exist before making a language/runtime choice. No PR or main merge/push was performed.
