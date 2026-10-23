# Files: a file manager in the panel

Approved on the design board `docs/superpowers/mockups/2026-10-11-files-explorer.html` (rev 3, 2026-10-12).

## What it is

A **Files** section in the sidebar (`g` then `f`) for everything under `~` and `/tmp`. Browse, preview any common document, edit text, upload, rename, move, duplicate, delete, clone a git repository, hand a file to J.A.R.V.I.S or an agent pane, and search every file name on the box as you type.

**Owner decisions**
- **Scope:** `~` and `/tmp`.
- **Secrets:** listed and opened like any file.
- **Renderers:** run in the browser, lazy-loaded.
- **Delete:** there is no bin.
- **Delete, move and rename refuse** what the panel itself needs: SSH, its own folder and state, Claude's credentials, CI runners. This is the disk explorer's existing guard, `deleteGuard().protected`.

## Layout

- **Edge to edge:** the sidebar folds to icons while Files is open, and the page header is hidden.
- **Toolbar:**
  - shortcut chips: Home, Repos, Projects, Uploads, /tmp;
  - back, forward and up, then a clickable path;
  - search, Upload, New ▾ (file, folder, upload a folder, clone), View ⋯ (hidden files, collapse, refresh);
  - the bell and lock.
- **Tree:**
  - virtual, so only the rows on screen are in the DOM;
  - click a folder to expand it, double-click to open it as a view;
  - click a file to open it in an italic preview tab that the next click replaces; double-click or editing keeps it;
  - keyboard: arrows, Enter, Space, F2, Delete;
  - drag to resize the tree; drop files or folders on it to upload; drag rows onto a folder to move them.
- **Tabs:** in one or two side-by-side groups. ⌘-click, Open to the side, or drag a tab to the right edge. `f` hides the tree (focus).
- **Phone:** one column. A tap drills into a folder, a file opens over the list with a back arrow, and touch-and-hold opens the actions.

## Viewers (`web/files.js`)

