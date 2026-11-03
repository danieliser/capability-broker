#!/usr/bin/env bash
# Install brokerd and run it as a user service (launchd on macOS, systemd --user on Linux).
#
#   ./install.sh                 from a clone with Go installed: build from source
#   ./install.sh --binary        download the release binary for this OS/arch and verify its checksum
#   sh install.sh                outside a clone (just this file): same as --binary
#   ./install.sh --no-service    install the binary only; run `brokerd` yourself (containers, CI, other inits)
#
# Settings: BIN_DIR (~/.local/bin), BROKERD_DATA_DIR (~/.brokerd), BROKERD_PORT (8890),
# BROKERD_VERSION (release to download; default: latest).
#
# The binary is replaced atomically (written beside, then mv): macOS kills a process whose binary is copied over
# in place (OS_REASON_CODESIGNING).
set -euo pipefail

REPO="danieliser/capability-broker"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
BIN_DIR="${BIN_DIR:-$HOME/.local/bin}"
DATA_DIR="${BROKERD_DATA_DIR:-$HOME/.brokerd}"
PORT="${BROKERD_PORT:-8890}"
MODE=""
SERVICE=1
for arg in "$@"; do
  case "$arg" in
    --binary) MODE=binary ;;
    --source) MODE=source ;;
    --no-service) SERVICE=0 ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done
if [ -z "$MODE" ]; then
  if [ -f "$SCRIPT_DIR/go.mod" ] && command -v go >/dev/null 2>&1; then MODE=source; else MODE=binary; fi
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$OS" in darwin|linux) ;; *) echo "error: $OS is not supported yet (macOS and Linux)" >&2; exit 1 ;; esac
ARCH="$(uname -m)"
case "$ARCH" in x86_64|amd64) ARCH=amd64 ;; arm64|aarch64) ARCH=arm64 ;; *) echo "error: unsupported CPU $ARCH" >&2; exit 1 ;; esac

mkdir -p "$BIN_DIR" "$DATA_DIR" "$DATA_DIR/catalog.d"
chmod 700 "$DATA_DIR"

if [ "$MODE" = source ]; then
  VERSION="${BROKERD_VERSION:-$( { grep -m1 -oE '^## \[[0-9][^]]*\]' "$SCRIPT_DIR/CHANGELOG.md" || true; } | tr -d '#[] ')}"
  COMMIT="${BROKERD_COMMIT:-$(git -C "$SCRIPT_DIR" rev-parse HEAD)}"
  if [ -z "$VERSION" ]; then VERSION="0.0.0-dev.$(git -C "$SCRIPT_DIR" rev-parse --short HEAD)"; fi
  if [ -n "$(git -C "$SCRIPT_DIR" status --porcelain)" ] && [ "${ALLOW_DIRTY:-0}" != "1" ]; then
    echo "error: working tree is dirty; commit first or set ALLOW_DIRTY=1"; exit 1
  fi
  echo "building brokerd ${VERSION}@${COMMIT}"
  ( cd "$SCRIPT_DIR" && go build -trimpath \
    -ldflags="-X github.com/danieliser/capability-broker/pkg/buildinfo.Version=${VERSION} -X github.com/danieliser/capability-broker/pkg/buildinfo.Commit=${COMMIT}" \
    -o "$BIN_DIR/brokerd.new" ./cmd/brokerd \
    && go build -trimpath -o "$BIN_DIR/brokerd-probe.new" ./cmd/brokerd-probe )
  "$BIN_DIR/brokerd.new" --require-build "${VERSION}@${COMMIT}"
  EXAMPLE="$SCRIPT_DIR/connectors.example.yaml"
