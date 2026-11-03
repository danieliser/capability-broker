# Rust brokerd comparison MVP

An experiment, not a replacement installation. No installer, launchd, grants,
policy, OAuth, client configuration, hot reload, prompts/resources, runtime tool
changes, response shaping or upstream supervision/restart. Tool discovery happens
once at startup; a failed child fails calls until the prototype is restarted.

```sh
cd rust
cargo build --release --locked
# Use a NEW, private fixture directory, never ~/.brokerd.
# Populate connectors.yaml, optional secrets.json, and owner.token (mode 0600).
./target/release/brokerd-rust /absolute/fixture /absolute/fixture/owner.token 8991
```

The explicit directory and token file are required; the listener is always IPv4
loopback and refuses port 8890. Protect the directory with mode 0700: the MVP does
not enforce token/secret/database permissions. A static token authenticates all
requests and grants owner access to every loaded tool. Use Ctrl-C to drain HTTP
requests and drop upstream transports. Do not use real credentials for comparison.
Child environment is only PATH/HOME plus configured env. `${secret:NAME}` references
inside env values must exist and be nonempty; remote/OAuth/disabled entries are
skipped. `command` and `args` are passed directly (no shell or home expansion).

`/mcp/<connector>` preserves upstream tool declarations. `/mcp` leaves names plain
unless they occur in multiple connectors, then prefixes each with `<connector>__`.
A secondary collision with a literal upstream name fails startup rather than routing
ambiguously. All endpoints use rmcp sessionless Streamable HTTP, with JSON replies
when possible. Results pass through as SDK values, not byte-identical JSON encoding.
Discovery prefers current MCP and falls back to legacy upstream initialization.

SQLite uses bundled SQLite through rusqlite, WAL, synchronous FULL, one shared
connection, two independent autocommit INSERTs per upstream call, append-only
triggers, and a call ID joining intent/outcome. A failed intent prevents execution;
a failed outcome returns an error (the upstream may have executed). The schema is
smaller than Go's: it records connector, tool, phase, state, timestamps and duration,
without Go's grant metadata, argument digest/summary or response size. Process
crashes can leave an intent without an outcome. Invalid tool names and rejected
HTTP auth do not execute upstream and do not receive two rows.

From the repository root:

```sh
python3 rust/bench/run.py       # builds scratch Go/mock/client; 3 x 2,000 echo calls each
python3 rust/bench/smoke.py     # after run.py has built the fixture mock
python3 rust/bench/cross.py     # attempts release linking, not just cargo check
```

Run sequentially on an otherwise quiet machine. All artifacts and disposable data
are in ignored `rust/.bench/`; raw committed measurement evidence is in `evidence/`.
The benchmark refuses occupied ports naturally at bind; only its own process
handles are stopped. Go runs with scratch HOME, BROKERD_DATA_DIR and port 8990;
Rust runs on 8991. No installed binaries or client configuration are used.
