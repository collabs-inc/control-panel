#!/bin/bash
# Keep the control panel and its Cloudflare tunnel alive. No systemd/cron on Cube:
# called from ~/.zshrc on every login and safe to run any time (idempotent, pidfile-guarded).
#   ./ensure-panel.sh           start whatever isn't running
#   ./ensure-panel.sh restart   rebuild + restart the panel (tunnel stays up)
cd /workspace/home/projects/control-panel || exit 1
# agent sessions export a local HTTPS proxy (teamclaude); the tunnel and the Access cert fetch must go direct
unset HTTPS_PROXY https_proxy HTTP_PROXY http_proxy
TOKEN="$HOME/.cloudflared/control-panel.token"   # `cloudflared tunnel token control-panel` (mode 600)
[ -f panel.env ] && [ -f "$TOKEN" ] || { echo "control-panel: needs panel.env (CF_TEAM, CF_AUD) and $TOKEN"; exit 0; }
. ./panel.env
mkdir -p run

# alive NAME PATTERN: the pidfile's process is running AND is ours (pids get reused fast here)
alive() {
  pid=$(cat "run/$1.pid" 2>/dev/null)
  [ -n "$pid" ] && tr '\0' ' ' 2>/dev/null < "/proc/$pid/cmdline" | grep -q -- "$2"
}

if [ "$1" = restart ]; then
  go build -o control-panel . || exit 1
  alive panel "control-panel -cf-team" && kill "$pid" && sleep 1
fi
[ -x control-panel ] || go build -o control-panel . || exit 1

start() {
  name=$1; pattern=$2; shift 2
  alive "$name" "$pattern" && return
  # all three fds redirected BEFORE the fork, so an ssh login that runs this is never held open
  nohup "$@" >> "run/$name.log" 2>&1 < /dev/null &
  echo $! > "run/$name.pid"
  echo "started $name (pid $!)"
}
start panel "control-panel -cf-team" ./control-panel -cf-team "$CF_TEAM" -cf-aud "$CF_AUD"
# tunnel "control-panel" (panel.alven.md) → the panel on loopback
start tunnel "control-panel.token" cloudflared tunnel --no-autoupdate --loglevel warn run --token-file "$TOKEN" --url http://127.0.0.1:7777
