# Control Panel — Design

Date: 2026-09-29 · Status: approved in chat, pending spec review

## Intent

One fast, secure web panel, running on this Cube VM, that shows everything happening on the
machine and allows a small set of safe actions. Reachable from the owner's phone and desktop
from anywhere. Single user.

Success looks like:

- Opening the panel on a phone shows live CPU/RAM/disk, CI runner health, and which agents
  need attention within a second.
- Nobody but the owner can reach even the login page, and a leaked session cannot run
  arbitrary commands.
- The panel costs almost nothing when nobody is looking at it.

What the owner stated: resources, CI health/stats, zellij agent sessions (like
pixel-agents-standalone / AgentTAB), security posture, future jobs (scripts, long Claude
loops); reachable from anywhere; PIN only the owner knows; phone + desktop; modern and fast.

Decided in brainstorming: Cloudflare Tunnel + Access + PIN; small fixed action set; Go
stdlib + vanilla JS.

## Environment facts (measured 2026-09-29)

- Debian 12, 10 cores, 31 GiB RAM, 4 GiB swap, `/workspace` 492 GB. Everything runs as
  `node` (uid 1000). No root, no sudo, no systemd, no cron.
- Consequences: no firewall changes; **per-process bandwidth is not measurable** (needs
  root/eBPF). VM-wide `eth0` counters from `/proc/net/dev` are available.
- CI: 10 GitHub Actions self-hosted runners in `/workspace/home/projects/ci-cd-jobs/`
  (`r1`–`r6`, `fe1`–`fe4`) for `roomss-ltd/alven-estate-agent`. Each has `runner.pid` and
  `runner.log` with lines:
  - `2026-09-29 19:19:15Z: Running job: <name>`
  - `2026-09-29 19:19:56Z: Job <name> completed with result: <Succeeded|Failed|Canceled…>`
  - Kept alive by `ensure-runners.sh`, called from `~/.zshrc`.
- Agents: zellij sessions from `zellij list-sessions`. The `claude-tab-status` plugin writes
  `/tmp/claude-tab-status/<session>.json`:
  `{"sessions":[{pane_id, run_id, tab_num, tab_name, icon, detail, activity, cwd, agent_kind,
  agent_title, active_subagents, parent_stopped}], "counts":{active,waiting,done}, "updated_at"}`.
  Stale files for dead sessions remain on disk.
- Listening beyond localhost: `0.0.0.0:8080` (Cube daemon `cubed`), `*:22022` (ssh).
  Everything else is on `127.0.0.1`.
- `cloudflared` 2026.9.3 is installed at `~/.local/bin/cloudflared`; no tunnel is configured.
- Go 1.27.1 is available.

## Architecture

```
phone/laptop ─HTTPS─▶ Cloudflare Access (email OTP, owner's email only)
             ─▶ cloudflared named tunnel (outbound from VM, no inbound port)
             ─▶ control-panel  127.0.0.1:7777  (never 0.0.0.0)
```

- A single Go binary that uses only the standard library. The UI (`web/`) is embedded with
  `go:embed`, gzip-compressed, and served with content-hashed names and immutable caching.
- A single sampler loop runs **only while at least one client is connected**. Sampling
  cadence:

  | Source | Cadence |
  |---|---|
  | `/proc` | 2 s |
  | agent status files | 2 s |
  | runner logs (read incrementally from the saved offset) | 5 s |
  | security scan | 60 s |
  | directory sizes | 10 min |

- Live data is sent over Server-Sent Events. `GET /api/stream?view=<panel>` sends a full
  snapshot, then changes every tick. It sends the active panel's detail plus a small
  `badges` summary for the tab bar. The client reconnects with a new `view` when the tab
  changes.
- `ensure-panel.sh` is idempotent and pidfile-guarded (same pattern as `ensure-runners.sh`).
  It keeps the panel and `cloudflared tunnel run` alive, and is called from `~/.zshrc`.

## Security

1. **Cloudflare Access** is an application on `panel.<owner-domain>`. Its policy is: allow
   only the owner's email, one-time PIN login. Unauthenticated traffic never reaches the
   VM.
2. **Access JWT verification.** Every request must carry a valid
   `Cf-Access-Jwt-Assertion` header:
   - RS256 signature verified against `https://<team>.cloudflareaccess.com/cdn-cgi/access/certs`
     (cached for 1 h, refetched when a `kid` is unknown);
   - `aud` equal to the application AUD tag;
   - `iss` equal to `https://<team>.cloudflareaccess.com`;
   - `exp` in the future.

   Anything else gets a 403. This also blocks other processes on the VM that hit
   `127.0.0.1:7777` directly. The team domain and AUD are passed as flags; they are not
   secrets.
