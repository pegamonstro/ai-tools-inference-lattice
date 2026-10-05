#!/usr/bin/env bash
# Install the IOGPU wired-memory-limit LaunchDaemon (system domain, needs root).
#
# Unlike the per-user agents in install-macos.sh, this is a LaunchDaemon: it sets a
# system-wide sysctl (iogpu.wired_limit_mb) once at boot, which requires root and
# therefore the system launchd domain rather than gui/$UID. It is a separate script
# because install-macos.sh runs without sudo.
#
#   sudo ./deploy/install-iogpu.sh
#   sudo IOGPU_WIRED_LIMIT_MB=24576 ./deploy/install-iogpu.sh   # conservative override
#
# 28672 MiB (28 GiB) on a 32 GiB host leaves ~4 GiB for the OS and non-GPU processes.
# Lower it (e.g. 24576 = 24 GiB) if you see memory-pressure instability under load.
set -euo pipefail

LABEL="com.lattice.iogpu"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$REPO_ROOT/deploy/$LABEL.plist.in"
DEST="/Library/LaunchDaemons/$LABEL.plist"
WIRED_LIMIT_MB="${IOGPU_WIRED_LIMIT_MB:-28672}"

if [[ $EUID -ne 0 ]]; then
  echo "error: installs a LaunchDaemon and sets a system sysctl — run with sudo." >&2
  exit 1
fi

# Re-install picks up template changes; "not loaded" is fine.
launchctl bootout "system/$LABEL" 2>/dev/null || true

sed "s|__IOGPU_WIRED_LIMIT_MB__|$WIRED_LIMIT_MB|g" "$TEMPLATE" > "$DEST"
chown root:wheel "$DEST"
chmod 644 "$DEST"

launchctl bootstrap system "$DEST"

# Apply it now too, so the ceiling is raised without waiting for a reboot.
sysctl -w "iogpu.wired_limit_mb=$WIRED_LIMIT_MB" >/dev/null

if launchctl print "system/$LABEL" >/dev/null 2>&1; then
  echo "installed: $DEST"
  echo "running  : system/$LABEL"
  echo "wired_limit_mb: $(sysctl -n iogpu.wired_limit_mb)"
else
  echo "error: bootstrap failed — check the plist at $DEST" >&2
  exit 1
fi
