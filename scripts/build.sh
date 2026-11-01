#!/usr/bin/env bash
# Build brokerd with a stamped identity: scripts/build.sh <version> <commit> <output-path>
set -euo pipefail
VERSION="$1"; COMMIT="$2"; OUT="$3"
mkdir -p "$(dirname "$OUT")"
go build -trimpath \
  -ldflags="-X github.com/danieliser/capability-broker/pkg/buildinfo.Version=${VERSION} -X github.com/danieliser/capability-broker/pkg/buildinfo.Commit=${COMMIT}" \
  -o "$OUT" ./cmd/brokerd
