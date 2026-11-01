#!/usr/bin/env bash
# Print the CHANGELOG section for one version: scripts/changelog-section.sh X.Y.Z
set -euo pipefail
V="${1:?version}"
awk -v v="$V" '$0 ~ "^## \\[" v "\\]" {f=1; next} /^## \[/ {f=0} f' CHANGELOG.md
