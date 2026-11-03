# Rust broker experiment — incomplete full-port attempt

This is **not a drop-in replacement**. See [COMPARISON-FULL.md](COMPARISON-FULL.md)
for the current parity gaps, measurements and methods. [COMPARISON.md](COMPARISON.md)
retains the earlier, smaller MVP results.

It currently implements stdio and unauthenticated/configured-header remote HTTP
connectors, owner tool/result and per-connector prompt/resource forwarding,
on-demand start, idle stop, discovery caching, two-second/SIGHUP connector reload,
upstream list-change subscriptions, tool filters/strip/response shaping and basic
on-demand discovery tools. No OAuth, runner pins, grants/policy/catalogs, admin API
or production CLI/client configuration support exists.

Use only new private fixture directories and test tokens:

```sh
cargo build --release --locked --manifest-path rust/Cargo.toml
# connectors.yaml and owner.token belong to a NEW private fixture directory.
rust/target/release/brokerd-rust /absolute/fixture /absolute/fixture/owner.token 8991
```

The explicit directory/token/port launch remains intentionally incompatible with
Go's CLI. Listener is IPv4 loopback and refuses 8890. Static fixture token grants
owner access; connectors with `clients:` are hidden because client identity is not
ported. OAuth configuration is explicitly rejected. Secrets must be mode 0600.
The receipt schema remains incompatible with Go: **never use an existing Go data
directory**. No installation, client config changes or live upstream credentials
are needed to run these tests:

```sh
cargo test --locked --manifest-path rust/Cargo.toml
cargo clippy --locked --manifest-path rust/Cargo.toml -- -D warnings
python3 rust/bench/full_smoke.py
python3 rust/bench/remote_smoke.py
python3 rust/bench/run.py
python3 rust/bench/shape.py
python3 rust/bench/cross.py
```

Run sequentially. `full_smoke.py` builds the Go mock; run.py builds isolated Go/mock
and SDK-client binaries. Scripts use ports 8990/8991/8995 and ignored scratch data
under `rust/.bench/`. Stop only process handles created by the harness. The old
`bench/smoke.py` targets MVP behavior (silent OAuth skipping) and is superseded by
`full_smoke.py`. Committed raw evidence for this attempt is in `evidence/full/`.
