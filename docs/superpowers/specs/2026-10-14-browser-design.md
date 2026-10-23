# Browser: a Chromium on the VM, in the panel

Built 2026-10-14, overnight, from the design discussed in chat that day (no board; the owner asked for a working first version). The same evening: real DevTools, sessions linked by cube-browser's own id, recordings (trails), web pages only on the localhost list, and the phone made usable.

## What it is

A **Browser** section in the sidebar (`g` then `b`), edge to edge like Files. It shows a real Chromium running on the VM, so `localhost` is the VM's. The page streams to you and your input goes back. It has two parts:

- **Your windows.** The panel's own Chromium, on a persistent profile, so logins and cookies stay. Tabs, an address bar, back, forward and reload, the page's own dialogs, popups, uploads and downloads. Agents can't reach it: the panel drives it over a pipe.
- **Agents.** Every `cube-browser` session an agent opens, watched live. Each has a card, its page and its command timeline. **Take over** lets you drive it while the agent's next command waits. Every session is recorded: **Recordings** replays it, or two side by side.

Around the page:

- **Start page** (a new tab): what serves web pages on localhost (port, `<title>`, process, project, zellij pane), the projects you can **Run**, and recent addresses. A server counts as a page when its `/` answers a 2xx HTML document (after up to 3 redirects within localhost: a login page counts); an API's JSON, a bare string (Flask's "Hello"), a 404 page or a database never shows, anywhere (the list, the Localhost menu, the address bar's suggestions, the badge, Run's auto-open).
- **Address bar:** a search goes to DuckDuckGo (Google, Brave and Startpage answer the VM's datacenter address with a robot check). The whole address is selected when the bar gets focus, at once (not on a timer: a busy page ran the first keystrokes first, and a late select-all ate them) and again on the click that focused it (a tap puts its caret after focus), never once you've typed.
- **Localhost menu**: open, stop, or run a project.
- **Viewports**: Fit (the default: the window's own size and density) or a device (iPhone SE/15, Pixel 8, iPad mini/Pro, Laptop, Desktop, 1080p). A device gets its size, density, touch, mobile layout and user agent, and a frame with a resize grip. Colour scheme (auto, light, dark) and network (Fast 4G, Slow 4G, 3G, offline).
- **Problems drawer** (`p`):
  - Problems: console errors and warnings, uncaught exceptions with stacks, HTTP ≥ 400, failed requests.
  - Console: every message, plus a prompt that runs JavaScript in the page.
  - Network: status, method, name, type, size, time.
  - Both have a filter, Clear, and **Send to agent**.
- **Find** (⌘F): matches highlighted in the page, with next and previous.
- **DevTools** (F12, ⌥⌘I; ⇧⌘C picks; right-click → Inspect): Chromium's own, docked on the right or at the bottom, resizable. All of it works: Elements and Styles (editing), Console, Network, Sources, Application, Performance. Its header shows the element selected in Elements and **Send to agent** sends it: selector, text, HTML, the CSS rules on it (author rules, most specific first, and the inline style) and a picture of it. On an agent's tab only while you've taken over (DevTools can change the page).
- **Right-click** on the page: Back, Forward, Reload, Inspect, Send this element to an agent, Annotate, and the page's own right-click (for apps with a menu of their own).
- **Annotate** (`a`): opens at once on a copy of what's on screen (a capture from the VM was a 2× PNG: over a second, then the tunnel), and the stream pauses while you draw (its frames go unacknowledged, so the server stops). A page that was moving, so 1× on screen, gets a sharp capture swapped in underneath your marks. The toolbar drags by its grip or between its buttons; a double-click puts it back in the middle. Tools:
  - pin a comment to an element (the page describes it);
  - pen, arrow, box (a box can carry a comment), text;
  - five colours, undo, clear, copy.

  Every mark gets a number, on the image and in the message. The page is asked what's under each as you draw it: a pin, an arrow's head or a text names the element at that point; a box or a freehand mark (a circle round something) names the element whose box overlaps it best (intersection over union, over the elements under a 5×5 grid of points in it and their parents) and the text that shows inside it.

  **Send to agent** picks a pane, listed by its zellij tab name (then session, the agent's title, what it's doing); the watched agent's first. The list is fetched when Annotate opens, so the sheet opens at once. The agent gets two images and a message (URL, viewport, each numbered mark with its element's selector, text, HTML and styles, and the page's recent problems):
  - the annotated image: JPEG, at most 1,600 px wide (what a model reads; ~35 KB for a plain page), uploading from the moment the sheet opens;
  - the clean one: captured by the panel when Annotate opens and saved straight into the chat's uploads (JPEG q85 at the page's density), so it never crosses the tunnel twice.

  Copy stays PNG, the clipboard's only image type. Sending goes through the chat's outbox like a typed message (`queueSend`): its files are the composer's attachment objects (`{file, path, up}`), or plain Files, which `filePath` uploads. (Until 2026-10-15 the sheet passed plain Files, which `filePath` didn't take: every annotate send failed after five "slow network" retries.)

