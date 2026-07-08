# web-terminal-server

[![Image Size](https://ghcr-badge.egpl.dev/cplieger/web-terminal-server/size)](https://github.com/cplieger/web-terminal-server/pkgs/container/web-terminal-server)
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
engine. A native-touch terminal in the browser for any command — phone and
desktop alike.

Published as a container image: `ghcr.io/cplieger/web-terminal-server`.

## ⚠️ Security: this is a remote shell

Anyone who can reach the server **and pass auth (if configured)** gets an
interactive process running `WT_CMD` with this server's privileges. Treat it
like exposing SSH.

- **The binary binds `127.0.0.1` by default.** Reachable only from the same
  host until you change `WT_ADDR`.
- **The container image binds `:7681`** (it has to, to be reachable via a
  published port) and so is **unauthenticated and network-exposed by default**.
  Before exposing it beyond a trusted host, do **one** of:
  - set `WT_PASSWORD` (enables HTTP Basic auth on every route, including the
    WebSocket handshake), and/or
  - front it with an authenticating reverse proxy (Caddy + forward-auth,
    oauth2-proxy, Authentik, …), and/or
  - keep the published port bound to loopback / a private network only.
- The server logs a loud warning at startup when it is listening on a
  non-loopback address without `WT_PASSWORD` set.

Built-in Basic auth is a convenience for simple setups; a reverse proxy with
real identity is the recommended posture for anything internet-facing. The
process runs as the container user (root by default) — restrict it with a
non-root `WT_CMD` target, a read-only root filesystem, dropped capabilities,
and a scoped work directory as your threat model requires.

## Run

```sh
docker run --rm -p 127.0.0.1:7681:7681 \
  -e WT_PASSWORD=changeme \
  -v "$PWD":/work -e WT_WORKDIR=/work \
  ghcr.io/cplieger/web-terminal-server
```

Open <http://127.0.0.1:7681>. The example binds the published port to loopback
and sets a password; adjust for your environment.

## Configuration

All configuration is via environment variables:

| Variable | Default (binary / image) | Purpose |
| --- | --- | --- |
| `WT_ADDR` | `127.0.0.1:7681` / `:7681` | Listen address. The binary defaults to loopback; the image must listen on all interfaces. |
| `WT_CMD` | `/bin/bash` | Command to run in the PTY, whitespace-split (use a wrapper script for complex commands). |
| `WT_WORKDIR` | _(process default)_ | Working directory for the command. Must be an existing directory if set. |
| `WT_SCROLLBACK` | `5000` | Lines of scrollback the server retains for reconnect replay. |
| `WT_IDLE_REAPER` | _(unset → disabled)_ | Go duration (e.g. `30m`); when > 0, idle sessions are reaped after this long. |
| `WT_USERNAME` | `admin` | Basic-auth username (only used when `WT_PASSWORD` is set). |
| `WT_PASSWORD` | _(unset → no auth)_ | Basic-auth password. When set, every route (including `/ws`) requires it. |
| `WT_TRUSTED_PROXIES` | _(unset → socket peer)_ | Comma-separated reverse-proxy CIDRs / bare IPs whose `X-Forwarded-For` the access log trusts to resolve `client_ip`. See [Client IP logging](#client-ip-logging). |

Endpoints: `/` (UI), `/ws?session=<id>` (per-session terminal WebSocket), `/api/sessions` (create/list/close), `/api/sessions/events` (status SSE), `/healthz` (readiness).

### Client IP logging

The access log records a `client_ip` per request. By default (`WT_TRUSTED_PROXIES` unset) it logs the direct socket peer and ignores any `X-Forwarded-For` header, so the logged IP cannot be spoofed; that's the correct choice when the server is directly exposed. Behind a reverse proxy the socket peer is the proxy, not the user, so set `WT_TRUSTED_PROXIES` to the proxy's address(es), a comma-separated list of CIDRs or bare IPs (e.g. `WT_TRUSTED_PROXIES=10.0.0.0/8,192.168.1.5`), and the log resolves the real client from a trusted `X-Forwarded-For`. Only a request whose socket peer is inside the set has its `X-Forwarded-For` trusted (spoof-safe); a malformed entry is logged and skipped rather than aborting startup. Log timestamps are UTC regardless of the container's `TZ`, so lines stay zone-stable for ingest.

## How it fits together

```
github.com/cplieger/web-terminal-engine   (Go engine: PTY + VT screen + wire protocol)
        │
        ├── terminal.NewSessionManager ─────►  this server (main.go)
        │
@cplieger/web-terminal-engine  +  @cplieger/web-terminal-ui   (TS engine + UI)
        └── compiled to static/vendor/ at image build, served to the browser
```

The server is deliberately thin: env parsing, `terminal.NewSessionManager`, the
session REST API + status SSE, a create rate limit, static file serving, optional
Basic auth, and graceful shutdown. All terminal behavior lives in the engine and
UI packages.

## Related projects

The web-terminal family:

- [`web-terminal-engine`](https://github.com/cplieger/web-terminal-engine) — the
  Go session engine + TypeScript browser renderer this server embeds.
- [`@cplieger/web-terminal-ui`](https://github.com/cplieger/web-terminal-ui) —
  the touch-first browser UI this server ships to the client.

Apps built on the same engine:

- [`vibekit`](https://github.com/cplieger/vibekit)
- [`web-terminal-kiro`](https://github.com/cplieger/web-terminal-kiro)

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See `LICENSE`.
