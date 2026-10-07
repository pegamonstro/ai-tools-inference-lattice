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

# The gateway is unauthenticated, so what it binds decides who can reach it:
# loopback for this host's own workloads, the tailnet address for the control
# plane — never a wildcard. The tailnet address is detected from the Tailscale
# CLI (standalone install or the GUI app bundle), falling back to a 100.x
# interface address; an operator can always override with GATEWAY_BIND.
GATEWAY_BIND="${GATEWAY_BIND:-}"
if [[ -z "$GATEWAY_BIND" ]]; then
  TS="$(command -v tailscale || true)"
  if [[ -z "$TS" && -x "/Applications/Tailscale.app/Contents/MacOS/Tailscale" ]]; then
    TS="/Applications/Tailscale.app/Contents/MacOS/Tailscale"
  fi
  TS_IP=""
  [[ -n "$TS" ]] && TS_IP="$("$TS" ip -4 2>/dev/null || true)"
  if [[ -z "$TS_IP" ]] && command -v ifconfig >/dev/null 2>&1; then
    TS_IP="$(ifconfig 2>/dev/null | awk '$1 == "inet" && $2 ~ /^100\./ {print $2; exit}')"
  fi
  if [[ -n "$TS_IP" ]]; then
    GATEWAY_BIND="127.0.0.1:8081,$TS_IP:8081"
  else
    echo "error: could not detect this Mac's tailnet address." >&2
    echo "a wildcard bind would leave the gateway open; set one explicitly:" >&2
    echo "  GATEWAY_BIND=127.0.0.1:8081,<tailnet-ip>:8081 ./deploy/install-macos.sh" >&2
    exit 1
  fi
fi

sed -e "s|__REPO_ROOT__|$REPO_ROOT|g" -e "s|__LOG_DIR__|$LOG_DIR|g" \
  -e "s|__GATEWAY_BIND__|$GATEWAY_BIND|g" \
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
# image_gen plugin. It is optional and only supervised when the mflux CLI (a uv
# tool shim, a username-path that must not be committed) is present. The sidecar
# is not a gateway provider: image generation is a different modality from the
# chat/embeddings surface the gateway speaks.
#
# mflux is installed as a `uv tool` (not a venv), so two things changed from the
# original spec: the sidecar interpreter is the system Python (mflux-sidecar.py is
# stdlib-only — it shells out to the CLI, it does not import mflux), and the
# mflux-generate binary is the uv shim in ~/.local/bin.
MFLUX_LABEL="com.lattice.mflux"
MFLUX_TEMPLATE="$REPO_ROOT/deploy/$MFLUX_LABEL.plist.in"
MFLUX_PYTHON="/usr/bin/python3"
MFLUX_BIN="$HOME/.local/bin/mflux-generate"
MFLUX_FILL_BIN="$HOME/.local/bin/mflux-generate-fill"
MFLUX_REDUX_BIN="$HOME/.local/bin/mflux-generate-redux"
# The 4-bit model baked by `mflux-save --model <path> --base-model dev --quantize 4`
# (see the spec and the M6 cutover notes). A username-path that must not be
# committed. This is Persephone 2.0 (Civitai #1775002), a dedicated NSFW
# FLUX.1-dev transformer fine-tune baked in place of dev's weights — no LoRA.
MFLUX_MODEL="$HOME/mflux-models/persephone-4bit"
# Baked 4-bit fill/redux models (mflux-save --model dev-fill / dev-redux --quantize 4).
# Their quantize env vars stay empty — the models are already 4-bit, and the sidecar
# passes --model <path> (no --base-model) + --vae-tiling for the 1024² decode.
MFLUX_FILL_MODEL="$HOME/mflux-models/flux-fill-dev-4bit"
MFLUX_REDUX_MODEL="$HOME/mflux-models/flux-redux-dev-4bit"
MFLUX_DEST="$HOME/Library/LaunchAgents/$MFLUX_LABEL.plist"

if [[ -x "$MFLUX_BIN" ]]; then
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
  sed -e "s|__MFLUX_PYTHON__|$MFLUX_PYTHON|g" -e "s|__MFLUX_BIN__|$MFLUX_BIN|g" -e "s|__MFLUX_FILL_BIN__|$MFLUX_FILL_BIN|g" -e "s|__MFLUX_REDUX_BIN__|$MFLUX_REDUX_BIN|g" -e "s|__MFLUX_MODEL__|$MFLUX_MODEL|g" -e "s|__MFLUX_FILL_MODEL__|$MFLUX_FILL_MODEL|g" -e "s|__MFLUX_REDUX_MODEL__|$MFLUX_REDUX_MODEL|g" -e "s|__REPO_ROOT__|$REPO_ROOT|g" -e "s|__LOG_DIR__|$LOG_DIR|g" \
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
  echo "note: mflux-generate not found at $MFLUX_BIN — skipping the mflux sidecar."
fi

