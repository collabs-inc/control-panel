#!/bin/sh
set -eu
panel_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec "$panel_dir/control-panel" -insecure-no-access -embed-local \
  -addr 127.0.0.1:7777 \
  -pin-file "$panel_dir/.local-state/pin" \
  -audit-file "$panel_dir/.local-state/audit.jsonl" "$@"
