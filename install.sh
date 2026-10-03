#!/bin/sh
# Cube runs this in the app's folder after it installs or updates the panel: build the binary that cube.json starts.
set -eu
if ! command -v go >/dev/null 2>&1; then
  toolchain="$HOME/.local/share/control-panel-toolchain/go/bin"
  if [ -x "$toolchain/go" ]; then
    PATH="$toolchain:$PATH"
    export PATH
  else
    echo "Go is required to build the control panel: install Go 1.27 or later, then press Retry." >&2
    exit 1
  fi
fi
go version
go build -o control-panel .
echo "built $(pwd)/control-panel"
