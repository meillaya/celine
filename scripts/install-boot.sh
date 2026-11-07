#!/usr/bin/env bash
# Install or remove the Celine boot units on a host that runs the stack with
# rootless Podman.
#
# Run this as the operator account that owns the Podman socket. The script uses
# sudo only for the system directories and systemctl, because the rendered units
# carry User=<operator> and must be rendered with the operator's uid and
# DOCKER_HOST.
set -euo pipefail

ROOT=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
UNIT_DIR="$ROOT/build/systemd"
SYSTEM_DIR="${CELINE_SYSTEMD_DIR:-/etc/systemd/system}"
CELINE_BIN="${CELINE_BIN:-}"
CONFIG=""
MODE="install"
DRY_RUN=0

UNIT_FILES=(
  "celine.service"
  "celine-backup.service"
  "celine-backup.timer"
)

usage() {
  cat <<'EOF'
Usage: scripts/install-boot.sh [options]

Install, enable, or remove the Celine systemd units on a rootless Podman host.
Run as the operator account, not as root; the script calls sudo internally.

Options:
  --dry-run          Render the units and print the install commands, write nothing
  --uninstall        Disable and remove the installed units
  --celine PATH      Path to the celine binary (default: build/celine, result/bin/celine, PATH;
                     when missing, the script builds build/celine with go if available)
  --config PATH      Path to celine.toml (default: discovered next to the repository)
  -h, --help         Show this help

Environment:
  CELINE_BIN         Same as --celine
  CELINE_SYSTEMD_DIR Install target (default: /etc/systemd/system)
EOF
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

run() {
  printf '  %s\n' "$*"
  if [ "$DRY_RUN" -eq 0 ]; then
    "$@"
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --uninstall)
      MODE="uninstall"
      shift
      ;;
    --celine)
      [ $# -ge 2 ] || fail "--celine needs a path"
      CELINE_BIN="$2"
      shift 2
      ;;
    --config)
      [ $# -ge 2 ] || fail "--config needs a path"
      CONFIG="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown option: $1 (try --help)"
      ;;
  esac
done

case "$SYSTEM_DIR" in
  /*) ;;
  *) fail "CELINE_SYSTEMD_DIR must be an absolute path, got: $SYSTEM_DIR" ;;
esac

if [ "$EUID" -eq 0 ]; then
  fail "run this as the operator account, not as root; the units need the operator's uid and DOCKER_HOST"
fi
if ! command -v sudo >/dev/null 2>&1; then
  fail "sudo is required to install system units"
fi

CONFIG_ARGS=()
if [ -n "$CONFIG" ]; then
  CONFIG="$(readlink -f -- "$CONFIG")"
  [ -f "$CONFIG" ] || fail "config file not found: $CONFIG"
  CONFIG_ARGS=(--config "$CONFIG")
fi

choose_celine() {
  if [ -n "$CELINE_BIN" ]; then
    printf '%s\n' "$CELINE_BIN"
    return
  fi
  if [ -x "$ROOT/build/celine" ]; then
    printf '%s\n' "$ROOT/build/celine"
    return
  fi
  if [ -x "$ROOT/result/bin/celine" ]; then
    printf '%s\n' "$ROOT/result/bin/celine"
    return
  fi
  if command -v celine >/dev/null 2>&1 && [ -x "$(command -v celine)" ]; then
    command -v celine
    return
  fi
  if command -v go >/dev/null 2>&1; then
    printf 'Building %s/build/celine\n' "$ROOT" >&2
    (cd "$ROOT" && go build -o build/celine ./cmd/celine)
    printf '%s\n' "$ROOT/build/celine"
    return
  fi
  fail "celine binary not found; build it (go build -o build/celine ./cmd/celine), run from devenv shell, or pass --celine PATH"
}

CELINE_BIN=$(readlink -f -- "$(choose_celine)")
[ -x "$CELINE_BIN" ] || fail "celine is not executable: $CELINE_BIN"
"$CELINE_BIN" version >/dev/null 2>&1 || fail "cannot run celine at $CELINE_BIN"

if [ "$MODE" = "uninstall" ]; then
  printf 'Removing Celine boot units from %s\n' "$SYSTEM_DIR"
  # Disable boot startup without stopping the running stack. Stopping
  # celine.service would run its ExecStop and stop the containers.
  run sudo systemctl disable celine.service celine-backup.timer || true
  run sudo systemctl stop celine-backup.timer || true
  for unit in "${UNIT_FILES[@]}"; do
    run sudo rm -f -- "$SYSTEM_DIR/$unit"
  done
  run sudo systemctl daemon-reload
  printf '\nBoot persistence removed; the running containers were left untouched.\n'
  printf 'Stop them with: %s down\n' "$CELINE_BIN"
  exit 0
fi

printf 'Rendering units with %s\n' "$CELINE_BIN"
mkdir -p "$UNIT_DIR"
(cd "$ROOT" && "$CELINE_BIN" systemd render --out "$UNIT_DIR" --celine "$CELINE_BIN" "${CONFIG_ARGS[@]}")
for unit in "${UNIT_FILES[@]}"; do
  [ -f "$UNIT_DIR/$unit" ] || fail "render did not produce $UNIT_DIR/$unit"
done

if [ "$DRY_RUN" -eq 0 ]; then
  sudo -v
fi

printf '\nInstalling units into %s\n' "$SYSTEM_DIR"
run sudo install -d -m755 "$SYSTEM_DIR"
for unit in "${UNIT_FILES[@]}"; do
  run sudo install -m644 "$UNIT_DIR/$unit" "$SYSTEM_DIR/$unit"
done
run sudo systemctl daemon-reload
run sudo systemctl enable --now celine.service celine-backup.timer

if [ "$DRY_RUN" -eq 1 ]; then
  printf '\nDry run complete; nothing was installed.\n'
  exit 0
fi

printf '\nBoot persistence installed.\n'
printf '  check:   systemctl status celine.service celine-backup.timer\n'
printf '  logs:    journalctl -u celine.service -u celine-backup.service\n'
printf '  restart: sudo systemctl restart celine.service\n'
printf '\nAfter editing paths, units, or DOCKER_HOST, rerun this script and restart celine.service.\n'
