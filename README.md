# web-terminal-server

[![Image Size](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/web-terminal-server/badges/size.json)](https://github.com/cplieger/web-terminal-server/pkgs/container/web-terminal-server)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-blue)
![base: Debian](https://img.shields.io/badge/base-Debian-A81D33?logo=debian)
[![Test coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/cplieger/web-terminal-server/badges/coverage.json)](https://github.com/cplieger/web-terminal-server/actions/workflows/coverage.yml)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13432/badge)](https://www.bestpractices.dev/projects/13432)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/cplieger/web-terminal-server/badge)](https://scorecard.dev/viewer/?uri=github.com/cplieger/web-terminal-server)
[![SBOM](https://img.shields.io/badge/SBOM-SPDX-1D4ED8)](https://github.com/cplieger/web-terminal-server/releases)

A small, generic web terminal: it runs a configured command in a PTY and serves
the [`@cplieger/web-terminal-ui`](https://github.com/cplieger/web-terminal-ui)
front end over HTTP + WebSocket, built on the
[`github.com/cplieger/web-terminal-engine`](https://github.com/cplieger/web-terminal-engine)
engine. A native-touch terminal in the browser for any command, on phone and
desktop alike.

Published as a multi-arch (amd64 + arm64) container image on **GHCR** (`ghcr.io/cplieger/web-terminal-server`) and **Docker Hub** (`cplieger/web-terminal-server`).

![web-terminal-server in the browser: a multi-tab, touch-first terminal with a shell prompt and a tab bar across the bottom.](docs/screenshot.png)

## ⚠️ Security: this is a remote shell

Anyone who can reach the server **and pass auth (if configured)** gets an
interactive process running `SESSION_CMD` with this server's privileges. Treat it
like exposing SSH.

- **The binary binds `127.0.0.1` by default.** Reachable only from the same
  host until you change `LISTEN_ADDR`.
- **The container image binds `:7681`** (it has to, to be reachable via a
  published port) and so is **unauthenticated and network-exposed by default**.
  Before exposing it beyond a trusted host, do **one** of:
  - set `AUTH_PASSWORD` (enables HTTP Basic auth on every route, including the
    WebSocket handshake), and/or
  - front it with an authenticating reverse proxy (Caddy + forward-auth,
    oauth2-proxy, Authentik, …), and/or
  - keep the published port bound to loopback / a private network only.
- The server logs a loud warning at startup when it is listening on a
  non-loopback address without `AUTH_PASSWORD` set.
- Each session's recent output (200 lines) is kept in the browser's `localStorage` by default so a
  reloaded tab does not refill over the wire. It is readable from that browser without
  passing `AUTH_PASSWORD` and outlives the tab; set `PERSIST_SCROLLBACK=false` on a
  shared device or where storing command output at rest is unacceptable. See
  [Persisted scrollback](#persisted-scrollback).
- **DNS rebinding reaches even loopback binds** through your own browser: an
  attacker's page makes its hostname resolve to this server, and same-origin
  checks then pass because `Origin` and `Host` agree. Set `ALLOWED_HOSTS`
  to the exact hostnames you browse to (rejects every other `Host`), or set
  `AUTH_PASSWORD` (the attacker's page cannot present credentials). The server
  warns at startup when neither is set.

Built-in Basic auth is a convenience for simple setups; a reverse proxy with
real identity is the recommended posture for anything internet-facing. The
process runs as the container user (root by default); restrict it with a
non-root `SESSION_CMD` target, a read-only root filesystem, dropped capabilities,
and a scoped work directory as your threat model requires.

## What it does

It starts one command per terminal tab in a real PTY and streams that terminal
to a browser: full VT screen buffer, scrollback, mouse, colours and clickable
hyperlinks, driven by touch on a phone as well as a keyboard on a desktop. Tabs
live on the server, so closing the page does not kill what is running, and
reopening it reattaches to the same terminals from any device.

It is deliberately thin. The terminal itself is two shared libraries (the engine
and its reference UI); this repo is two small Go files that start the PTY, serve
the bundled front end, and apply the security posture described above.

## Run

[`compose.yaml`](compose.yaml) in this repo is a working example: loopback-bound,
password set, a work directory mounted, and `init: true`. Copy it and adjust.

Or as a one-shot run:

```sh
docker run --rm --init -p 127.0.0.1:7681:7681 \
  -e AUTH_PASSWORD=changeme \
  -v "$PWD":/work -e WORK_DIR=/work \
  ghcr.io/cplieger/web-terminal-server
```

Open <http://127.0.0.1:7681>. Both examples bind the published port to loopback
and set a password; adjust for your environment.

**`--init` / `init: true` is required, not cosmetic.** Whatever `SESSION_CMD` runs can
fork a child that outlives its own parent, and the kernel reparents that orphan
onto PID 1. This server waits only for the processes it started itself, so with no
init it is PID 1 and every orphan stays a zombie for the container's lifetime.
Docker's `--init` puts a small reaper there instead, which owns nothing anything
else is waiting on. The server logs a warning at startup when it finds itself
running as PID 1.

## Naming terminals

Each terminal tab is labelled automatically: whatever is running in it (the foreground command), or the directory it sits in when the shell is idle, or the window title the program set for itself. Right-click a tab (press `F2`, or double-click it) to type your own name instead, which then sticks for the life of that terminal and shows on every device you have the page open on. The same menu offers **Use automatic name** to remove it again.

Names live on the server, not in your browser, so they survive a reload and are the same in every window.

## Persisted scrollback

On by default. Set `PERSIST_SCROLLBACK=false` to turn it off.

What it does: the browser keeps the newest 200 lines of each session, so a reload
asks the server only for what was printed while the page was gone. Without it the
terminal comes back holding nothing and pulls the whole retained scrollback back
over the wire — you see the history filling in, and on a phone that is the normal
case rather than an edge case, because iOS discards backgrounded tabs under memory
pressure and returning to one re-runs the page. A warm reconnect and switching
between tabs in one page already replay nothing, so this only changes the
fresh-load case.

What it stores, which is the reason there is a switch at all: up to 200 lines of
each session's output, in this origin's `localStorage`, and at most about 1 MB in
total across every session. That is readable from that
browser without reaching this server and without passing `AUTH_PASSWORD`, and it
outlives the tab that produced it — an entry is deleted when you close its
terminal, and otherwise after seven days. `SESSION_CMD` decides what ends up in there.

Most ways to read it also hand over a live shell, so the snapshot is rarely the
weakest thing available. The exception worth knowing is a window where the snapshot
is readable and the shell is not: a laptop off the VPN, a stopped container, an
expired credential. **Turn it off on a shared or borrowed device, or where storing
command output at rest is not acceptable.**

Nothing is ever sent anywhere: the server neither reads nor receives these
snapshots, and it does not know whether a browser kept one. No permission prompt is
involved — `localStorage` needs none — and a browser that blocks site data, or a
private window, simply restores nothing and replays over the wire as before.

Restored content is checked against the running server on the first reconnect and
cleared if it came from a previous run, which covers the confusing case: a restarted
server numbers its output from the beginning again. If that session is already gone
— which is what a restart usually leaves — the restore is discarded outright rather
than shown behind a “Session ended” banner.

## Configuration reference

All configuration is via environment variables. Where the binary and image
defaults differ, the Default column shows them as binary / image.

| Variable | Description | Default |
| --- | --- | --- |
| `LISTEN_ADDR` | Listen address. The binary defaults to loopback; the image must listen on all interfaces. The baked healthcheck derives its port from this value and probes `127.0.0.1`, so changing the port is safe but pinning the bind to one non-loopback interface makes the container report `unhealthy` while serving normally. | `127.0.0.1:7681` / `:7681` |
| `LOG_LEVEL` | Log verbosity: `debug`, `info`, `warn`, or `error` (case-insensitive; slog offset syntax like `warn+1` also parses). An unparseable value falls back to `info` with a startup warning. | `info` |
| `SESSION_CMD` | Command to run in the PTY, whitespace-split (use a wrapper script for complex commands). | `/bin/bash` |
| `WORK_DIR` | Working directory for the command. Must be an existing directory if set. | _(process default)_ |
| `SCROLLBACK` | Lines of history the server retains per session — how far back a user can scroll, and what a reconnect can replay. Kept in memory and grown as history is produced, so a large value costs nothing until a session actually reaches it: to say "never truncate", set a number no session will hit. `0` retains nothing beyond the live screen. Values between `1` and `2000` are raised to `2001` with a warning, because at or below the depth a reconnect replays in full there is nothing left to page for, so the browser falls back to holding its whole buffer — asking for less server history would cost the phone more. This is the terminal engine's own variable, shared verbatim with every app built on it, which is why this server holds no default of its own. | `100000` |
| `PERSIST_SCROLLBACK` | Keep each session's recent scrollback in the browser's `localStorage`, so a reloaded or browser-discarded tab resumes with a delta instead of refilling its whole buffer over the wire. Set `false` to turn it off — see [Persisted scrollback](#persisted-scrollback). | `true` |
| `IDLE_TIMEOUT` | Go duration (e.g. `30m`); when > 0, idle sessions are reaped after this long. | _(unset → disabled)_ |
| `AUTH_USERNAME` | Basic-auth username (only used when `AUTH_PASSWORD` is set). | `admin` |
| `AUTH_PASSWORD` | Basic-auth password. When set, every route (including `/ws`) requires it. | _(unset → no auth)_ |
| `ALLOWED_HOSTS` | Comma-separated exact hostnames/IPs the server answers for; any other `Host` header is rejected (the DNS-rebinding guard; see the security warning above). The carve-out needs loopback on **both** ends — a loopback client address _and_ a loopback `Host` — so the image healthcheck and a same-host `curl` keep working while a forged loopback `Host` from a remote peer does not. Any other name you browse to must be listed. Malformed entries are dropped with a warning; a list whose entries are **all** malformed fails closed, rejecting every non-loopback request. | _(unset)_ |
| `TRUSTED_PROXIES` | Comma-separated reverse-proxy CIDRs / bare IPs whose `X-Forwarded-For` the access log trusts to resolve `client_ip`. See [Client IP logging](#client-ip-logging). | _(unset → socket peer)_ |

Endpoints: `/` (UI), `/ws?session=<id>` (per-session terminal WebSocket), `/api/sessions` (create/list/close), `/api/sessions/{id}/pinned-title` (name a terminal; `PUT` to set, `DELETE` to go back to the automatic name), `/api/sessions/events` (status SSE), `/healthz` (readiness).

The variables above are the whole operator surface. Separately, the terminal
engine injects one `WT_`-prefixed key into each session's OWN environment,
`WT_SESSION_REAP`, so you see it in `env` inside a terminal. It is internal
plumbing, not a setting: the session reaper matches it on the exact `KEY=VALUE`
pair. Do not set it.

### Volumes

| Mount | Description |
| --- | --- |
| _(any path)_ | Nothing is required. Mount whatever the command needs to reach and point `WORK_DIR` at it; the examples above mount the current directory at `/work`. |

### Ports

| Port | Description |
| --- | --- |
| `7681` | HTTP + WebSocket. Serves the UI, the session API and the terminal socket. Change the listen address with `LISTEN_ADDR`; the healthcheck follows it. |

## Startup failures

A startup failure produces exactly one `ERROR` line, `web-terminal-server exited
with error`. The remedy is in its `error` field, and a `stage` field names which
step failed:

| `stage` | What failed |
| --- | --- |
| `config` | The environment is invalid: a `WORK_DIR` that is missing, unreadable or not a directory, an empty `SESSION_CMD`, or an unparseable `PERSIST_SCROLLBACK`, `SCROLLBACK` or `IDLE_TIMEOUT`. |
| `static` | The embedded front end or its Content-Security-Policy is unusable. A build defect, not a setting: no environment change fixes it. |
| `listen` | The address in `LISTEN_ADDR` could not be bound. |
| `serve` | The HTTP server exited with an error while running. |
| `unknown` | A failure nothing attributed. |

Key log queries and alert rules on `stage` rather than on the message text: the
values are a stable contract with a test behind them, the prose is not.

A rejected environment value is never echoed into that line. Only the variable's
name and the accepted shape appear, because a compose interpolation mistake is
what puts a credential on a variable in the first place.

## Client IP logging

The access log records a `client_ip` per request. By default (`TRUSTED_PROXIES` unset) it logs the direct socket peer and ignores any `X-Forwarded-For` header, so the logged IP cannot be spoofed; that's the correct choice when the server is directly exposed. Behind a reverse proxy the socket peer is the proxy, not the user, so set `TRUSTED_PROXIES` to the proxy's address(es), a comma-separated list of CIDRs or bare IPs (e.g. `TRUSTED_PROXIES=10.0.0.0/8,192.0.2.10`), and the log resolves the real client from a trusted `X-Forwarded-For`. Only a request whose socket peer is inside the set has its `X-Forwarded-For` trusted (spoof-safe); a malformed entry is logged and skipped rather than aborting startup. Log timestamps are UTC regardless of the container's `TZ`, so lines stay zone-stable for ingest.

A terminal that successfully attaches (the `/ws` WebSocket handshake) gets no line: the handshake ends the HTTP exchange, so the line could only be written when the socket finally closes, reporting a session-long duration and a status the server never sent. Every attach ATTEMPT is recorded separately, before the handshake runs, with a truncated session id, the client IP and the request id — that record is the audit trail for a socket that presents a session credential. A handshake that is _refused_ is also logged with its real status — a rejected `Host`, a cross-origin request, missing credentials, a plain HTTP request with no upgrade headers — so that is what to grep when a browser cannot attach.

The session id in `/ws?session=<id>` is a **capability**: holding it is enough to attach to that terminal. This server keeps it out of its own logs (the access log records the route template for session paths, and the attach record truncates the id), but a fronting reverse proxy logs full request URIs by default and would capture it in the clear. Drop or redact the `/ws` query string in the proxy's own access log.

## Healthcheck

The image ships a `HEALTHCHECK` that probes `/healthz` on loopback every 30s,
after a 15s start period. `/healthz` answers `200 {"status":"ok"}` once the
listener is bound and `503` during startup and the graceful-shutdown drain, so a
load balancer stops sending traffic while the server is draining.

Two details worth knowing. The probe derives its port from `LISTEN_ADDR`, so moving
the listener keeps it working. And when `AUTH_PASSWORD` is set the probe
authenticates with `AUTH_USERNAME`/`AUTH_PASSWORD` through a curl config file on
stdin rather than a command-line flag, so the password never appears in the
container's process list.

It is a readiness signal, not liveness: nothing restarts the container on an
unhealthy state, so a problem surfaces as `unhealthy` in `docker ps` without a
restart loop.

## Dependencies

| Dependency | Source |
| --- | --- |
| Debian trixie-slim | Base image, pinned by digest. `apt-get upgrade` runs at build time so security updates ship with each release. |
| `github.com/cplieger/web-terminal-engine` | The Go PTY/VT session engine and its TypeScript browser renderer. |
| `@cplieger/web-terminal-ui` | The touch-first browser UI served to the client. |
| `github.com/cplieger/webhttp` | Server HTTP plumbing: access logging, middleware chain, security headers, static serving, rate limiting. |
| `github.com/cplieger/envx`, `slogx` | Typed environment parsing and the fleet-standard slog setup. |
| Monaspace Neon NF | The terminal webfont, fetched at build time and digest-verified per face. |
| Go toolchain, TypeScript compiler | Build-time only, both digest-verified per architecture. |

Every version is pinned, and every build-time download is checked against a
recorded sha256, so a compromised registry cannot change what the image contains.
Updates arrive as automated pull requests and ship through a fresh image build.

## Related projects

The web-terminal family:

- [`web-terminal-engine`](https://github.com/cplieger/web-terminal-engine): the
  Go session engine + TypeScript browser renderer this server embeds.
- [`@cplieger/web-terminal-ui`](https://github.com/cplieger/web-terminal-ui):
  the touch-first browser UI this server ships to the client.

Apps built on the same engine:

- [`vibekit`](https://github.com/cplieger/vibekit): a chat-first browser front end for the Kiro CLI (chat history, MCP, editor, git/forge workflows).
- [`web-terminal-kiro`](https://github.com/cplieger/web-terminal-kiro): a touch-first, multi-tab browser terminal wired to the Kiro CLI (`kiro-cli`), on desktop or phone.

## Contributing

Issues and PRs are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
conventions and how to run the checks locally.

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude](https://claude.com), [GPT](https://openai.com), and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

MPL-2.0. See [LICENSE](LICENSE).
