# Control Panel v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a single-binary, PIN-protected web panel that monitors this VM's resources,
CI runners, zellij agents and security posture. It performs three allowlisted actions and is
reached through Cloudflare Tunnel + Access.

**Architecture:**
- One Go package (standard library only) that serves an embedded vanilla-JS UI.
- A sampler reads `/proc`, runner logs, zellij status files and sockets, but only while an
  SSE client is connected.
- The auth chain is: Cloudflare Access JWT, then PIN, then session cookie. Actions re-check
  the PIN.

**Tech Stack:** Go 1.27 stdlib (`net/http`, `crypto/pbkdf2`, `crypto/rsa`, `net/netip`,
`embed`), vanilla JS/CSS, `cloudflared`.

**Spec:** `docs/superpowers/specs/2026-09-29-control-panel-design.md`

**Owner rule:** this directory is NOT a git repo. Every "commit" step is replaced by
"run `go test ./...`".

## Global Constraints

- Standard library only: no `go get` and no third-party JS/CSS.
- Listen address `127.0.0.1:7777`. The server refuses to start on a non-loopback address.
- PIN: 6–32 digits, stored as PBKDF2-SHA256 with 600000 iterations in
  `~/.config/control-panel/pin` (mode 0600). It is never placed in flags, env or logs.
- Lockout: 5 wrong PINs → 15 min. Session idle timeout: 12 h. Cookie:
  `HttpOnly; Secure; SameSite=Strict; Path=/`.
- CSP: `default-src 'self'; frame-ancestors 'none'`. No inline script or style (set widths
  via CSSOM).
- Sampler cadence:

  | Source | Cadence |
  |---|---|
  | `/proc` | 2 s |
  | agent status files | 2 s |
  | `zellij list-sessions` | 10 s |
  | fd/socket scan | 10 s |
  | CI logs | 5 s |
  | security | 60 s |
  | `du` | 10 min, and only while the Resources view is open |

  The history ring holds cheap totals every 10 s, always.
- Zellij CLI calls: `list-sessions` and `focus-pane-id` only.
- UI ≤ 50 KB gzipped. Server idle RSS ≤ 20 MB.

## Review Focus

1. A session that expires while the SSE stream is open → the UI must return to the PIN
   screen, not show "reconnecting" forever. (Task 8: `/api/me` check on stream error.)
2. A runner killed mid-job leaves a `Running job` line with no completion → it must not show
   busy forever. The job is closed as `Interrupted` when the next `Running job` appears or
   when the runner is dead. (Task 3 test.)
3. A process exits between listing `/proc` and reading its files → the process is skipped
   and the tick continues. (Task 2 test: missing pid dir.)
4. PIN failures through actions count toward the same lockout as login. (Task 1 test.)
5. zellij not running, no status files, or stale files for dead sessions → the Agents panel
   shows an empty state. (Task 4 test.)
6. PID reuse between SIGTERM and the 5 s SIGKILL → re-check the process start time before
   SIGKILL. (Task 6.)

---

### Task 1: Auth core — `auth.go`, `auth_test.go`

**Produces:**
- `hashPIN(pin string, iter int) (string, error)`: returns
  `pbkdf2-sha256$<iter>$<b64 salt>$<b64 key>`.
- `verifyPIN(encoded, pin string) bool`: constant-time comparison.
- `validPIN(pin string) bool`: 6–32 ASCII digits.
- `type auditEvent struct{T int64; Event, Detail, IP string}`, plus:
  - `openAudit(path) *auditLog`: loads the last 200 lines;
  - `(*auditLog).add(event, detail, ip)`;
  - `(*auditLog).list() []auditEvent`: newest first.
- `type auth struct{pinFile string; audit *auditLog; now func() time.Time; ...}`:
  - `checkPIN(pin, ip string) error`: returns `errLocked`, `errBadPIN` or `errNoPIN`. It
    re-reads the pin file each call, serializes callers with a mutex, and uses one shared
    counter for login and actions;
  - `newSession(email) string`;
  - `validSession(token, email) bool`: extends the idle expiry;
  - `endSession(token)`;
  - `lockedFor() time.Duration`;
  - `fails24h() int`.
