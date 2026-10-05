# Control Panel

A Linux control panel for machine resources, terminals, Claude sessions,
CI runners, and security. Built by **Adrian Zabica** in Go and JavaScript,
with integration by the [Cube Computer](https://cube.computer) team.

This repository preserves Adrian's source and the Cube integration work.
The source was shared with permission relayed by the Cube team on
October 2, 2026. It contains no instance configuration or account data.

## Install in Cube

In **Market**, choose **Add app from repo** and enter:

```text
https://github.com/collabs-inc/control-panel
```

The app runs on Linux. Install Go 1.27.1 or later on your Cube before
installing the app; `install.sh` builds the server using Go from `PATH`
or `~/.local/share/control-panel-toolchain/go/bin`. Terminal features
also require Zellij (tested with 0.45.1) on `PATH` or in `~/.local/bin`.

Cube reads `cube.json`, builds the app, and opens it using Cube's sign-in.

## Build and test

Requires Linux and Go 1.27.1 or later. Node.js is needed only for the
JavaScript tests; the frontend has no build step.

```sh
git clone https://github.com/collabs-inc/control-panel.git
cd control-panel
go test ./...
node --test lib_test.js
go build -o control-panel .
```

## Run your own instance

Set a new PIN for this instance, then start the server in local mode:

```sh
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

## Run as a Cube app

`cube.json` makes this repository a Cube app: Cube clones it, runs
`install.sh` (which builds the binary with Go from `PATH`, or from
`~/.local/share/control-panel-toolchain/go/bin`), and starts
`./control-panel -behind-cube -addr 127.0.0.1:$PORT`. With `-behind-cube`,
Cube's gate signs every visitor in before a request reaches the panel, so
there is no PIN screen and no Cloudflare Access check; the loopback bind and
the cross-origin protection stay on. It cannot be combined with `-cf-team`.
The flag trusts anything that reaches the panel's own port: the PIN used to
cover a program on the same machine, and a page that re-points its domain at
`127.0.0.1`, calling that port directly. On a cloud Cube no browser can reach
it; on a local machine, prefer the PIN.
Data stays in `~/.config/control-panel` and `~/.local/state/control-panel`,
outside the app's folder, so an update keeps it.

## Integration dependencies

The system views use Linux `/proc` and utilities such as `du`, `nice`, and
`ionice`. Install Zellij separately for terminal controls, which
create and use their own Zellij sessions; they do not automatically expose
Cube's ptyd terminals. Claude views expect the local user's Claude session
files. CI views expect runner directories under `~/projects/ci-cd-jobs`,
which you can change with `-ci-dir`; GitHub title lookup uses `gh`.

The application reads the home directory of the user running it. Run it
as your normal Cube user. It includes process, terminal, and cleanup actions
that operate on this Cube once you use them.
