#!/bin/bash
# The Cloudflare tunnel (panel.alven.md -> the panel on 127.0.0.1:7777) in the foreground, for a zellij
# tab beside run-panel.sh; it comes back if it exits. Ctrl-C stops it for good.
unset HTTPS_PROXY https_proxy HTTP_PROXY http_proxy # the tunnel must go direct

# take over from a background copy that ensure-panel.sh started (a login can start one whenever the tunnel
# is down for a moment); a copy started here never writes the pidfile, so only that one is stopped
takeover() {
  p=$(cat /workspace/home/projects/control-panel/run/tunnel.pid 2>/dev/null)
  [ -n "$p" ] && [ "$(cat /proc/$p/comm 2>/dev/null)" = cloudflared ] && tr '\0' ' ' < /proc/$p/cmdline | grep -q -- "control-panel.token" && kill "$p" && sleep 0.5
}
while true; do
  takeover
  cloudflared tunnel --no-autoupdate --loglevel warn run --token-file "$HOME/.cloudflared/control-panel.token" --url http://127.0.0.1:7777
  echo "tunnel exited ($?): back in 2 s (Ctrl-C to stop)"
  sleep 2
done