## How it works

| Piece | File |
|---|---|
| Websocket (RFC 6455: server upgrade, client dial) | `ws.go` |
| CDP client (pipe and websocket transports) | `cdp.go` |
| The owner's Chromium, tabs, events, emulation, streaming, the page's protocol | `browser.go` |
| Whole frames vs patches, sharp at rest | `browserpatch.go` |
| Agents' browsers: registry, attach, timeline, hold, the wrapper | `browseragents.go` |
| Ports, projects, Run, Stop, the static server, favicons | `browserports.go` |
| The UI | `web/browser.js`, `web/browser.css` |

**Chromium:**
- **Binary:** Playwright's newest (`/opt/cube/browser-binaries`, else `~/.cache/ms-playwright`). It runs headless, sandboxed, with `--no-proxy-server` (the box's proxy is for the agents' API calls) and `--force-device-scale-factor=2`.
- **Profile:** `~/.local/state/control-panel/browser/profile`.
- **Tabs:** saved in `tabs.json` and reopened after a restart. Chromium's own session restore is cleared first, or after the panel restarts it would reopen them a second time.

**Headless quirks this works around:**
- The screencast captures the *window*, so every tab has its own window, sized to its viewport plus the browser's frame (measured).
- Titles aren't reported as they change: they're read from the navigation history on load and every 2 s for tabs on screen.
- A clipped screenshot makes the screencast send a clip-sized frame: frames whose aspect isn't the viewport's are dropped.

**Streaming:**
- **Frames:** one websocket per open page (`/api/browser/ws`). JSON goes both ways; frames come down as binary (kind, metadata, image).
- **Flow control:** a viewer has at most 2 frames in flight. Chromium's screencast is acknowledged only when some viewer has room, so a slow phone gets fewer frames, never a backlog. Only tabs on screen stream.
- **Motion at 1×, sharp at rest:**
  - While a page moves, frames come at CSS size, JPEG q65 (~140 KB, ~30 fps scrolling Wikipedia).
  - 180 ms after the last frame, one sharp capture follows: WebP at 2× (~280 KB) for a retina screen, JPEG q88 at 1× otherwise.
  - A 2× stream would be ~500 KB a frame at 14 fps.
