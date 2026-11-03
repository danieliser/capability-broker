# brokerd: Go daemon vs Rust MVP

Measured 2026-11-02/03 on this Mac: Apple M2 Max, 64 GiB RAM, macOS
Darwin 25.5.0 arm64. Go 1.26.5; rustc/cargo 1.97.1; Go MCP SDK 1.8.0;
rmcp 3.8.0; rusqlite 0.39.0 with bundled libsqlite3-sys 0.37.0.
Go source baseline: `b240e459b5a7e4ea05f7e832edd5b9c1d0018df9`.
Rust dependencies are pinned by Cargo.lock.

## Measured comparison

| Metric | Go daemon | Rust MVP |
|---|---:|---:|
| Echo latency, median of three run medians | 728.813 µs | 363.709 µs |
| Run 1 median, 2,000 sequential calls | 728.813 µs | 363.709 µs |
| Run 2 median, 2,000 sequential calls | 717.146 µs | 363.292 µs |
| Run 3 median, 2,000 sequential calls | 731.855 µs | 364.896 µs |
| Idle daemon RSS, after tools/list | 22.625 MiB | 8.281 MiB |
| Daemon RSS after exactly 2,000 echo calls | 31.438 MiB | 9.641 MiB |
| Release binary bytes | 13,830,466 | 9,292,112 |
| Release binary MiB | 13.190 | 8.862 |
| Process launch → successful tools/list, median of 3 | 26.937 ms | 27.137 ms |
| Launch samples, ms | 372.921 / 26.218 / 26.937 | 353.959 / 26.327 / 27.137 |
| Physical source lines in scoped functionality files† | 1,928 | 294 |

Rust measured about half the median latency here, with lower RSS and binary size.
Startup medians were effectively equal. This is **not a language-only comparison**:
Go is the full daemon, Rust a deliberately smaller prototype, and the receipt
schema/index count differs. No production or many-client throughput claim follows.

† `bench/loc.py` counts physical lines including blanks/comments, excluding tests,
benchmarks, generated dependencies and lockfiles. Rust's 294 lines implement the
MVP end to end. Go's matching responsibilities live in `pkg/mcpfront/front.go`
(651), `pkg/adapters/upstream/upstream.go` (661), `pkg/connectors/connectors.go`
(268), `pkg/audit/audit.go` (214), and `pkg/durable/sqlite.go` (134). Those files
also implement extra functionality, and exclude additional auth, policy, registry,
reload, adapter helpers and startup code. An exact equivalent-only Go LOC cannot
be separated honestly from those shared modules without building another Go MVP;
1,928 is a reproducible scoped-file count, not an equivalent implementation ratio.

## Method and isolation

Run from the repository root, sequentially:

```sh
cargo build --release --locked --manifest-path rust/Cargo.toml
python3 rust/bench/cross.py
python3 rust/bench/run.py
python3 rust/bench/smoke.py
python3 rust/bench/report.py
python3 rust/bench/loc.py
```

`bench/run.py` builds Go with `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w'`
and builds one `cmd/mockmcp` fixture binary and one Go SDK client used by **both**
daemons. Rust release uses stripped symbols and thin LTO. Both are native arm64.
Each daemon hosts one child, started once and reused for the measurement.
Go uses scratch BROKERD_DATA_DIR and HOME, port 8990; Rust uses a separate scratch
directory and port 8991. No installer, installed daemon, client config, launchd
or live port 8890 is used. Fixture tokens are disposable and not committed.
Only subprocesses created by the runner are stopped.

Each latency run makes 2,000 sequential owner `echo({"text":"hello"})` calls through
`/mcp/mock`, reusing the same Go SDK Streamable HTTP client and pooled HTTP transport.
There is no warmup call; initialization/discovery is outside the timer. The clock
surrounds SDK CallTool, including HTTP, upstream stdio, response decode and both
receipt writes. Three runs reuse the same daemon/session. Median averages the two
middle sorted durations; the headline is the median of those three run medians.
Mean call times (including loop overhead) are in `evidence/results.json`.
Both paths negotiate MCP 2026-07-28 and use stateless JSON HTTP replies.

RSS is macOS `ps -o rss= -p <daemon-pid>` in KiB, excluding the child and benchmark
client. Idle is after the first successful list and before any call. The client
pauses after exactly 2,000 calls for one RSS sample. Smoke calls occur after all
6,000 timed calls. These are snapshots, not peak memory or allocator profiles.

For startup, each binary is launched three times with fresh data directories and
its own mock child. The runner repeatedly invokes the **same** SDK client until
list returns nonempty, with a 5 ms delay after failures. Timing starts before
Popen and includes client process creation, discovery, HTTP, polling and tool list.
Go's DB/schema and owner token are seeded before timing (required to authenticate);
Rust creates its receipt DB/schema during startup and reads a prewritten token.
The first launch costs ~350–373 ms; all samples are retained. This is process cold
start, **not** a flushed filesystem/page-cache cold-start measurement. Tools/list
first succeeds around 26–27 ms on subsequent launches; polling resolution limits
precision. Cross builds and this task's test compilation completed before latency collection. Other machine activity
was not controlled; these three local runs are not an isolated laboratory study.

Both receipt paths use WAL and synchronous FULL, autocommit twice per call, not
one transaction per pair. Go uses modernc SQLite; Rust bundles C SQLite through
rusqlite and executes each insertion on Tokio's blocking pool, sharing one mutexed
connection. Go additionally resolves/authenticates an owner grant, loads catalogs,
computes argument digests/summaries, and writes more columns and indexes. Rust
has a smaller receipt table/index set and static bearer validation. Two writes
match the requested cost model, not identical CPU work or pages written.
Verified rows: Go 6,005 intent + 6,005 outcome; Rust 6,010 + 6,010 (the extra five
are an aggregate endpoint smoke). The five upstream tool-error results are
preserved; error state is recorded as failed.