- `type access struct{team, aud string; fetch func() (map[string]*rsa.PublicKey, error); now func() time.Time}`:
  - `verify(tok string) (email string, err error)`: RS256 only, `kid` lookup, and a refetch
    on an unknown `kid` at most once per minute. `aud` must contain the configured AUD,
    `iss == https://<team>.cloudflareaccess.com`, and `exp`/`nbf` are checked with 60 s
    skew;
  - `fetchCerts(team)`: parses the JWKS (`n`, `e` base64url).

Tests:
- round-trip; wrong PIN; `validPIN` table;
- 5 failures → locked, even with the correct PIN;
- lock expiry with a fake clock;
- a failed action PIN counts toward the lockout;
- session idle expiry; session bound to email;
- JWT accepted when valid;
- JWT rejected for: bad signature, wrong `aud`, wrong `iss`, expired, alg `none`/`HS256`,
  unknown `kid`, malformed. The test RSA key is generated in the test.

- [ ] Write the tests, confirm they fail, implement, then `go test ./...` passes.

### Task 2: System sampler — `sys.go`, `sys_test.go`

**Produces:**
- Parsers:
  - `parseStat(b []byte) (procStat, bool)`: handles `comm` containing spaces or `)`;
  - `parseCPU(b []byte) []cpuTimes`: index 0 is the total;
  - `parseMeminfo(b []byte) map[string]uint64` (bytes);
  - `parseNetDev(b []byte) (rx, tx uint64)`: all interfaces except `lo`;
  - `parseProcNet(b []byte, proto string) []sockEntry`;
  - `parseDu(b []byte, root string) []DirSize`: top 15, root excluded.
- `type sysSampler`, with `sample(now time.Time, sock sockScan) Resources` and
  `history() []histPoint`.
- Process group key, cached per `(pid, starttime)`, first match wins:
  1. cmdline contains `/ci-cd-jobs/<r|feN>/` → `runner:<N>`;
  2. environ `RUNNER_NAME=cube-<N>` → `runner:<N>`;
  3. environ `ZELLIJ_SESSION_NAME` + `ZELLIJ_PANE_ID` → `pane:<session>:<id>`;
  4. otherwise `comm`.
- `scanSockets() sockScan{inodePID map[string]int; tcpByPID map[int]int; entries []sockEntry}`.
- `du`: `nice -n19 ionice -c3 du -x -d2 -B1 /workspace/home`, run in a goroutine; results
  are cached.

Tests:
- `parseStat` with `comm` `(a) b)`;
- CPU % from two `/proc/stat` fixtures;
- meminfo kB → bytes;
- net/dev skips `lo`;
- `/proc/net/tcp` fixture line (LISTEN `0100007F:1E61`) and a tcp6 line;
- `du` output parse and order;
- group-key rules from cmdline/environ fixtures;
- sampling a missing pid dir does not panic.

- [ ] Write the tests, confirm they fail, implement, then `go test ./...` passes.

### Task 3: CI tracker — `ci.go`, `ci_test.go`

**Produces:**
- `type Job{Runner, Name, Result string; Start, End int64}`.
- `type ciTracker{dir string; ...}`:
  - `poll(now time.Time, alive func(pid int) bool) CI`: reads `runner.log` incrementally,
    and resets if the file shrinks.
- `CI{Runners []Runner; Day, Week, All Totals; ByName []NameStat; Recent []Job; FailedDay int}`.
- `Runner{Name, State /*busy|idle|dead*/, Job string; Since int64; PID int}`.
- `feed(r *runnerState, name string, chunk []byte) (consumed int)`: consumes complete lines
  only.