3. **PIN** (at least 6 digits):
   - Set on the VM with `control-panel set-pin`, which reads the PIN from the terminal
     without echo.
   - Stored as PBKDF2-SHA256 (`crypto/pbkdf2`) with a random salt and 600k iterations in
     `~/.config/control-panel/pin`, mode 0600.
   - Compared in constant time.
   - Never stored in the repo, environment variables, flags or logs.
4. **Lockout.** A global failure counter: 5 wrong PINs lock login for 15 minutes. Lockouts
   are recorded in the audit log and shown on the Security panel.
5. **Session.**
   - The token is 32 bytes from `crypto/rand`, kept in memory as a map from token to expiry.
   - Cookie flags: `HttpOnly; Secure; SameSite=Strict; Path=/`.
   - Idle timeout 12 h. A restart logs everyone out.
6. **Actions.**
   - Available only through `POST /api/action/<name>`.
   - Each request needs: a valid session, the PIN re-entered in the body, and an `Origin`
     that matches the panel host.
   - Actions come from a hardcoded allowlist, and there is no free-form command path.
7. **Headers.**
   - `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'` (no inline
     script or style);
   - `X-Content-Type-Options: nosniff`;
   - `Referrer-Policy: no-referrer`.
8. **Audit log.** Append-only JSON lines in `~/.local/state/control-panel/audit.jsonl`.
   Each line records: time, event (login ok/fail, lockout, action, jwt reject), and detail.
   The Cloudflare `CF-Connecting-IP` is included when present.

## Panels

The layout is a bottom tab bar on phones and a left sidebar at ≥ 900 px. The tab bar shows
badges: agents waiting, CI failures today, and security reds. Dark theme. A web app
manifest makes it installable to the home screen; there is no service worker.

### Resources

- **CPU:** total and per core from `/proc/stat` deltas; load average from `/proc/loadavg`.
- **Memory:** RAM and swap from `/proc/meminfo`.
- **Disk:** per mount via `syscall.Statfs` on real mounts from `/proc/mounts`.
- **Network:** `eth0` rx/tx bytes per second from `/proc/net/dev`, with a 1 h sparkline
  (in-memory ring buffer).
- **Top processes:**
  - Columns: CPU %, RSS, and disk read/write per second (`/proc/<pid>/stat`, `statm`, `io`),
    plus a count of open TCP sockets.
  - Grouped by app. The group key is the runner directory for CI processes, otherwise the
    executable name.
  - Groups can be expanded to show individual processes, and the table can be sorted.
- **Largest directories:** under `/workspace/home`, depth 2, top 15. Computed by a
  low-priority walk every 10 minutes.
- **Bandwidth per process:** not shown, because it cannot be measured without root. The
  note "per-process bandwidth needs root" appears on the card.
- **Action:** kill a process (see Actions).

### CI

- **Runner cards:**
  - `busy: <job>`: a "Running job" line with no completion yet.
  - `idle`.
  - `dead`: `runner.pid` is not alive.
  - Each card also shows uptime.
- **Totals** for today, the last 7 days and all time: jobs, runner-minutes and success
  rate.
- **Per job name:** count, success rate and average duration.
- **Recent jobs:** the last 50, with runner, name, result, duration and start time.
- **Log reading:** offsets are kept in memory and the first read is a full scan.
  `runner.log` is small (≈12 KB after 3 days).
- **Action:** restart a runner.

### Agents

- **Live sessions only:** `zellij list-sessions --no-formatting` every 10 s. Status files for
  other sessions are ignored.
- **Contents:** session → tab (`tab_num`, `tab_name`) → pane. Each pane shows agent kind,
  activity (thinking / tool / waiting / done / idle), `detail`, `cwd` and subagent count.
- **Waiting panes** are highlighted and sorted to the top.
- **Zellij calls:** no zellij CLI calls apart from `list-sessions` and the focus action.
  This respects the pixel-agents rule that every CLI call makes zellij re-broadcast its
  state.
- **Action:** focus pane.

### Security

The result is a checklist marked green, amber or red. There is no numeric score.

- **Listening sockets** from `/proc/net/tcp{,6}`, with the owning process (socket inode →
  `/proc/<pid>/fd`). A socket bound beyond loopback is amber. The panel's own port must be
  on loopback, otherwise it is red.
- **Established connections from public IP addresses:** the remote address, the local port
  and the process.
