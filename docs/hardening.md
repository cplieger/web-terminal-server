# Security

This page is for readers who expose web-terminal-server beyond the Docker host, or who want to know what it protects and what the image contains.

## What a login gives

Anyone who can reach the server and pass its login gets an interactive process running `SESSION_CMD`, with the server's privileges. Treat it like exposing SSH.

- A binary built from source listens on `127.0.0.1` by default, so only the same host reaches it until `LISTEN_ADDR` changes.
- The image listens on `:7681`, every interface inside the container, because a published port or a reverse proxy has to reach it. With no `AUTH_PASSWORD` it is therefore open to whoever reaches the published port.
- The server logs a warning at startup when it listens on an address other than loopback without `AUTH_PASSWORD`.

Before you open it beyond a trusted host, do at least one of these:

- Set `AUTH_PASSWORD`. Every route then asks for HTTP Basic auth, the terminal connection included. Failed logins are rate-limited.
- Put a reverse proxy with its own login in front, such as Caddy with forward auth, oauth2-proxy or Authentik.
- Keep the published port on loopback or a private network.

The built-in login suits a simple setup. A reverse proxy that knows your users is the better choice for anything reachable from the internet.

## Behind a reverse proxy

[Running an app behind a reverse proxy](https://github.com/cplieger/docs/blob/main/docs/reverse-proxy.md) sets web-terminal-server up behind Caddy, nginx, Traefik or Nginx Proxy Manager, with the WebSocket, `ALLOWED_HOSTS` and `TRUSTED_PROXIES` settings it needs.

- Point the proxy at `http://127.0.0.1:7681` when it runs on the Docker host. When it runs in a container on the same Docker network, remove the `ports:` lines and point it at `http://web-terminal-server:7681`.
- Serve the page over HTTPS. Basic auth sends the password with every request.
- Drop or redact the query string of `/ws` in the proxy's own access log, as the next section explains.

## Terminal ids

A terminal connects at `/ws?session=<id>`, and holding that id is enough to join that terminal. The server keeps the id out of its own logs. The access log records the route pattern for terminal paths, and the attach record shortens the id. A reverse proxy logs full request addresses by default and would record the id in the clear, so drop or redact it there.

## DNS rebinding

DNS rebinding reaches even a terminal bound to loopback, through your own browser. A malicious page makes its own host name resolve to this server, and the browser's same-origin checks then pass because the page's origin and the `Host` header agree.

Either of two settings stops it. `ALLOWED_HOSTS` set to the exact names you browse to makes the server refuse every other `Host`. `AUTH_PASSWORD` works too, because the attacker's page cannot present your credentials. The server warns at startup when neither is set.

## Lines stored in the browser

With `PERSIST_SCROLLBACK` on, its default, the browser keeps up to 200 lines of each terminal's output in this site's `localStorage`, and about 1 MB in total across all terminals. That storage is readable from that browser without reaching the server and without the password, and it outlives the tab. An entry is deleted when you close its terminal, and otherwise after seven days. What ends up there depends on what `SESSION_CMD` prints.

Most ways to read that storage also hand over a live shell, so it is rarely the weakest point. The exception is a time when the stored lines are readable and the shell is not, such as a laptop off the VPN, a stopped container or an expired login. Set `PERSIST_SCROLLBACK=false` on a shared or borrowed device, or where storing command output on disk is not acceptable.

Nothing is sent anywhere. The server neither reads nor receives these lines, and it does not know whether a browser kept them. `localStorage` needs no permission prompt. A browser that blocks site data, or a private window, restores nothing and loads the history from the server as before.

## Limiting what a terminal can do

The terminals run as the container user, which is root by default. Restrict them as your threat model requires:

- Point `SESSION_CMD` at a command that switches to a user other than root.
- Mount only the folders the terminals need, and point `WORK_DIR` at them.

## What the image contains

| Component | Source |
| --- | --- |
| Debian, slim variant | The base image, pinned by digest. `apt-get upgrade` runs at build time, so each release ships the current Debian security updates. |
| [web-terminal-engine](https://github.com/cplieger/web-terminal-engine) | The Go engine that runs each terminal, and its TypeScript browser renderer. |
| [web-terminal-ui](https://github.com/cplieger/web-terminal-ui) | The touch-first browser interface. |
| [webhttp](https://github.com/cplieger/webhttp) | The HTTP plumbing: access logging, middleware, security headers, static files and rate limiting. |
| [envx](https://github.com/cplieger/envx), [slogx](https://github.com/cplieger/slogx) | Typed environment parsing and the structured-logging setup. |
| Monaspace Neon NF | The terminal's text font, fetched at build time and checked against a recorded digest per face. |
| [Web Terminal Glyphs](https://github.com/cplieger/web-terminal-glyphs) | The overlay font for box drawing, blocks, shades, braille and mosaics, digest-checked. The build fails if its cell size differs from the served CSS. |
| Go toolchain, TypeScript compiler | Used at build time only, both checked against a recorded digest per architecture. |

The base image is pinned by digest, and every archive downloaded at build time is checked against a recorded SHA-256. Debian packages come from Debian's signed repositories and are upgraded at each build. Updates arrive as automated pull requests and ship in a fresh image build.

The runtime image adds bash, curl and CA certificates to Debian, so the terminals start with those tools and whatever you mount.

The image also carries the license text of every bundled component under `/usr/share/licenses/`.
