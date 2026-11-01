#!/usr/bin/env bash
# Build brokerd with a stamped identity, install it atomically under ~/.local/bin,
# and (re)load the launchd agent. Mirrors agentruntime's install.sh: never cp over
# the running binary (macOS kills it with OS_REASON_CODESIGNING); write a new file
# and mv it into place, then kickstart the service.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
DATA_DIR="${BROKERD_DATA_DIR:-$HOME/.brokerd}"
PORT="${BROKERD_PORT:-8890}"
LABEL="com.capability-broker.brokerd"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/capability-broker"

VERSION="${BROKERD_VERSION:-$(grep -m1 -oE '^## \[[0-9][^]]*\]' "$SCRIPT_DIR/CHANGELOG.md" | tr -d '#[] ')}"
COMMIT="${BROKERD_COMMIT:-$(git -C "$SCRIPT_DIR" rev-parse HEAD)}"
if [ -z "$VERSION" ]; then VERSION="0.0.0-dev.$(git -C "$SCRIPT_DIR" rev-parse --short HEAD)"; fi
if [ -n "$(git -C "$SCRIPT_DIR" status --porcelain)" ] && [ "${ALLOW_DIRTY:-0}" != "1" ]; then
  echo "error: working tree is dirty; commit first or set ALLOW_DIRTY=1"; exit 1
fi

mkdir -p "$BIN_DIR" "$DATA_DIR" "$LOG_DIR"
chmod 700 "$DATA_DIR"

echo "building brokerd ${VERSION}@${COMMIT}"
( cd "$SCRIPT_DIR" && go build -trimpath \
  -ldflags="-X github.com/danieliser/capability-broker/pkg/buildinfo.Version=${VERSION} -X github.com/danieliser/capability-broker/pkg/buildinfo.Commit=${COMMIT}" \
  -o "$BIN_DIR/brokerd.new" ./cmd/brokerd )
"$BIN_DIR/brokerd.new" --require-build "${VERSION}@${COMMIT}"
if [ -f "$BIN_DIR/brokerd" ]; then
  cp "$BIN_DIR/brokerd" "$BIN_DIR/brokerd.pre-$(date +%Y%m%dT%H%M%S)"
fi
mv -f "$BIN_DIR/brokerd.new" "$BIN_DIR/brokerd"
echo "installed $BIN_DIR/brokerd"

if [ "$(uname)" = "Darwin" ]; then
  mkdir -p "$(dirname "$PLIST")"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN_DIR/brokerd</string>
        <string>--port</string>
        <string>$PORT</string>
        <string>--data-dir</string>
        <string>$DATA_DIR</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>$LOG_DIR/brokerd.log</string>
    <key>StandardErrorPath</key>
    <string>$LOG_DIR/brokerd.log</string>
    <key>WorkingDirectory</key>
    <string>$HOME</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$HOME/.nvm/versions/node/v22.19.0/bin:$HOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
        <key>HOME</key>
        <string>$HOME</string>
        <key>BROKERD_WP_ROUTER_COMMAND</key>
        <string>${BROKERD_WP_ROUTER_COMMAND:-$HOME/.nvm/versions/node/v22.19.0/bin/npx}</string>
    </dict>
</dict>
</plist>
EOF
  launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$PLIST"
  launchctl kickstart -k "gui/$(id -u)/$LABEL"
  echo "launchd agent $LABEL loaded; log at $LOG_DIR/brokerd.log"
  for _ in $(seq 1 30); do
    if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
      echo "healthy: $(curl -fsS "http://127.0.0.1:$PORT/healthz")"; exit 0
    fi
    sleep 1
  done
  echo "error: brokerd did not answer /healthz within 30s"; exit 1
fi
