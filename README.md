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
| `WT_USERNAME` | `admin` | Basic-auth username (only used when `WT_PASSWORD` is set). |
| `WT_PASSWORD` | _(unset → no auth)_ | Basic-auth password. When set, every route (including `/ws`) requires it. |

Endpoints: `/` (UI), `/ws` (terminal WebSocket), `/healthz` (readiness).

## How it fits together

```
github.com/cplieger/web-terminal-engine   (Go engine: PTY + VT screen + wire protocol)
        │
        ├── terminal.Handler ──────────────►  this server (main.go)
        │
@cplieger/web-terminal-engine  +  @cplieger/web-terminal-ui   (TS engine + UI)
        └── compiled to static/vendor/ at image build, served to the browser
```

The server is deliberately thin: env parsing, `terminal.NewHandler`, static
file serving, optional Basic auth, and graceful shutdown. All terminal
behavior lives in the engine and UI packages.

## Related projects

The web-terminal family:

- [`web-terminal-engine`](https://github.com/cplieger/web-terminal-engine) — the
  Go session engine + TypeScript browser renderer this server embeds.
- [`@cplieger/web-terminal-ui`](https://github.com/cplieger/web-terminal-ui) —
  the touch-first browser UI this server ships to the client.

Apps built on the same engine:

- [`vibekit`](https://github.com/cplieger/vibekit)
- [`vibecli`](https://github.com/cplieger/vibecli)

## Disclaimer

This project is built with care and follows security best practices, but it is intended for personal / self-hosted use. No guarantees of fitness for production environments. Use at your own risk.

This project was built with AI-assisted tooling using [Claude Opus](https://www.anthropic.com/claude) and [Kiro](https://kiro.dev). The human maintainer defines architecture, supervises implementation, and makes all final decisions.

## License

GPL-3.0-or-later. See `LICENSE`.
