#!/bin/sh
set -eu
export HOME=/workspace/home/.local/share/control-panel-2026-10-20/home
mkdir -p "$HOME"
exec ./control-panel -behind-cube -addr "127.0.0.1:$PORT" -agent-addr '' -compact-sessions '^$'