- **Patches:**
  - **Trigger:** a frame that isn't part of motion (fewer than 3 quick frames in a row), on the owner's tabs.
  - **Diff:** compared with the last whole frame in 16 px blocks.
  - **Send:** if what changed is under 20% of the page, only that rectangle is sent: cut from a whole sharp capture, not a clipped one (a clipped capture briefly shrinks the page's view, and a click arriving then misses).
  - **Overflow:** a viewer with 8 unsent patches gets a whole sharp frame instead.
  - **Result:** a blinking caret costs ~1 KB/s instead of ~900 KB/s, and nothing flickers.
- **Density:** the owner's tabs always emulate 2×, the density Chromium renders at. An emulated density that differs from it makes Chromium scale input by the wrong factor now and then (clicks landing at twice their coordinates). A device preset's own density (3×, 1×) is shown, not emulated. A 1× screen still gets 1× frames.
- **Input:**
  - mouse, wheel (coalesced per animation frame), touch (real touches in a mobile viewport; a tap is a click and a drag scrolls otherwise);
  - keys: printable keys with their text, the rest raw, ⌘ mapped to Ctrl on a Mac;
  - IME, phone keyboard and paste as inserted text, copy as the page's selection;
  - the Browser's own keys (⌘L, ⌘R, Alt ←/→, Alt T/W, ⌘⇧A).

**The page's own UI, redrawn in the panel's style:**
- alert, confirm, prompt, beforeunload;
- file choosers (a bar asks you to pick; the files are uploaded and handed to the page under their own names);
- popups (a new tab beside the opener);
- downloads (to `~/Downloads`, with a toast).

**Agents:**
- **The wrapper:** `~/.local/bin/cube-browser`, installed by the panel at start when the Cube image has cube-browser. It never replaces a file that isn't its own. It runs the image's tool unchanged, with `CUBE_BROWSER_BINARIES` pointing at a mirror (`~/.local/share/control-panel/browser-bin`). In the mirror, `chrome` adds `--remote-debugging-port=0` and writes `<pid>.json` into `~/.local/state/control-panel/agent-browsers/`.
- **Which session, which agent, which tab** (one id everywhere: cube-browser's own):
  - **Session:** cube-browser's launch id. The wrapper works it out with cube-browser's own code (`resolveContext` imported from `/opt/cube/browser/context.mjs`), so it can't drift from the tool: `CUBE_BROWSER_LAUNCH` for an agent Cube started, `manual-<hash of checkout and name>` for `--session`. The timeline (`log/<id>.jsonl`), the hold (`hold/<id>`) and the trail are named by it. (Before, the zellij pane named them: an agent outside zellij got a new name every command, and two sessions in one pane shared one.)
  - **Chromium:** its runtime folder names it (Playwright runs it with `XDG_CACHE_HOME=<runtime>/cache`), so the registry entry carries the launch id and the runtime folder; the runtime's `owner.json` gives the checkout, the conversation and the agent's process, whose zellij pane names the agent when nothing else does.
  - **Agent:** whoever ran the session's last command (each log line has its pane), else the owner process's pane. Its card shows the pane's tab name and title, and what it's doing.
  - **Tab:** the one it works in, tracked: the tab it opened or navigated last (and the newest left when one closes). The card, the picture after each command and the recording use it.
- **Take over:** while `hold/<id>` exists, its next command waits (up to 100 s, then it tells the agent to retry). A hold ends on Hand back, after 15 minutes unused, or when no page has the Browser open.
- **Attaching:** the panel scans every second from its start (not only while a page has the Browser open: recordings need it; only `main` starts the scan, so a test that serves the Browser never touches the agents) and connects to each agent's Chromium as a second debugging client. It streams the agent's tab and records its console, never touches its viewport, and sends input only while you hold it.
- **Undo:** delete `~/.local/bin/cube-browser`, or start the panel with `CP_NO_AGENT_WATCH=1` so it doesn't reinstall the wrapper.

**Recordings (trails, `browsertrails.go`):**
- **What:** every agent session, whether anyone watches or not. Per Chromium run, `agent-browsers/trails/<launch>/<start>-<pid>/`: `events.jsonl` (commands and their results, navigations, page loads, console errors and failed requests), `shots/<n>.jpg` (the page after each command and each load, JPEG q60), `motion.bin` (the page as it moved, ≤ 2 fps: an agent's tab nobody watches gets a low "record" screencast, JPEG q50 at 1280 wide; a static page makes no frames, so costs nothing) and `meta.json`. Named by its Chromium's start, so a panel restart carries on with the same trail. `visits.jsonl` lists every page load of every trail.
- **One recorder:** only the panel holding `trails/.lock` (flock) writes trails; it says so in `agent-browsers/recorder.json` (its pid). Another panel on the box (a test one) watches but doesn't record, and takes over when the first one ends: the sessions it watched unrecorded connect again, with their trails. (Before 2026-10-16 every test panel and test run recorded too: each wrote every live session's commands into its trail again, 16 times over in one.)
- **A run's commands only, once:** a session's log keeps all its runs (its name is used again), and a restarted panel reads it from the top. `trailStep` writes a command only if it ended after this Chromium started (minus 2 s), and once: the ids written are read back when a trail is reopened. (Before, a run's recording could begin with commands from the day before: an 18-hour timeline.)
- **API calls (`browsercalls.go`):** an agent page's XHR and fetch requests, each kept as `calls/<n>.json` in its trail when it ends: method, URL, the page that made it, request headers and body, response status, headers and body (≤ 2 MB each, kept as text, or base64 when it isn't; 100 MB and 3,000 calls a run). Credentials never go to disk: the auth, cookie and API-key headers, and JSON or form fields named like password, secret, token, api key, pin, otp, become `[redacted]`. An event (`call`) goes in the trail, a line in `trails/calls.jsonl`. Agent tabs get Network buffers of 64 MB (16 MB a resource) so bodies are still there when asked for.
- **The first page's calls too:** while a recording panel runs, the wrapper turns `open <url>` into `open` (blank), a wait (≤ 3 s) until the panel has the tab (`agent-browsers/attached/<chromium pid>`, written once its Network is on), then `goto <url>`, printed exactly as `open` prints (its session and pid around goto's result). About 1 s more on a session's first open; without a recording panel, or if anything differs, the plain command.
- **Kept:** 7 days (by last write), and 1 GB in all at most, the oldest first; a pinned one stays. The sweep runs hourly.
- **The list:** "Live now" (big cards, a pulsing LIVE badge) above "Recordings" (rows with a small picture and a REPLAY badge, their command, page and API-call counts); on a phone, one strip, told apart by the badges. Opened on Agents, the recordings load at once (not at the next 15 s refresh).
- **Replay:** Agents → Recordings: a card per session; it opens in a player (the pictures over time, play at 1–16× with idle stretches skipped, a scrubber marked with commands, loads and errors, the events alongside, each a jump). **Compare** puts a second recording beside it: picking an event in the first sends the second to its own visit of the same page. Pin, delete. A live session's bar has **Replay** (its trail so far). The side has **Steps** (commands, loads, errors) and **API calls**: each opens in place to what was sent and what came back (JSON pretty-printed), its headers, Copy the response.
- **For agents:** `cube-browser history [text]` (the wrapper answers it): alone, the sessions of the last 7 days with their pages and last screenshot; with text, each visit to matching pages: when, by whom, the command that led there, its console and network errors (a failed page request counts for the page it asked for), and the screenshot's path (an agent can open it). After a successful `open` or `goto`, one line says when other sessions already loaded that page, and how many API calls its pages made. `cube-browser api-calls [text]` (cube-browser has a `requests` of its own): the newest of each method and URL matching the text (in the URL, or in the page that called), how many times, by whom, and the path of its file, to read instead of calling that API again. `--help` lists both.

**DevTools (`browserdevtools.go`):**
- **Its files:** the Chromium the panel runs serves its own DevTools (same version) on a debugging port, so a helper Chromium with an empty profile serves them; the panel copies each file it's asked for to `browser/devtools/<version>/` and serves it from there (immutable, the version in the path), the helper stopping a minute after the last fetch (and with the panel: `Pdeathsig`). `inspector.html`'s own CSP (websockets to 127.0.0.1 only) is dropped; the panel's header for `/api/browser/devtools/f/` allows its scripts, styles, workers and websocket, in a frame of the panel only.
- **Its connection:** `/api/browser/devtools/ws/<tab>` (session, same origin, a tab you may drive). The panel attaches a session of its own on the tab for it (`Target.attachToTarget`) and relays both ways in order, its ids renumbered; the frames and workers it attaches are relayed too; its own screencast is answered without one (the panel shows the page, and a second screencast would starve the stream). Chromium announces that session as an attached target: it isn't a tab (before that check, closing DevTools removed the real tab from the panel).
- **Reading along:** `DOM.setInspectedNode` (the element it selected: its label goes to the pages showing the tab); right-click → Inspect sends DevTools what Chromium's picker would (`Overlay.inspectNodeRequested`); ⇧⌘C turns the picker on in its session and off once it picks.

**Run:**
- **Detection:** a `package.json` dev, start, serve or preview script with its lockfile's package manager, in the folder or a usual app subfolder, plus `install` when `node_modules` is missing. Also Django's `manage.py`, or a plain `index.html`.
- **Where it runs:** commands go to a tab of the **servers** zellij session (created when missing), where Terminals shows their output. Static folders are served by the panel itself.
- **Opening:** the page opens in a new tab once a new port listens from that pane or folder.
- **Stop:** sends Ctrl-C to the pane, or closes the static server.

## API (all behind the session)

| Route | |
|---|---|
| `GET /api/browser/ws` | The page's websocket. Same origin only. |
| `GET /api/browser/ports` | The localhost servers that serve a web page, with process, folder, project, pane, `<title>` and status. |
| `GET /api/browser/projects` | Runnable projects in `~/projects`, `~/repos`, `~/side-quests`. |
| `POST /api/browser/run` | `{dir, sub, cmd}`: under `~` or `/tmp`, one line. Audited. |
| `POST /api/browser/stop` | `{port}`. Audited. |
| `GET /api/browser/icon?url=` | A site's favicon, fetched here (the page's CSP keeps images to the panel). |
| `GET /api/browser/trails` | The recordings, newest first, with their size. |
| `GET /api/browser/trail?id=` | A recording's meta, events and motion index. |
| `GET /api/browser/trail/shot?id=&n=` (or `&off=&len=`) | A recording's picture or motion frame. |
| `GET /api/browser/trail/call?id=&n=` | One of a recording's API calls (its file). |
| `POST /api/browser/trail/edit` | `{id, do: pin|unpin|delete}`. Audited. |
| `GET /api/browser/devtools/f/<version>/<path>` | DevTools' files. |
| `GET /api/browser/devtools/ws/<tab>` | DevTools' connection to a tab. |

