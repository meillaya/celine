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
SYSTEM_DIR="${CELINE_SYSTEMD_DIR:-/etc/systemd/system}"
CELINE_BIN="${CELINE_BIN:-}"
CONFIG=""
MODE="install"
DRY_RUN=0
ENABLE_LINGER=0
ENABLE_PODMAN=0
UNIT_DIR=""
UNIT_FILES=()
ENABLE_UNITS=()

usage() {
  cat <<'EOF'
Usage: scripts/install-boot.sh [options]

Install, enable, or remove the Celine systemd units on a rootless Podman host.
Run as the operator account, not as root; the script calls sudo internally.

Options:
  --dry-run          Render into a temporary directory and print the actions
  --uninstall        Disable and remove the installed units
  --enable-linger    Enable systemd user lingering (sudo loginctl enable-linger)
  --enable-podman    Enable the rootless podman.socket user unit
  --celine PATH      Path to the celine binary (default: build/celine, result/bin/celine, PATH;
                     when missing, the script builds build/celine with go if available)
  --config PATH      Path to celine.toml (default: discovered next to the repository)
  -h, --help         Show this help

Environment:
  CELINE_BIN         Same as --celine
  CELINE_SYSTEMD_DIR Install target (default: /etc/systemd/system)
  DOCKER_HOST        Rootless Podman socket, for example unix:///run/user/1000/podman/podman.sock
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

warn() {
  printf 'warning: %s\n' "$*" >&2
}

cleanup() {
  if [ -n "$UNIT_DIR" ] && [ -d "$UNIT_DIR" ]; then
    rm -rf -- "$UNIT_DIR"
  fi
}
trap cleanup EXIT

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
    --enable-linger)
      ENABLE_LINGER=1
      shift
      ;;
    --enable-podman)
      ENABLE_PODMAN=1
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
  if [ "$DRY_RUN" -eq 0 ]; then
    sudo -v
  fi
  # Disable boot startup without stopping the running stack. Stopping
  # celine.service would run its ExecStop and stop the containers.
  run sudo systemctl disable celine.service celine-backup.timer || true
  run sudo systemctl stop celine-backup.timer || true
  for unit in celine.service celine-backup.service celine-backup.timer; do
    run sudo rm -f -- "$SYSTEM_DIR/$unit"
  done
  run sudo systemctl daemon-reload
  printf '\nBoot persistence removed; the running containers were left untouched.\n'
  printf 'Stop them with: %s down\n' "$CELINE_BIN"
  exit 0
fi

printf 'Rendering units with %s\n' "$CELINE_BIN"
UNIT_DIR=$(mktemp -d "${TMPDIR:-/tmp}/celine-units.XXXXXX")
(cd "$ROOT" && "$CELINE_BIN" systemd render --out "$UNIT_DIR" --celine "$CELINE_BIN" "${CONFIG_ARGS[@]}")

for unit in celine.service celine-backup.service celine-backup.timer; do
  if [ -f "$UNIT_DIR/$unit" ]; then
    UNIT_FILES+=("$unit")
  fi
done
[ -f "$UNIT_DIR/celine.service" ] || fail "render did not produce celine.service"
ENABLE_UNITS=(celine.service)
if [ -f "$UNIT_DIR/celine-backup.timer" ]; then
  ENABLE_UNITS+=(celine-backup.timer)
fi

if [ "$DRY_RUN" -eq 0 ]; then
  sudo -v
fi

check_boot_mount() {
  local target
  target=$(findmnt -T "$ROOT" -n -o TARGET 2>/dev/null | head -n1 || true)
  if [ -n "$target" ] && [ "$target" != "/" ]; then
    if grep -qsF -- "$target" /etc/fstab; then
      printf '  mount: %s is listed in /etc/fstab\n' "$target"
    else
      warn "repository is on mount $target, which is not in /etc/fstab; it may appear after celine.service starts"
      printf '  the unit now waits up to 20 minutes for the deployment paths; add an fstab/systemd mount entry for unattended boot\n'
    fi
  fi
}

check_prereqs() {
  local operator
  operator=$(id -un)
  if command -v loginctl >/dev/null 2>&1; then
    local linger
    linger=$(loginctl show-user "$operator" -p Linger 2>/dev/null || true)
    case "$linger" in
      *Linger=yes*)
        printf '  linger: enabled for %s\n' "$operator"
        ;;
      *)
        warn "user lingering is disabled; a system unit cannot reach the rootless Podman socket before login"
        if [ "$ENABLE_LINGER" -eq 1 ]; then
          run sudo loginctl enable-linger "$operator"
        else
          printf '  fix: sudo loginctl enable-linger %s (or rerun with --enable-linger)\n' "$operator"
        fi
        ;;
    esac
  fi

  if command -v systemctl >/dev/null 2>&1 && systemctl --user is-enabled podman.socket >/dev/null 2>&1; then
    printf '  podman.socket: enabled for %s\n' "$operator"
  else
    warn "podman.socket is not enabled in the user manager"
    if [ "$ENABLE_PODMAN" -eq 1 ]; then
      run systemctl --user enable --now podman.socket
    else
      printf '  fix: systemctl --user enable --now podman.socket (or rerun with --enable-podman)\n'
    fi
  fi

  if [ -n "${DOCKER_HOST:-}" ] && [[ "${DOCKER_HOST}" == unix://* ]]; then
    local socket="${DOCKER_HOST#unix://}"
    if [ ! -S "$socket" ]; then
      warn "DOCKER_HOST socket $socket does not exist yet; start podman.socket before the service"
    fi
  fi
}

printf '\nChecking rootless prerequisites\n'
check_prereqs
printf '\nChecking the repository mount\n'
check_boot_mount

if [ ! -f "$UNIT_DIR/celine-backup.timer" ]; then
  # backup.enable is false: remove units left by an earlier install instead of
  # leaving an enabled timer that points at a unit this render no longer owns.
  run sudo systemctl disable --now celine-backup.timer || true
  run sudo rm -f -- "$SYSTEM_DIR/celine-backup.service" "$SYSTEM_DIR/celine-backup.timer"
fi

printf '\nInstalling units into %s\n' "$SYSTEM_DIR"
run sudo install -d -m755 "$SYSTEM_DIR"
for unit in "${UNIT_FILES[@]}"; do
  run sudo install -m644 "$UNIT_DIR/$unit" "$SYSTEM_DIR/$unit"
done
run sudo systemctl daemon-reload
# A failed boot can leave the unit in start-limit-hit. Clear that before
# enabling/starting so this run actually retries.
run sudo systemctl reset-failed celine.service || true
run sudo systemctl enable --now "${ENABLE_UNITS[@]}"

if [ "$DRY_RUN" -eq 1 ]; then
  printf '\nDry run complete; nothing was installed.\n'
  exit 0
fi

printf '\nBoot persistence installed.\n'
printf '  check:   systemctl status celine.service\n'
if [ -f "$SYSTEM_DIR/celine-backup.timer" ]; then
  printf '  timer:   systemctl list-timers celine-backup.timer\n'
fi
printf '  logs:    journalctl -u celine.service -u celine-backup.service\n'
printf '  restart: sudo systemctl restart celine.service\n'
printf '  verify:  %s doctor --strict --expect-running\n' "$CELINE_BIN"
printf '\nAfter editing paths, units, or DOCKER_HOST, rerun this script and restart celine.service.\n'
