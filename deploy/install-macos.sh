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

# The optional MLX server — a second local runtime behind the gateway. Its venv
# lives outside the repo (a username-path that must not be committed), and it is
# only supervised when that venv is present.
MLX_LABEL="com.lattice.mlx"
MLX_TEMPLATE="$REPO_ROOT/deploy/$MLX_LABEL.plist.in"
MLX_SERVER="$HOME/lattice-mlx/venv/bin/mlx_lm.server"
MLX_DEST="$HOME/Library/LaunchAgents/$MLX_LABEL.plist"

if [[ ! -x "$BIN" ]]; then
  echo "error: $BIN is missing or not executable." >&2
  echo "build it first:" >&2
  echo "  GOOS=darwin GOARCH=arm64 go build -o bin/darwin-arm64/lattice-gateway ./cmd/lattice-gateway" >&2
  exit 1
fi

# Tear down our own instance before anything looks at :8081, so a re-install picks
# up template changes and the check below only ever sees a process launchd does not
# own. "not loaded" is fine. Deliberately after the binary check above: failing
# that must not leave the user with nothing running.
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true

# A gateway started by hand (no LaunchAgent) holds :8081, so the managed one
# would crash-loop on bind failure and KeepAlive would keep retrying forever.
# Refuse rather than fight it — name the offender and let the operator decide.
# Our own instance is already booted out, so a listener here is genuinely
# unmanaged, and the "kill" below is advice that clears the condition rather than
# one launchd keeps undoing.
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

# The MLX server is optional: without its venv the gateway still runs, and an
# MLX-routed model fails loudly at request time. Only supervise it when present,
# and refuse to fight an unmanaged mlx-lm server already holding :8080.
if [[ -x "$MLX_SERVER" ]]; then
  launchctl bootout "$DOMAIN/$MLX_LABEL" 2>/dev/null || true
  busy="$(lsof -nP -iTCP:8080 -sTCP:LISTEN -t 2>/dev/null || true)"
  if [[ -n "$busy" ]]; then
    for pid in $busy; do
      if [[ "$(ps -o command= -p "$pid" 2>/dev/null)" == *mlx_lm.server* ]]; then
        echo "error: an unmanaged mlx-lm server (pid $pid) is holding :8080." >&2
        echo "stop it, then re-run this script:" >&2
        echo "  kill $pid" >&2
        exit 1
      fi
    done
  fi
  sed -e "s|__MLX_SERVER__|$MLX_SERVER|g" -e "s|__LOG_DIR__|$LOG_DIR|g" \
    "$MLX_TEMPLATE" >"$MLX_DEST"
  launchctl bootstrap "$DOMAIN" "$MLX_DEST"
  if launchctl print "$DOMAIN/$MLX_LABEL" >/dev/null 2>&1; then
    echo "installed: $MLX_DEST"
    echo "running  : $DOMAIN/$MLX_LABEL"
    echo "logs     : $LOG_DIR/lattice-mlx.{out,err}.log"
  else
    echo "warning: mlx bootstrap failed — check $LOG_DIR/lattice-mlx.err.log" >&2
  fi
else
  echo "note: mlx server not found at $MLX_SERVER — skipping the mlx agent."
fi

# The MFLUX sidecar — the local image-generation backend for the Hermes "mflux"
# image_gen plugin. It is optional and, like the MLX server, only supervised when
# its venv (a username-path that must not be committed) is present. The sidecar is
# not a gateway provider: image generation is a different modality from the
# chat/embeddings surface the gateway speaks.
MFLUX_LABEL="com.lattice.mflux"
MFLUX_TEMPLATE="$REPO_ROOT/deploy/$MFLUX_LABEL.plist.in"
MFLUX_PYTHON="$HOME/lattice-mflux/venv/bin/python"
# The 4-bit model baked by `mflux-save --model dev --quantize 4` (see the spec).
# It is the username-path that must not be committed, like the venv above.
MFLUX_MODEL="$HOME/lattice-mflux/models/flux-dev-4bit"
MFLUX_DEST="$HOME/Library/LaunchAgents/$MFLUX_LABEL.plist"

if [[ -x "$MFLUX_PYTHON" ]]; then
  launchctl bootout "$DOMAIN/$MFLUX_LABEL" 2>/dev/null || true
  busy="$(lsof -nP -iTCP:8899 -sTCP:LISTEN -t 2>/dev/null || true)"
  if [[ -n "$busy" ]]; then
    for pid in $busy; do
      if [[ "$(ps -o command= -p "$pid" 2>/dev/null)" == *mflux-sidecar.py* ]]; then
        echo "error: an unmanaged mflux sidecar (pid $pid) is holding :8899." >&2
        echo "stop it, then re-run this script:" >&2
        echo "  kill $pid" >&2
        exit 1
      fi
    done
  fi
  sed -e "s|__MFLUX_PYTHON__|$MFLUX_PYTHON|g" -e "s|__MFLUX_MODEL__|$MFLUX_MODEL|g" -e "s|__REPO_ROOT__|$REPO_ROOT|g" -e "s|__LOG_DIR__|$LOG_DIR|g" \
    "$MFLUX_TEMPLATE" >"$MFLUX_DEST"
  launchctl bootstrap "$DOMAIN" "$MFLUX_DEST"
  if launchctl print "$DOMAIN/$MFLUX_LABEL" >/dev/null 2>&1; then
    echo "installed: $MFLUX_DEST"
    echo "running  : $DOMAIN/$MFLUX_LABEL"
    echo "logs     : $LOG_DIR/lattice-mflux.{out,err}.log"
  else
    echo "warning: mflux bootstrap failed — check $LOG_DIR/lattice-mflux.err.log" >&2
  fi
else
  echo "note: mflux venv not found at $MFLUX_PYTHON — skipping the mflux sidecar."
fi