Tests:
- start + completion → one job with duration;
- a partial last line is not consumed;
- start, start → the first job is `Interrupted`;
- totals over 24 h and 7 d windows;
- per-name success rate;
- a dead runner with an open job → state `dead`, job closed as `Interrupted`;
- a shrunk file resets the offset.

- [ ] Write the tests, confirm they fail, implement, then `go test ./...` passes.

### Task 4: Agents tracker — `agents.go`, `agents_test.go`

**Produces:**
- `parseSessions(out []byte) []string`: drops lines containing `EXITED`.
- `type agentsTracker{dir string; list func() ([]byte, error)}`:
  - `poll(now time.Time) Agents`: the session list is refreshed every 10 s; status files are
    read every call.
- `Agents{Sessions []ZSession; Waiting, Working int}`.
- `ZSession{Name string; Updated int64; Panes []Pane; Err string}`.
- `Pane`: mirrors the status JSON fields, plus `CPU float64`, `RSS uint64`, which are filled
  from `pane:<s>:<id>` groups.
- `(*agentsTracker).hasPane(session string, pane int) bool`.

Tests:
- EXITED filtered;
- stale file for a dead session ignored;
- missing directory → empty, no error;
- `Waiting` sorted first;
- counts.

- [ ] Write the tests, confirm they fail, implement, then `go test ./...` passes.

### Task 5: Security — `sec.go`, `sec_test.go`

**Produces:** `secScan(sock sockScan, procName func(pid int) string, env secEnv) Security`,
where:
- `secEnv{home, ciDir string; panelAddr string; accessOn, pinSet, tunnelUp bool; audit *auditLog; fails24h int}`;
- `Security{Checks []Check; Listens []Listen; Inbound []Conn; Outbound []RemoteCount; Keys []Key; Loose []string; Audit []auditEvent}`;
- `Check{Level, Title, Detail}`, where Level is `ok`, `warn` or `bad`.

Helpers:
- `fingerprint(line) (Key, bool)`: `SHA256:` + base64 (raw std) of sha256(blob);
- `looseFiles(paths []string) []string`: mode & 0o004;
- `isPublic(netip.Addr) bool`: not loopback, private, link-local or unspecified.

Checks:
- Access enforced (`bad` if off);
- panel on loopback;
- PIN set;
- tunnel process up;
- ports beyond loopback (`warn`, with the list);
- inbound public connections (`warn`);
- world-readable secrets (`warn`);
- failed PINs in 24 h (`warn` if > 0, `bad` if locked).

Tests:
- fingerprint of a known ed25519 line (compare with `ssh-keygen -lf` output captured in the
  test);
- `isPublic` table;
- `looseFiles` on temp files with 0644/0600;
- a listen on `0.0.0.0` is flagged.

- [ ] Write the tests, confirm they fail, implement, then `go test ./...` passes.

### Task 6: Actions — `actions.go`, `actions_test.go`

**Produces:** `type actor{ciDir string; self int; uid int; agents *agentsTracker; run func(dir, name string, args ...string) error}`:

- `kill(pid int) error`: SIGTERM, then SIGKILL after 5 s only if the `starttime` still
  matches.
- `restartRunner(name string) error`: SIGTERM the `runner.pid` tree (`descendants`), wait
  ≤ 10 s, SIGKILL survivors, then run `ensure-runners.sh`.
- `focusPane(session string, pane int) error`: `zellij --session <s> action focus-pane-id <n>`.
- `killGuard(pid int, st procStat, uid int, cmd string) error`: refuses pid ≤ 1, a foreign
  uid, self, and any process whose `comm` is `cloudflared` or `zellij`, or whose cmdline
  contains `/workspace/.cube/`.
- `descendants(root int, ppid map[int]int) []int`.

Tests:
- guard table;
- runner-name regex rejects `../r1`, `r1;rm`, `x1`;
- `descendants` on a fixture tree;
- `focusPane` on an unknown pane → error;
- run args are passed as argv, checked with a recorded fake `run`.

