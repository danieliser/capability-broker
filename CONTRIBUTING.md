# Contributing

Issues and pull requests are welcome. For anything bigger than a fix, open an issue first so we can agree on the
shape before you build it.

## Develop

```sh
go test ./...                    # includes test/e2e: the broker against cmd/mockmcp, a server using every MCP feature
go vet ./... && gofmt -l .       # CI fails on either
go run ./cmd/brokerd --port 8999 --data-dir /tmp/brokerd-dev   # a throwaway broker that leaves yours alone
```

Two rules shape most changes:

1. **Invisible to the owner.** Your own apps must see each MCP server exactly as if connected directly: tool
   definitions, results, errors, instructions, prompts and resources pass through untouched. A visible difference
   is a bug.
2. **Agents get grants, not servers.** Anything an agent can reach goes through a grant and a receipt.

If you add an MCP feature to the pass-through, add it to `cmd/mockmcp` and cover it in `test/e2e`.

## Release

Open a `release/X.Y.Z` branch whose `CHANGELOG.md` top entry is `## [X.Y.Z] - YYYY-MM-DD`, and raise a PR into
`main`. The gate checks branch, changelog and build identity; merging tags the release and publishes binaries for
macOS and Linux (amd64, arm64) with a `SHA256SUMS` file.