else
  command -v curl >/dev/null 2>&1 || { echo "error: curl is required" >&2; exit 1; }
  TAG="${BROKERD_VERSION:+v${BROKERD_VERSION#v}}"
  if [ -z "$TAG" ]; then
    # Read the whole response first: piping curl into a reader that exits early breaks curl (pipefail).
    latest="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest")"
    TAG="$(printf '%s\n' "$latest" | grep '"tag_name"' | head -1 | sed -E 's/.*"(v[^"]+)".*/\1/')"
  fi
  [ -n "$TAG" ] || { echo "error: could not find the latest release" >&2; exit 1; }
  NAME="brokerd_${TAG#v}_${OS}_${ARCH}"
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT
  echo "downloading $NAME ($TAG)"
  curl -fsSL -o "$TMP/$NAME.tar.gz" "https://github.com/$REPO/releases/download/$TAG/$NAME.tar.gz"
  curl -fsSL -o "$TMP/SHA256SUMS" "https://github.com/$REPO/releases/download/$TAG/SHA256SUMS"
  want="$(awk -v n="$NAME.tar.gz" '$2==n {print $1}' "$TMP/SHA256SUMS")"
  if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$TMP/$NAME.tar.gz" | awk '{print $1}')"; else got="$(shasum -a 256 "$TMP/$NAME.tar.gz" | awk '{print $1}')"; fi
  [ -n "$want" ] && [ "$want" = "$got" ] || { echo "error: checksum mismatch for $NAME.tar.gz" >&2; exit 1; }
  echo "checksum ok"
  tar -C "$TMP" -xzf "$TMP/$NAME.tar.gz"
  mv -f "$TMP/$NAME/brokerd" "$BIN_DIR/brokerd.new"
  mv -f "$TMP/$NAME/brokerd-probe" "$BIN_DIR/brokerd-probe.new"
  chmod 755 "$BIN_DIR/brokerd.new" "$BIN_DIR/brokerd-probe.new"
  curl -fsSL -o "$TMP/connectors.example.yaml" "https://raw.githubusercontent.com/$REPO/$TAG/connectors.example.yaml" || true
  EXAMPLE="$TMP/connectors.example.yaml"
fi

if [ ! -f "$DATA_DIR/connectors.yaml" ] && [ -f "$EXAMPLE" ]; then
  cp "$EXAMPLE" "$DATA_DIR/connectors.yaml" && chmod 600 "$DATA_DIR/connectors.yaml"
  echo "wrote $DATA_DIR/connectors.yaml from the example; add your servers there (or: brokerd client adopt)"
fi
if [ -f "$BIN_DIR/brokerd" ]; then
  # One previous binary is kept for a manual rollback.
  rm -f "$BIN_DIR"/brokerd.pre-*
  cp "$BIN_DIR/brokerd" "$BIN_DIR/brokerd.pre-$(date +%Y%m%dT%H%M%S)"
fi
mv -f "$BIN_DIR/brokerd.new" "$BIN_DIR/brokerd"
mv -f "$BIN_DIR/brokerd-probe.new" "$BIN_DIR/brokerd-probe"
echo "installed $BIN_DIR/brokerd ($("$BIN_DIR/brokerd" --version))"
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) echo "note: add $BIN_DIR to your PATH" ;; esac

if [ "$SERVICE" = 0 ]; then
  echo "no service installed; run: $BIN_DIR/brokerd --port $PORT --data-dir $DATA_DIR"
  exit 0
fi

# MCP servers are launched by the broker with this PATH: the one you installed from, so npx, uvx, node and
# python resolve the same way they do in your shell.
SERVICE_PATH="$BIN_DIR:$PATH"

wait_healthy() {
  for _ in $(seq 1 90); do
    if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
      echo "healthy: $(curl -fsS "http://127.0.0.1:$PORT/healthz")"; return 0
    fi
    sleep 1
  done
  echo "error: brokerd did not answer /healthz within 90s; see $1" >&2; return 1
}

if [ "$OS" = darwin ]; then
  LABEL="com.capability-broker.brokerd"
  PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
  LOG_DIR="$HOME/Library/Logs/capability-broker"
  mkdir -p "$(dirname "$PLIST")" "$LOG_DIR"
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
    <!-- Not Background: macOS throttles a Background job's CPU and I/O, and every MCP child inherits it
         (measured: children started 10-35x slower than by hand). -->
    <string>Standard</string>
    <key>StandardOutPath</key>
    <string>$LOG_DIR/brokerd.log</string>
    <key>StandardErrorPath</key>
    <string>$LOG_DIR/brokerd.log</string>
    <key>WorkingDirectory</key>
    <string>$HOME</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>$SERVICE_PATH</string>
        <key>HOME</key>
        <string>$HOME</string>
    </dict>
</dict>
</plist>
EOF
  # A loaded service refuses a second bootstrap (I/O error 5): unload, wait for launchd to drop it, then load.
  if launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1; then
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
    for _ in $(seq 1 60); do launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || break; sleep 0.5; done
  fi
  launchctl bootstrap "gui/$(id -u)" "$PLIST" || launchctl kickstart -k "gui/$(id -u)/$LABEL"
  echo "launchd agent $LABEL loaded; log at $LOG_DIR/brokerd.log"
  wait_healthy "$LOG_DIR/brokerd.log"
else
  command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1 || {
    echo "systemd --user is not available; run: $BIN_DIR/brokerd --port $PORT --data-dir $DATA_DIR"; exit 0; }
  UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/brokerd.service" <<EOF
[Unit]
Description=brokerd capability broker (MCP hub)
After=network-online.target

[Service]
ExecStart=$BIN_DIR/brokerd --port $PORT --data-dir $DATA_DIR
Environment=PATH=$SERVICE_PATH
Environment=HOME=$HOME
WorkingDirectory=$HOME
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable brokerd.service >/dev/null
  systemctl --user restart brokerd.service
  echo "systemd user service brokerd.service enabled; logs: journalctl --user -u brokerd -f"
  echo "to keep it running after logout: loginctl enable-linger $USER"
  wait_healthy "journalctl --user -u brokerd"
fi