- **`~/.ssh/authorized_keys`:** key count and SHA256 fingerprints.
- **World-readable secrets:** a scan of `~/.ssh`, `~/.config`, `~/.claude`, and the runner
  `.credentials`/`.env`/`.runner` files. Any file with mode `o+r` is amber. This flags the
  runners' `.credentials` today, which are mode 0644.
- **Tunnel health:** whether the `cloudflared` process is alive.
- **Panel audit:** the last 50 audit events, and failed PIN attempts in the last 24 h.

## Actions (the allowlist)

| Name | Input | Behaviour | Guards |
|---|---|---|---|
| `kill` | pid | SIGTERM, then SIGKILL after 5 s if still alive | Must be uid 1000. Refuses PID 1, the panel itself, `cloudflared`, `cubed`, and the zellij server. |
| `restart-runner` | runner dir name | SIGTERM the process tree under `runner.pid` (walk `/proc/*/stat` ppid; never the process group, which may be the login shell's), then run `ensure-runners.sh` | The name must match `^(r\|fe)[0-9]+$` and the directory must exist. The UI warns if the runner is busy. |
| `focus-pane` | session, pane_id | `zellij --session <s> action focus-pane-id <id>` | The session must be live and the pane must be in its status file. Arguments are passed as an argv list, never through a shell. |

## Errors

- Each data source fails independently. Its card shows `unavailable: <reason>` and the other
  panels keep working.
- If the SSE connection drops, the browser reconnects automatically and a "reconnecting"
  badge is shown.
- An action failure returns a message that is shown as a toast. It is also recorded in the
  audit log.

## Performance budget

- UI total ≤ 50 KB gzipped. No framework and no chart library: sparklines are drawn on a
  `<canvas>`.
- Server RSS ≤ 20 MB when idle, and 0 % CPU with no clients connected.
- < 1 % of one core while sampling with a client connected.
- One tick of about 500 processes takes < 10 ms.

## Code layout

```
control-panel/
  main.go          flags, routes, SSE hub, sampler loop, set-pin subcommand
  auth.go          Access JWT verify, PIN hash/verify, sessions, lockout, audit log
  sys.go           /proc CPU, memory, disk, network, processes, directory sizes
  ci.go            runner log parsing, runner state, stats
  agents.go        zellij sessions + status files
  sec.go           sockets, connections, keys, permission scan
  actions.go       allowlist + guards
  *_test.go        parser/auth/guard tests with inline fixtures
  web/index.html, web/app.js, web/app.css, web/manifest.json, web/icon.svg
  ensure-panel.sh  keep panel + cloudflared alive
```

## Testing

- Plain `go test` (standard library only).
- **Parsers:** fixtures captured from this VM (`/proc/stat`, `/proc/<pid>/stat`,
  `/proc/net/tcp`, `runner.log`, the status JSON).
- **Auth:**
  - PIN hash round-trip, and a wrong PIN is rejected;
  - lockout after 5 failures and reset after the window;
  - JWT rejected for a bad signature, wrong `aud`, expired `exp`, or unknown `kid` (test
    RSA key generated in the test);
  - an action without the PIN, or with a wrong `Origin`, is rejected.
- **Action guards:** the kill denylist, the runner-name regex, and a focus-pane on an
  unknown pane.
- **End to end:** before calling it done, `curl` through the real tunnel. Expect a 403
  without Access, a redirect or login challenge with Access, and a working stream after the
  PIN.

## Setup (done 2026-09-29)

- **Tunnel.** Tunnel `control-panel` (id `e5e3e914-…`) was created with `cloudflared tunnel login` for
  the alven.md zone, then `tunnel create` and `tunnel route dns control-panel panel.alven.md`.
  Its token is in `~/.cloudflared/control-panel.token` (mode 600).
- **Run command.** `ensure-panel.sh` runs the tunnel as
  `cloudflared tunnel run --token-file … --url http://127.0.0.1:7777`.
- **Access.** Access app "Control Panel" covers `panel.alven.md`. Team `alven-ai`; the AUD tag is in
  `panel.env`.
- **Autostart.** `~/.zshrc` runs `ensure-panel.sh` next to `ensure-runners.sh`.
- **Proxy.** CLI calls must bypass the teamclaude HTTPS proxy that agent shells export
  (`env -u HTTPS_PROXY …`). `ensure-panel.sh` unsets it.
- **PIN.** The owner sets the PIN with `control-panel set-pin`.

## Out of scope for v1

- Registry and controls for future jobs (scripts, long Claude loops). They are visible in
  Resources in the meantime. Add the registry when the first such job exists.
- Alerts and push notifications.
- History beyond 1 h in memory.
- GitHub API data (queue depth, workflow runs).
- Sending input to agent panes, and a web terminal.
- Multiple users.