## Portability: actual release-link attempts from this Mac

| Target | Go, CGO_ENABLED=0 | Rust, bundled SQLite | What was needed / blocker |
|---|---|---|---|
| linux/amd64 | Built | Failed | Missing `x86_64-linux-gnu-gcc` for SQLite |
| linux/arm64 | Built | Failed | Missing `aarch64-linux-gnu-gcc` for SQLite |
| windows/amd64 | Built | Failed | Missing `x86_64-w64-mingw32-dlltool`; no MinGW compiler/toolchain |
| darwin/amd64 | Built | Built | rustup target + installed Apple clang/macOS SDK |

Installed Rust std targets with `rustup target add x86_64-unknown-linux-gnu
 aarch64-unknown-linux-gnu x86_64-pc-windows-gnu x86_64-apple-darwin`, then ran
`cargo build --release --locked --target <triple>` for each. Go required only
GOOS/GOARCH/CGO_ENABLED=0 and the existing Go toolchain. Raw diagnostics and
elapsed build times are in `evidence/cross*`. No failed target is counted as
portable because cargo check succeeded. Intel Mac binaries were linked, not
executed on a separate Intel machine; cross Go binaries were not run either.

A Linux/Windows cross C compiler, target sysroot and linker would be required for
this Rust choice; Zig/cargo-zigbuild or target-native CI/container builds are
possible follow-ups, not verified remedies. They were not installed for this MVP.
Bundled SQLite avoids a runtime system SQLite dependency but introduces native
build machinery. The macOS Rust binary still dynamically links libSystem and
libiconv (`otool -L`); bundled SQLite is not a fully static binary. Fully static
Linux would need a musl target plus a suitable C toolchain and actual verification;
this experiment does not claim that build works. Go's pure-Go modernc dependency
makes the requested no-CGO cross builds much simpler.

## What was difficult / limitations

- **Async runtime:** rmcp client services must remain alive across frontend requests.
  Shared RunningService handles, per-endpoint views and owned route names need
  explicit lifetimes/Arc ownership. SQLite calls are blocking, so spawn_blocking
  avoids blocking the two Tokio I/O workers; the mutex serializes database access.
- **SDK API maturity:** rmcp 3.8 uses non-exhaustive model structs/builders and
  CallToolResponse rather than only CallToolResult in handlers. The first compile
  caught those differences. Default `ServiceExt::serve` uses legacy initialization;
  the current mock adds protocol metadata under current discovery. Using
  `ClientServiceExt::serve_with_lifecycle(Auto)` fixed the observed metadata loss.
  The exact direct-vs-proxy comparison then passed, without removing fields from
  result comparisons. SDK values preserve the tested JSON semantics, not original
  byte ordering. Unknown future schema fields may need SDK upgrades.
- **Go SDK ergonomics:** the existing daemon already has transparent owner paths,
  tests, lifecycle supervision and current protocol integration. Rust's typed
  glue is small but unfamiliar; this experiment does not establish broad
  superiority/inferiority of either SDK.
- **SQLite choice:** rusqlite was straightforward and compact; bundled SQLite
  trades easy native linking for harder cross builds. Replacing it with SQLx
  would still need SQLite linking, unless choosing a different backend/build
  strategy; no alternative was benchmarked.
- **Scope:** no grants/policy, URL/OAuth, config reload, dynamic tool refresh,
  resource/prompt relay, sampling, response filtering, client installation,
  upstream restart, or production permissions enforcement. Invalid tool calls
  and HTTP denials do not get paired receipts; accepted upstream calls do.
  The smaller scope likely contributes materially to memory/latency differences.
- **Transparency boundaries:** JSON-RPC transport failures are mapped to internal
  MCP errors; tool results including isError, structured content and images pass
  through. Current MRTR/task/interactive workflows were not tested.
- **Evidence lookup:** SOFA search `rmcp stateless StreamableHttpService proxy`
  returned no applicable Rust integration posts. Implementation used official
  [Rust SDK documentation](https://docs.rs/rmcp/3.8.0/rmcp/) and fetched SDK source,
  plus local compile/runtime evidence.

## Verification

`cargo build --release --locked`, `cargo fmt --check`, `cargo clippy -- -D warnings`,
`go test ./...` and `go vet ./...` passed. SDK smoke compares complete serialized
CallToolResult values against a direct mock connection for echo, markdown, pixel,
fail and noisy, plus every upstream tool definition (including annotations,
output schema, title and _meta). Both Rust per-connector and unprefixed single-
connector aggregate endpoints passed. Additional smoke covers two-connector name
clashes, bearer denials on all endpoints, stateless headers, skipped remote,
args/env/secret substitution, environment isolation, missing secrets and intent
write failure preventing upstream execution. Re-querying receipts confirmed both
phases; a denied grow call did not increment the mock's added-tool counter.

Binary SHA-256:

- Go: `4b3e71469948478329f8353bd76a705f3da9177c1ec2d0a0dd9cca587f9ae4d1`
- Rust: `11cfe93904a87c6f68eb83cf4d375b4d817ae4f6bc5510c624e993ac2306130b`
- Shared mock: `073beb93b55f9fcd0a255b330d15ca12b8df85cf1ae62d4fce4b9130a635be92`
