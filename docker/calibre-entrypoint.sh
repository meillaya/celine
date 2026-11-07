#!/bin/sh
# Keep every mutable Calibre path inside the mounted config directory and
# replace the shell with the server so signals reach it directly.
set -eu

config_dir="${CALIBRE_CONFIG_DIRECTORY:-/config}"
mkdir -p "$config_dir"

exec /opt/calibre/calibre-server "$@"