# The SPEECH sidecar — the local kokoro-mlx (TTS) + mlx-whisper (STT) backend for
# the gateway's "speech" provider. Like mflux it is optional and only supervised
# when its venv (a username-path that must not be committed) is present. It binds
# loopback only: the gateway, which proxies speech, runs on this same host.
SPEECH_LABEL="com.lattice.speech"
SPEECH_TEMPLATE="$REPO_ROOT/deploy/$SPEECH_LABEL.plist.in"
SPEECH_PYTHON="$HOME/lattice-speech/.venv/bin/python"
WHISPER_BIN="$HOME/.local/bin/mlx_whisper"
SPEECH_DEST="$HOME/Library/LaunchAgents/$SPEECH_LABEL.plist"

if [[ -x "$SPEECH_PYTHON" ]]; then
  launchctl bootout "$DOMAIN/$SPEECH_LABEL" 2>/dev/null || true
  busy="$(lsof -nP -iTCP:8900 -sTCP:LISTEN -t 2>/dev/null || true)"
  if [[ -n "$busy" ]]; then
    for pid in $busy; do
      if [[ "$(ps -o command= -p "$pid" 2>/dev/null)" == *speech-sidecar.py* ]]; then
        echo "error: an unmanaged speech sidecar (pid $pid) is holding :8900." >&2
        echo "stop it, then re-run this script:" >&2
        echo "  kill $pid" >&2
        exit 1
      fi
    done
  fi
  # ffmpeg lives in ~/bin (an imageio-ffmpeg symlink) and mlx_whisper shells out
  # to it for audio loading; launchd's default PATH omits it, so it must be in the
  # agent's environment or transcription silently produces no output.
  SPEECH_PATH="$HOME/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
  sed -e "s|__SPEECH_PYTHON__|$SPEECH_PYTHON|g" -e "s|__SPEECH_PATH__|$SPEECH_PATH|g" -e "s|__WHISPER_BIN__|$WHISPER_BIN|g" -e "s|__REPO_ROOT__|$REPO_ROOT|g" -e "s|__LOG_DIR__|$LOG_DIR|g" \
    "$SPEECH_TEMPLATE" >"$SPEECH_DEST"
  launchctl bootstrap "$DOMAIN" "$SPEECH_DEST"
  if launchctl print "$DOMAIN/$SPEECH_LABEL" >/dev/null 2>&1; then
    echo "installed: $SPEECH_DEST"
    echo "running  : $DOMAIN/$SPEECH_LABEL"
    echo "logs     : $LOG_DIR/lattice-speech.{out,err}.log"
  else
    echo "warning: speech bootstrap failed — check $LOG_DIR/lattice-speech.err.log" >&2
  fi
else
  echo "note: speech venv not found at $SPEECH_PYTHON — skipping the speech sidecar."
fi

# The ESRGAN sidecar — the local Real-ESRGAN super-resolution backend for img-gen's
# "Upscale (4x)" mode. Like mflux it is optional and only supervised when its CLI
# (a uv-tool shim, a username-path that must not be committed) is present. It is a
# separate service from mflux because upscaling is a distinct, deterministic pass
# with its own single-flight lifecycle and 4x RAM profile.
ESRGAN_LABEL="com.lattice.esrgan"
ESRGAN_TEMPLATE="$REPO_ROOT/deploy/$ESRGAN_LABEL.plist.in"
ESRGAN_PYTHON="/usr/bin/python3"
ESRGAN_BIN="$HOME/.local/bin/realesrgan-mlx"
ESRGAN_DEST="$HOME/Library/LaunchAgents/$ESRGAN_LABEL.plist"

if [[ -x "$ESRGAN_BIN" ]]; then
  launchctl bootout "$DOMAIN/$ESRGAN_LABEL" 2>/dev/null || true
  busy="$(lsof -nP -iTCP:8901 -sTCP:LISTEN -t 2>/dev/null || true)"
  if [[ -n "$busy" ]]; then
    for pid in $busy; do
      if [[ "$(ps -o command= -p "$pid" 2>/dev/null)" == *esrgan-sidecar.py* ]]; then
        echo "error: an unmanaged esrgan sidecar (pid $pid) is holding :8901." >&2
        echo "stop it, then re-run this script:" >&2
        echo "  kill $pid" >&2
        exit 1
      fi
    done
  fi
  sed -e "s|__ESRGAN_PYTHON__|$ESRGAN_PYTHON|g" -e "s|__ESRGAN_BIN__|$ESRGAN_BIN|g" -e "s|__REPO_ROOT__|$REPO_ROOT|g" -e "s|__LOG_DIR__|$LOG_DIR|g" \
    "$ESRGAN_TEMPLATE" >"$ESRGAN_DEST"
  launchctl bootstrap "$DOMAIN" "$ESRGAN_DEST"
  if launchctl print "$DOMAIN/$ESRGAN_LABEL" >/dev/null 2>&1; then
    echo "installed: $ESRGAN_DEST"
    echo "running  : $DOMAIN/$ESRGAN_LABEL"
    echo "logs     : $LOG_DIR/lattice-esrgan.{out,err}.log"
  else
    echo "warning: esrgan bootstrap failed — check $LOG_DIR/lattice-esrgan.err.log" >&2
  fi
else
  echo "note: realesrgan-mlx not found at $ESRGAN_BIN — skipping the esrgan sidecar."
fi

# The IOGPU wired-memory-limit daemon is a LaunchDaemon (system domain, root), so it is
# installed by its own sudo script rather than here — this script runs without sudo.
#   sudo ./deploy/install-iogpu.sh