| Kind | How |
|---|---|
| Text and code | CodeMirror 6 opens editable at once, with highlighting, a minimap, find, undo and ⌘S. Up to 5 MB; past that it's read-only, and a `.log` opens on its last 5 MB. |
| Markdown | Preview / Split (live) / Source. |
| HTML | A sandboxed iframe (`allow-scripts`, never `allow-same-origin`), plus Source. A design board's comments work as in the session viewer: kept in `<page>.comments.json`, and *Send to chat* hands them to the agent you pick. |
| CSV / TSV | SheetJS table (editable) plus Source; a save writes the text back, nothing lost. |
| xlsx / xls / ods | SheetJS: every sheet, a virtual grid. The formula bar is the cell editor: type over a cell, Enter / Tab commits, `=` makes a formula. A save rewrites the workbook (values, formulas, number formats, merges, column widths; not colours, fonts or charts), so it asks once and offers *Save a copy*. |
| PDF | pdf.js: pages drawn as they scroll in, text selectable, zoom, `isEvalSupported: false`. |
| docx | docx-preview (with JSZip), in a shadow root; its links open a new tab. |
| pptx | `web/office.js`: each slide drawn from its XML (text with sizes, colours and the theme's fonts, pictures, shapes, tables, layout and master placeholders and backgrounds). No charts or SmartArt. |
| doc / ppt | `web/office.js`: the text out of the OLE container (SheetJS's CFB): a .doc's piece table, a .ppt's slide text, shown as a page or slide cards marked *text only*. |
| Images, video, audio | Native elements. |
| Folders | Grid (thumbnails), List (sortable, virtual), or Sizes (the disk-usage chart, moved here from Resources). |
| Anything else | A card with Download and Send to… |

All of it is built with `textContent` / escaped strings: a file's text is never HTML in the panel.

The libraries are vendored in `web/` and built by `scripts/vendor/build.sh`. Each loads through a `<meta name="lib-…">` tag the first time a file needs it, with an immutable cache.

## API (`fs.go`, every route behind the session)

| Route | Notes |
|---|---|
| `GET /api/fs/list?path=` | Entries, plus the git branch of repo folders, plus free space. |
| `GET /api/fs/raw?path=[&dl=1]` | ServeContent (Range) and `X-Mtime`. Everything except images, media and PDF is served under a sandbox CSP. |
| `PUT /api/fs/file?path=&mtime=` | Atomic save. 409 if the file changed on disk; `mtime=0` creates a new file. |
| `POST /api/fs/upload?dir=[&replace=1]` | Streamed multipart, relative paths for folders, never overwrites without `replace`. |
| `mkdir`, `rename`, `move`, `copy`, `delete` | Confined to the roots. Roots and protected paths are refused. Each action is audited. |
| `GET /api/fs/search` | The name index. |
| `GET /api/fs/grep` | ripgrep, in the folder you're in, 8 s cap, 200 lines. |
| `GET /api/fs/repos` | The owner's GitHub repos via gh, and where each is already cloned. |
| `POST /api/fs/clone` | git over HTTPS with gh's token. Progress streams as NDJSON. |

## Search (`fsindex.go`)

- Every name under `~` and `/tmp`, held in compact arenas in memory: on this box 2.57M entries, built in about 1.3 s on 16 cores, 87 MB of heap.
- Rebuilt every 10 minutes, and 2 s after any change made through Files.
- The longest query word is matched across the arena in parallel, then the other words are checked against the path and the results ranked:
  - exact name, then prefix, then word start, then substring;
  - junk folders (node_modules, .git, caches, CI `_work`) sink but stay findable.
- Measured end to end in the browser: 3–14 ms for typical queries, 26 ms for a single letter matching 2M names.
- *Ceiling:* a full rewalk with no inotify (2.5M files would exceed the watch limit). The upgrade path is dirty-directory rescans.

## Resources

Storage and Disk usage leave the Resources page. Free space shows in the Files tree footer, and folder sizes are the Sizes view. Memory replaces them:
- where the RAM is (apps, page cache, kernel, shared, free), with swap, PSI pressure and dirty pages;
- an hour of apps, cache and kernel;
- who is using memory, with each group's change over the hour.

## Speed (measured on this box, in the browser, 2026-10-12)

| | |
|---|---|
| Panel page load with Files in it | DOMContentLoaded ~40–110 ms; `files.js` is 31 KB (gz) and does no work until the view opens |
| Search a name (2.6M indexed) | 6–15 ms; one letter matching 2M names 26 ms; 26–63 ms a keystroke with results drawn; quick look 5 ms; a file made through Files is found at once (no rebuild) |
| Open a file (first time) | code 46 ms (the editor, pdf.js and the sheet worker are started while idle), a 5 MB log 67 ms, a 3,000-section note 115 ms (off-screen blocks skip layout), a 300-page PDF's first page ~100 ms (one shared pdf.js worker), .pptx 62 ms, .doc 76 ms, an image 30 ms, a small CSV 13 ms |
| Big sheets | parsed in `sheet-worker.js`, the page never blocked; a 6 MB CSV shows its first rows in 85 ms, a 60k-row workbook (9.7 MB) in ~650 ms with the rest a second later (editing waits for it). A workbook over 1.5 MB is read by ranges: the zip's directory from the end, then only the parts the first 3,000 rows need, the sheet cancelled once they're in (the first 200 rows on screen before the rest of those); the whole file follows at low priority, and closing the tab stops it. Meanwhile you can edit (edits wait in the worker and land in the whole book) and switch sheets (each one's first rows by ranges too). The shared strings are read only as far as those rows need (Excel lists them in order of first use): a book with 120k unique strings shows its first rows in 0.85 s on a phone, not 4.9. An .xlsx / .xlsm save sends only the edited cells (`/api/fs/xlsx`): the panel patches their XML in the file and copies every other part byte for byte, so colours, fonts and charts stay, and it's ~0.3 s on a phone even before the book has loaded (the first cell of a shared formula — the one holding what a filled-down column copies — goes with its other cells written out one by one, from SheetJS's reading of them; a cell inside an array formula is refused at once, as Excel does, since those change only as a whole; SheetJS writes the whole file only for a workbook the patcher can't read). The whole-file download stops while its tab is behind another and goes on from the byte it had (Range) when it's back, so it never takes the link from what's in front |
| Photos | Pictures over 200 KB come scaled on the panel (`/api/fs/thumb`): by ImageMagick in its own process when the box has it (a JPEG decoded straight at 1/2–1/8 by libjpeg; eight at once; never given a file name), else the standard library. A 12 MP photo's tile is 13 KB in ~0.1 s; 24 at once 0.3–0.4 s with the panel's RSS flat (it was +90 MB through Go). EXIF orientation kept. The viewer's Fit gets one sized to the screen, 100% the original. HEIC (which browsers can't show) opens this way in ~0.9 s (0.76 s of it decoding); what ImageMagick made is kept in `~/.cache/control-panel/thumbs` (30 days unused), so a second look from any device is instant. A HEIC's tile also makes its opened size in the same decode, so opening it from its folder takes ~13 ms |
| Text over the link | text, code, logs, CSV, JSON, SVG go gzipped (2–10×: a 5 MB log is 578 KB), a PDF read whole too (a third smaller); a big text file shows its first 256 KB (a log its last) at once, read-only, and the rest to 5 MB follows, at low priority; put behind another tab it stops and starts over when shown |
| Typing in a 120,000-line file | 1.7 ms a keystroke (13 ms to the next frame, minimap included); the minimap draws only the lines it shows |
| Folders | list 2–5 ms; 20,000 files 66 ms cold (stats on all cores, sorted once on the server, gzipped), 5 ms from cache; a refresh of 23 open folders 4 ms; 300 photos as tiles 49 ms (thumbnails load near the screen) |
| File operations (loopback) | upload 427 MB/s; save 16 ms; rename 5 ms; duplicate 5 ms; move 2 ms; delete 50 MB 50 ms |
| Tabs | switch ~13 ms; open beside 94 ms; selection 0.02 ms (a class swap) |
| Memory (browser) | panel 3 MB; nine big files open 62 MB; all closed 14 MB; a 300-page PDF scrolled end to end keeps 3 canvases |
| Memory and CPU (server) | RSS 156 MB with Files vs 39 MB without: the index (~85 MB) lives in mmap, outside the Go heap, so it costs its size and no GC headroom; a full walk 0.9 s wall / ~5 s CPU (raw getdents64, no stat per folder); a rewalk lists again only the folders whose ctime moved and stats the rest: 0.5 s wall / 2.2 s CPU, every 10 minutes and at most once a minute after changes (every sixth a full walk) |

On a phone over the tunnel (simulated: 80 ms round trips, 10 Mbps, CPU 4× slower, cold cache): panel 0.6 s; Files first paint 0.16–0.46 s; code 0.33 s; markdown 0.21 s; a PDF's first page 0.4–0.84 s (pages drawn one at a time, nearest first); a 6 MB log 0.18–0.5 s; a CSV 0.35 s; a 60k-row workbook's first rows 0.79–1.07 s (7.7 s when it waited for the whole 8.9 MB), another of its sheets 0.5 s; .pptx 0.53 s; a folder of 4.4 MB camera photos, its first 8 tiles 0.62 s (~28 s at full size); one of them open 0.33–0.39 s; a 12 MP HEIC 1.0 s; 20,000-file folder 0.23–0.37 s; search ~100 ms (one round trip). On a bad 3G link (300 ms, 1.6 Mbps, CPU 6× slower): panel 2.5 s cold (its assets are cached after), Files 0.6 s, code 0.67 s, a 6 MB log 0.58 s, a PDF 0.87 s, a 12 MB workbook's first rows 2.7 s and its save 0.38 s, photo tiles 1.2 s, search ~0.4 s.

Real devices: every open (its kind, size, time, the phone layout or not, the browser's network guess; never a path) goes to `files-timings.jsonl` beside the audit log (~1 MB kept), so what a real phone on its real link gets can be read on the box. A reload on the bad link is ~0.96 s (assets are cached by content hash; after a deploy only what changed comes again).

## Not in this version

- Charts, SmartArt and animations in .pptx; layout in .doc / .ppt (text only).
- inotify-fresh search (a rewalk every 10 minutes; changes made in Files are searchable at once).
