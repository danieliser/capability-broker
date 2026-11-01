#!/usr/bin/env bash
# Publication gate: scripts/release-check.sh release/X.Y.Z
# The branch name, the CHANGELOG's top released entry and semver syntax must agree.
set -euo pipefail
BRANCH="${1:?branch name}"
case "$BRANCH" in release/*) ;; *) echo "not a release branch: $BRANCH"; exit 1;; esac
VERSION="${BRANCH#release/}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || { echo "version '$VERSION' is not semver"; exit 1; }
TOP="$(grep -E '^## \[' CHANGELOG.md | grep -v '^## \[Unreleased\]' | head -1 || true)"
[[ "$TOP" =~ ^##\ \[([^]]+)\]\ -\ [0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || { echo "CHANGELOG top entry must be '## [X.Y.Z] - YYYY-MM-DD', got: $TOP"; exit 1; }
CHANGELOG_VERSION="${BASH_REMATCH[1]}"
test "$CHANGELOG_VERSION" = "$VERSION" || { echo "CHANGELOG top entry is $CHANGELOG_VERSION, branch says $VERSION"; exit 1; }
if grep -qE '^## \[Unreleased\]' CHANGELOG.md; then
  # An Unreleased section may exist only if it is empty.
  BODY="$(awk '/^## \[Unreleased\]/{f=1;next} /^## \[/{f=0} f' CHANGELOG.md | grep -vE '^\s*$' || true)"
  test -z "$BODY" || { echo "CHANGELOG still has Unreleased entries; move them under [$VERSION]"; exit 1; }
fi
echo "release $VERSION: branch and changelog agree"
