# Control panel v2: Terminals, CI/CD, balancer

Date: 2026-09-30. Owner request: 10 items (the quick fixes, items 1–2, shipped the same day). The owner set a goal: "iterate until full functionality", so no review gates. The owner's decisions:

- A **Chat + live screen** view per pane.
- **No PINs after login**. This covers the existing kill and restart too; every control action is still audited.
- **Agent status accuracy is the top priority.**

## 1. Terminals tab (items 5–10)

The Agents tab is renamed **Terminals** (view key `term`). It mirrors zellij 1:1: every live session → tab (by position) → pane (by id). The order comes from the zellij structure, never from status, so rows don't jump. A tab with one pane renders as a single row; a tab with more panes gets a header row with its panes under it.

### Sources (all local, no zellij plugin changes)

| Fact | Source | Cost |
|---|---|---|
| sessions | socket dir + server start time (existing `zellijSessions`) | free |
| tabs and panes: id, title, command, exited/held, floating, size | `zellij -s S action list-panes --json --all` | 1 call / 3 s / session, only while a `term` client is connected (the owner's cost rule: every CLI call makes zellij re-broadcast to its plugins) |
| pane ↔ processes | the sampler's pane group key `pane:S:N`; root = the process whose parent is the zellij server | free (sampler runs anyway) |
| foreground command | root's `tpgid` (stat field 8) → the process-group leader's cmdline | free |
| last output | mtime of the root's tty (`/proc/<root>/fd/0` → `/dev/pts/N`), which the kernel bumps on writes (8 s granularity) | free |
| **Claude status** | `~/.claude/sessions/<pid>.json`: `status` busy/idle/waiting, `waitingFor`, `statusUpdatedAt`, `sessionId`. Valid only if the pid is alive and `procStart` equals stat starttime. The pid → pane mapping comes from the sampler. | ~11 small files / tick |
| fallback status (a Claude with no sessions file, codex) | the existing `/tmp/claude-tab-status/<session>.json` | as today; **never drain the hook spool**, the zellij plugin owns it |
| transcript | `~/.claude/projects/*/<sessionId>.jsonl`, path cached per sessionId | tail reads, cached by size |
| subagents | `<transcript dir>/<sessionId>/subagents/agent-*.jsonl` + `.meta.json` (description, agentType, model, requestShape) | stat all; read only files modified in the last 60 min, tail-cached by size+mtime |
| permission prompt text | one `dump-screen --pane-id` per waiting transition (cached per pid+statusUpdatedAt) | rare |

Verified on 2026-09-30:

- Hook status said "Thinking" for PMF MNTND UK; the screen said idle, and so did the sessions file.
- Hook status said "working" for Scheduler Fleet; the screen showed a permission prompt, and the sessions file said `waiting` / "permission prompt". It also had 17 background subagents writing transcripts seconds earlier.

### Pane states (one pill each)

| state | rule | pill |
|---|---|---|
| asking | agent status `waiting` | "Needs answer" (amber, highlighted row) |
| waiting | agent status `idle` | "Waiting for you" (amber outline): item 5, idle agents are waiting on the owner |
| working | agent status `busy` | "Working" + what it's doing (last tool_use from the transcript tail) |
| stalled | busy, and for ≥ 15 min: no transcript write, no tty output, and tree CPU < 2% | "Stalled?" (red) |
| idle | shell at its prompt (fg group = root) | "Idle" |
| running | fg is a command | "Running" + command |
| stopped | fg leader state `T` | "Stopped" |
| stuck | fg leader state `D` for ≥ 30 s | "Stuck (I/O)" |
| exited | pane exited/held | "Exited <code>" |

Counts: **Waiting for you** = asking + waiting (subline: "k need an answer · m finished"). **Working** = working + stalled. The nav badge shows the waiting count (amber) when > 0, else working (blue).

### Rows

Line 1: pill, title (tab name, or pane title in multi-pane tabs), kind chip (claude/shell/cmd), CPU · RAM, and a subagent chip "17 subagents ▸". Line 2: detail, one line, ellipsis: what the agent is doing / waitingFor / "finished 5m ago", or the fg command + "output 3s ago". Rows have a fixed height; the only thing that expands is the subagent dropdown, and only when the owner taps the chip. The dropdown lists the running subagents: description, type · model, bg/fg, "doing: Bash: pytest…", running for, last active. Tapping the row opens the pane drawer. The Focus button is removed (and the `focus-pane` action with it).

A running subagent: its transcript's last user/assistant entry is not a final assistant message (stop_reason end_turn/stop_sequence without tool_use), the parent Claude is alive, and the file was modified within 60 min. `ponytail:` a tool call longer than 60 min drops off the list.

### Pane drawer (item 8)

Full screen on a phone, a right-hand panel on desktop. Opening it pushes history state, so Back closes it. It has two tabs:

- **Chat** (Claude panes; the default for them). Built server-side from the transcript into compact items:
  - `user` (typed messages; meta/command noise becomes notes);
  - `text` (assistant text, simple markdown: fences, inline code, bold, headings, lists; escaped first);
  - `tool` (name + a one-line input summary, paired with its `result`: first ~1.5 KB, error flag, collapsed);
  - `note` (turn duration, task notifications, interrupts).

  Thinking blocks are hidden. The initial load is the last ~200 items; "Load earlier" pages back by byte offset. Live updates come from polling the file size every 1 s. On a `sessionId` change (/clear, /resume), the chat resets.
- **Screen** (every pane). A live 1:1 viewport from `zellij subscribe --pane-id terminal_N --format json --ansi --scrollback 300`, relayed latest-wins at ≤ 5 fps. It's rendered as HTML by a small SGR parser (16/256/truecolor, bold, dim, italic, underline, inverse) and has a Fit toggle that scales the font to the pane's columns. The subscribe process exists only while a drawer is open.

Footer, in both tabs:

- the permission banner when asking: the question plus one button per numbered option (sends that digit);
- quick keys: 1 2 3, Esc, Enter, ↑ ↓, Tab, ⇧Tab, Ctrl-C;
- a message box (Enter sends, Shift+Enter adds a newline) that sends `paste` + `send-keys Enter`.

### Create (item 9)

- A "New session" button in the Terminals header, and "+ Tab" / "+ Pane" on each session and tab.
- A small form: name, directory (default `~`), start: Shell or Claude (types `claude-multi` into the new shell, the owner's own launcher, so it behaves as if typed over SSH), and a split direction for panes.
- Commands:
  - `zellij attach -b NAME` (cwd = directory);
  - `zellij -s S action new-tab --name N --cwd D` (switches attached clients to the new tab, like zellij does);
  - `zellij -s S action new-pane --tab-id T --no-focus --cwd D --name N [-d right|down]`.
- zellij is always run with an env stripped of `ZELLIJ*`, `CLAUDE*`, `CLAUDECODE` and `AI_AGENT`. The live panel was started from inside an agent pane, and a nested `claude` would think it's a child session.

### API

- `GET /api/stream?view=term` → `Terminals`.
- `GET /api/pane/stream?session=S&pane=N&mode=chat|screen` (SSE, gzip).
- `GET /api/pane/chat?session=S&pane=N&before=OFF` → `{items, more}`.
- `POST /api/pane/input {session, pane, text?, enter?, keys?[]}`: text ≤ 16 KB; keys from a whitelist.
- `POST /api/term/new {what: session|tab|pane, session?, tab?, name?, cwd?, run: shell|claude, dir?}`.
- Validation:
  - session names match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` and must be live (or absent, for a new session);
  - names ≤ 64 printable characters;
  - cwd is an existing absolute dir;
  - pane and tab ids are ints ≥ 0.
- Everything goes through exec with arg vectors, never a shell.
- Audit:
  - `input` (`S:N` + the first 80 chars or the key list);
  - `create`.

## 2. CI/CD tab (item 4)

- **Live runners table** (full width), replacing the runner cards. Columns: Runner · State · PR (#N + title, linked) · Branch → base · Workflow / job · Event · Running for · Restart.
  - The data comes from the runner's newest `_diag/Worker_*.log`. That's the github context `"k": …, "v": …` pairs in the first ~400 KB: repository, ref (`refs/pull/N/merge`), head_ref, base_ref, event_name, workflow, run_id, actor, sha.
  - Parsed metadata is cached per file.
  - The PR title comes from `gh api repos/R/pulls/N --jq .title`, run in the background and cached 30 min. It's optional.
- **Job types**, grouped by workflow (CI, Nightly, …): workflow success rate and run count, and under each, its job names with success %, runs and avg duration. Each job is matched to the newest Worker log started at or before the job start + 60 s.
- **Recent jobs**: completed only (running jobs are in the live table), newest 200, each with a result pill, PR/branch, runner, duration and age.
- Both lists scroll inside their card (max-height 520 px).

## 3. Balancer (item 3)

This VM has no cgroups for uid 1000 (v1, root-owned, no caps), so there are no hard CPU or RAM caps. The levers that work without root (verified):

- **CPU cores**: `sched_setaffinity`. Reversible, and inherited by children.
- **Priority**: nice 10 / 19. One-way: `RLIMIT_NICE` is 0, so it can't be raised back until the process restarts; the UI says so.
- **Disk priority**: ioprio idle / best-effort. Reversible.
- **Pause / resume**: SIGSTOP / SIGCONT.

A rule is keyed by process-group key (`runner:r1`, `pane:dev-1:10`, `qemu-system-x86`, …): `{key, label, cores[], nice, ioIdle, paused}`. Rules are saved to `~/.config/control-panel/limits.json` (0600). An enforcer goroutine, with its own sampler, applies them every 10 s to every pid in the group, including new ones. It runs only while rules exist.

UI: the expanded process-group row gets a "Limit" panel with core toggles, Priority (Normal/Low/Lowest), Disk (Normal/Idle) and Pause/Resume. Active rules show as a "Limits" card at the top of Resources, with Remove. Remove resets affinity to all cores, ioprio to best-effort 4 and SIGCONTs; nice stays.

Guards:

- never touch the panel itself or `cloudflared`;
- pause is refused for the `killGuard`-protected set (zellij, cube, the panel);
- only own-uid processes are touched.

Audit: `limit`.

Skipped: RAM caps (need cgroups; `RLIMIT_AS` would just crash processes) and a CPU % cap by SIGSTOP duty-cycling (breaks timeouts). Add them if the VM ever grants cgroup delegation.

## Out of scope

- Closing or killing panes, tabs and sessions: not asked for. Kill a process from Resources.
- Multi-user.
- Codex transcript chat: codex panes get the Screen view only.