- [ ] Write the tests, confirm they fail, implement, then `go test ./...` passes.

### Task 7: Server — `main.go`, `server.go`, `server_test.go`

**Produces:** `control-panel [flags]` and `control-panel set-pin`.

Flags:
- `-addr` (default `127.0.0.1:7777`);
- `-cf-team`, `-cf-aud`;
- `-insecure-no-access`: local testing only; logged loudly; the Security panel shows `bad`;
- `-pin-file`, `-audit-file`, `-ci-dir`, `-status-dir`.

Routes (Go 1.22+ mux patterns), wrapped by `http.CrossOriginProtection` and security
headers:
- `GET /`, `GET /{file}`: embedded, gzip, `?v=<hash>` busting;
- `POST /api/login`;
- `POST /api/logout`;
- `GET /api/me`;
- `GET /api/stream?view=res|ci|agents|sec`;
- `POST /api/action/{name}`.

Runtime:
- The hub wakes the sampler on the first client and parks it when none remain.
- The sampler goroutine builds the snapshot each tick; each client gets the
  `{badges, view, data}` payload for its view. Unchanged payloads are skipped, and a
  `: ping` is sent every 15 s.
- `set-pin` reads the PIN twice with `stty -echo`, validates it, and writes 0600 atomically
  (tmp + rename).

Tests (`httptest`):
- no JWT → 403;
- valid JWT, no session → `/api/me` 401;
- login with the right PIN → cookie set, and flags checked;
- stream sends a first event;
- an action without the PIN → 403;
- cross-origin POST → 403;
- CSP header present.

- [ ] Write the tests, confirm they fail, implement, then `go test ./...` passes.

### Task 8: UI — `web/index.html`, `web/app.js`, `web/app.css`, `web/manifest.json`, `web/icon.svg`

- PIN screen: `inputmode=numeric`, `autocomplete=current-password`.
- App shell:
  - a bottom nav on phones and a sidebar at ≥ 900 px, with badges;
  - a connection dot;
  - a logout control.
- **Resources:** stat tiles (CPU, RAM, swap, disks, net) with canvas sparklines from
  history; per-core bars; a sortable, expandable process-group table with Kill buttons;
  largest directories.
- **CI:** runner grid, totals, per-name table, recent jobs, Restart buttons.
- **Agents:** sessions → tabs → panes. Waiting panes first and highlighted, with CPU/RAM per
  pane and Focus buttons.
- **Security:** checklist, ports, connections, keys, loose files, audit.
- Action modal (confirm text + PIN), with the result shown as a toast.
- On stream error, call `/api/me`: a 401 shows the PIN screen; otherwise show the
  "reconnecting" badge.
- Verify in a real browser at phone (390×844) and desktop (1440×900) widths.

### Task 9: Run it — `ensure-panel.sh`, live smoke test

- `ensure-panel.sh`: pidfile-guarded like `ensure-runners.sh`. It builds if the binary is
  missing, then starts the panel and `cloudflared tunnel run --url http://127.0.0.1:7777
  control-panel`. Values come from `panel.env` (`CF_TEAM`, `CF_AUD`).
- Smoke test on the VM with a throwaway PIN file and `-insecure-no-access` on another port:
  login, stream every view, run the three actions against safe targets (a `sleep` process,
  focus pane, restart of one idle runner), check the audit log, and check RSS and CPU.

### Task 10: Cloudflare — with the owner

1. `cloudflared tunnel login` (the owner opens the URL), then `tunnel create control-panel`,
   then `tunnel route dns control-panel panel.<domain>`.
2. The owner creates the Access app (Allow: owner's email) and hands over the team name and
   AUD → `panel.env`.
3. The owner runs `control-panel set-pin`. Add `ensure-panel.sh` to `~/.zshrc`.
4. End to end:
   - `curl` without Access → Access login redirect;
   - direct `127.0.0.1:7777` without a JWT → 403;
   - the owner logs in from phone and desktop.