## Hardening (from a review of the first version)

- **Session:** the websocket re-checks the session every 25 s and closes when it's gone (Lock, expiry). Locking the panel also closes it from the page.
- **Ordering:**
  - Whatever waits on Chromium (navigating, emulating, dialogs, files) runs on one ordered worker per viewer, off the read loop, so acks and input never queue behind a slow page.
  - Screencast changes and emulation are applied under a per-tab lock, re-reading what's wanted, so of racing changes the last one wins.
  - Held screencast acks are a list, so none are lost.
- **Bounds:**
  - Page strings: titles 300, URLs 2000, dialogs 2000, sources 600, stacks 4000.
  - Logs trim in batches, and the drawer updates at most four times a second.
  - A viewer far behind keeps its newest 500 messages.
  - Logs kept a week; recordings a week and 1 GB in all (motion: 200 MB a run; 300 console and network events a run).
- **Stop:** sends Ctrl-C only when the server is its terminal's foreground job (else SIGINT to the process), so an agent's chat never gets Ctrl-C.
- **Static server:** answers only as localhost and never serves dotfiles.
- **Crash:** Chromium is relaunched when it dies while someone watches.

## Tests

| Test | Covers |
|---|---|
| `TestBrowserLive` | A real Chromium, watched by a 1× viewer: tabs, navigation, title, frames, a click causing a console error and a 404, the element probe, typing with IME text (it failed half the time before the density fix), eval, a sharp still, new and close. |
| `TestBrowserDevTools` | DevTools' files (the version, the CSP gone, no path out), its session (a command answered under its id, the document), the element it selects reaching the page and going to an agent with its CSS rules, right-click → Inspect revealing an element, and the tab staying one tab while DevTools is open and after it goes. |
| `TestProbeHTTPPage` | A Vite page and a login redirect are pages; a JSON API, Flask's bare string and Express's 404 aren't. |
| `TestBrMobileUA` | A phone's user agent. |
| `TestTrailsSweep` | A week kept, a pinned one past it, the deleted ones' visits gone. |
| `TestTrailStepRun` | A trail holds its own run's commands, once: not an earlier run's from the session's log, not twice after a panel restart. |
| `TestOneRecorder` | A second panel on the same home doesn't record until the first one ends. |
| `TestBrRedactBody` | Secret fields of JSON (camelCase too) and form bodies redacted, the rest untouched; auth headers redacted. |
| `TestBrowserPatches` | A blinking caret costs patches, not frames. |
| `TestAgentWatch` | The wrapper with the Cube image's real cube-browser in a scratch home: the session's id (cube-browser's), register, attach, stream, timeline, a click reaching the page, a hold blocking the next command, its recording (commands, load, console error, pictures, motion), the page's fetch kept with what it sent and got and its password, bearer token and access token redacted, `api-calls` finding it, a second session's `open` (blank, then goto) keeping its first load's API call and printing as open does, `history` showing the visit with its error and picture, and another session's `open` of the same page told it was seen. |
| `TestWS*`, `TestBrURL` | The websocket framing and the address bar's rules. |

All pass under `-race` (`CC=gcc CGO_ENABLED=1`).

## Known limits

- An agent's Playwright connects to its own Chromium at the browser level. The owner's browser is on a pipe nobody else can reach.
- No page zoom yet.
- Copy reads the page's selection, not what the page itself puts on the clipboard.
- The mouse cursor's shape is asked of the page (`getComputedStyle` under the pointer), since headless Chromium doesn't report it: a cursor a page draws itself isn't shown.
- Device presets show their density but render at 2× (see Density).
- Basic-auth prompts aren't handled.
- One shared browser: two screens on the same tab in Fit (a laptop and a phone) each fit it to themselves, the last to resize winning.
- DevTools' source maps load through Chromium when they can; a source map only the owner's machine could reach doesn't.

## On a phone

What made it unusable (2026-10-14 evening), and what changed:
- **A dropped socket never came back:** the reconnect called a function that didn't exist and threw before scheduling the retry; a phone drops the socket whenever it locks or switches apps. It reconnects now, at once when the page is visible again or the network returns, with a "Reconnecting" note meanwhile.
- **The address bar:** a tap left its caret where the finger was (typing went into the old address), and `inputmode="url"` gave iOS a keyboard without a space bar. The whole address is selected on the tap, and the keyboard is a search keyboard with Go.
- **Searches:** Google answered with a robot check (see the address bar).
- **The keyboard over a page's field:** a tap opens the phone's keyboard only inside the tap, too soon to ask the VM what's there. The page keeps a map of the page's text fields (`fields`: their boxes, asked after the page changes, at most every 300 ms) and looks the tap up in it.
- **Desktop sites:** a phone in Fit gets Chrome's Android user agent (`Linux; Android 10; K … Mobile Safari`), so sites send their mobile version.
- **Recordings and DevTools:** the player hides the session cards while it plays (Close brings them back); DevTools docks at the bottom.
