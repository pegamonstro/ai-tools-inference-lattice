#!/usr/bin/env bash
# Install the Lattice gateway as a macOS LaunchAgent.
#
# macOS has no systemd, so this is the per-user equivalent of the two systemd
# user units on the Pi. It has to exist as a script (rather than a plain plist in
# the repo) because launchd cannot expand shell variables: the plist needs
# absolute paths, and absolute macOS paths contain a username, which must not be
# committed.
#
#   ./deploy/install-macos.sh
set -euo pipefail

LABEL="com.lattice.gateway"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$REPO_ROOT/deploy/$LABEL.plist.in"
BIN="$REPO_ROOT/bin/darwin-arm64/lattice-gateway"
DEST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/lattice"
DOMAIN="gui/$UID"

if [[ ! -x "$BIN" ]]; then
  echo "error: $BIN is missing or not executable." >&2
  echo "build it first:" >&2
  echo "  GOOS=darwin GOARCH=arm64 go build -o bin/darwin-arm64/lattice-gateway ./cmd/lattice-gateway" >&2
  exit 1
fi

# A gateway started by hand (no LaunchAgent) holds :8081, so the managed one
# would crash-loop on bind failure and KeepAlive would keep retrying forever.
# Refuse rather than fight it — name the offender and let the operator decide.
busy="$(lsof -nP -iTCP:8081 -sTCP:LISTEN -t 2>/dev/null || true)"
if [[ -n "$busy" ]]; then
  for pid in $busy; do
    if [[ "$(ps -o command= -p "$pid" 2>/dev/null)" == *lattice-gateway* ]]; then
      echo "error: an unmanaged lattice-gateway (pid $pid) is holding :8081." >&2
      echo "stop it, then re-run this script:" >&2
      echo "  kill $pid" >&2
      exit 1
    fi
  done
fi

mkdir -p "$LOG_DIR" "$(dirname "$DEST")"
sed -e "s|__REPO_ROOT__|$REPO_ROOT|g" -e "s|__LOG_DIR__|$LOG_DIR|g" \
  "$TEMPLATE" >"$DEST"

# Bootout first so a re-install picks up template changes; "not loaded" is fine.
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true
launchctl bootstrap "$DOMAIN" "$DEST"

if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
  echo "installed: $DEST"
  echo "running  : $DOMAIN/$LABEL"
  echo "logs     : $LOG_DIR/lattice-gateway.{out,err}.log"
  echo
  echo "the gateway is now supervised and will restart on crash and at login."
else
  echo "error: bootstrap failed — check $LOG_DIR/lattice-gateway.err.log" >&2
  exit 1
fi
