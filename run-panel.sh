#!/bin/bash
# The control panel in the foreground, for a zellij tab (box-servers): its log scrolls right there.
# It rebuilds and comes back whenever it exits, so an update (./ensure-panel.sh restart kills it) takes
# a second and nobody has to type in this tab. Ctrl-C stops it for good.
cd /workspace/home/projects/control-panel || exit 1
unset HTTPS_PROXY https_proxy HTTP_PROXY http_proxy # the Access cert fetch must go direct
. ./panel.env

# take over from a background copy that ensure-panel.sh started (a login can start one whenever the panel
# is down for a moment); a copy started here never writes the pidfile, so only that one is stopped
takeover() {
  p=$(cat /workspace/home/projects/control-panel/run/panel.pid 2>/dev/null)
  [ -n "$p" ] && [ "$(cat /proc/$p/comm 2>/dev/null)" = control-panel ] && tr '\0' ' ' < /proc/$p/cmdline | grep -q -- "-cf-team" && kill "$p" && sleep 0.5
}
while true; do
  go build -o control-panel . || { echo "control-panel: build failed, retrying in 10 s"; sleep 10; continue; }
  takeover
  ./control-panel -cf-team "$CF_TEAM" -cf-aud "$CF_AUD"
  echo "control-panel exited ($?): back in 1 s (Ctrl-C to stop)"
  sleep 1
done
