# Adrian's control panel

A source snapshot of Adrian Zabica's Go and JavaScript control panel, copied
with the permission relayed by the Cube team on October 2, 2026.

The source came from `/workspace/home/projects/control-panel` on his Cube.
The snapshot includes the Go backend, frontend, tests, and helper scripts.
It excludes his compiled binary, `panel.env`, runtime directory, PIN,
Cloudflare credentials, and other account data. This is a local checkout;
nothing has been published.

## Build and test on this Cube

Go 1.27.1 is installed in a dedicated directory on this Cube. Its downloaded
archive was checked against the SHA-256 hash published by go.dev.

```sh
cd /workspace/home/repos/adrian-control-panel
export PATH="/workspace/home/.local/share/control-panel-toolchain/go/bin:$PATH"
go test ./...
node --test lib_test.js
go build -o control-panel .
```

## Run your own instance

Set a new PIN for this instance, then start the server in local mode:

```sh
cd /workspace/home/repos/adrian-control-panel
./control-panel set-pin -pin-file "$PWD/.local-state/pin"
./run-local.sh
```

The PIN prompt accepts 6–32 digits. Use your own PIN; no Adrian credentials
are needed. The app binds to loopback. The launcher enables local testing
and `-embed-local`, which uses HttpOnly, Secure, SameSite=None, Partitioned
session cookies. This lets Cube's embedded site frame keep its own login
without depending on unpartitioned third-party cookies. Cross-site POST
protection and PIN authentication remain enabled.

Open it through Cube's forwarded loopback address or `http://localhost:7777`.
Chromium accepts Secure cookies on loopback HTTP. For another computer,
forward port 7777 over SSH and use localhost there. For plain HTTP through a
non-loopback hostname, omit `-embed-local` when running the binary directly;
that mode uses Strict cookies and is intended for a standalone browser tab.

For a deployed HTTPS instance, configure your own Cloudflare Access team,
application audience, and tunnel, then replace `-insecure-no-access` with
`-cf-team YOUR_TEAM -cf-aud YOUR_AUDIENCE`. The imported `ensure-panel.sh`
is Adrian's original deployment helper and contains his directory layout;
it is preserved as source, not configured to run on this Cube.

## Integration dependencies

The system views use Linux `/proc` and utilities such as `du`, `nice`, and
`ionice`. Zellij 0.45.1 is installed at `~/.local/bin/zellij`. Terminal controls
create and use their own Zellij sessions; they do not automatically expose
Cube's ptyd terminals. Claude views expect the local user's Claude session
files. CI views expect runner directories under `~/projects/ci-cd-jobs`,
which you can change with `-ci-dir`; GitHub title lookup uses `gh`.

The application reads the home directory of the user running it. Run it
as your normal Cube user. It includes process, terminal, and cleanup actions
that operate on this Cube once you use them.

## Local instance status

The instance was started on October 2, 2026, on `127.0.0.1:7777`.
Its PID and log are in `.local-state/server.pid` and `.local-state/server.log`.
The configured PIN hash is in `.local-state/pin`; runtime files are ignored by
Git. The server is a background process, not a service configured for restart.
Browser login and creation of the `control-panel-demo` Zellij session were
verified on this Cube.
