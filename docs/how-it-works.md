# How web-terminal-server works

This page covers how terminals run and reconnect, what happens at startup, what the logs record and what the healthcheck checks. It is for readers who operate the server or want to know what happens underneath.

## Terminals and tabs

The server starts one command per tab in a real pseudo-terminal, the same kind a local terminal window uses, and streams it to the browser. The browser shows the full screen, scrollback, mouse input, colours and clickable links, driven by touch on a phone or a keyboard on a desktop.

Tabs live on the server. Closing the page does not stop what runs in them, and reopening it attaches to the same terminals from any device. Tab names and the split-view layout are stored on the server with the tabs, so a reload or another device shows the same arrangement. The terminals are processes inside the container, so stopping or restarting the container ends them.

## Why the container needs an init

Whatever `SESSION_CMD` runs can start a child process that outlives its own parent, and the kernel then hands that orphan to PID 1. This server waits only for the processes it started itself. Without an init it is PID 1, and every orphan stays behind as a zombie process for the life of the container. Docker's `--init`, or `init: true` in compose, puts a small init at PID 1 that collects them. The server logs a warning at startup when it finds itself running as PID 1, and keeps serving.

## Reloading a page

With `PERSIST_SCROLLBACK` on, the browser keeps the newest 200 lines of each terminal. A reload then asks the server only for what was printed while the page was gone. Without it, the terminal comes back empty and loads the whole history the server keeps, and you see it fill in. On a phone that is the normal case. iOS discards background tabs when memory runs low, and returning to one loads the page again. Reconnecting an open page and switching tabs within one page already replay nothing, so the setting changes only a fresh page load.

On the first reconnect, the stored lines are checked against the running server. They are cleared if they came from an earlier run, because a restarted server numbers its output from the start again. A restart usually leaves that terminal gone. The stored lines are then discarded rather than shown behind a "Session ended" banner. What is stored, and the privacy side of it, is in [Security](hardening.md#lines-stored-in-the-browser).

## Startup failures

A startup failure produces exactly one `ERROR` line, `web-terminal-server exited with error`. Its `error` field carries the remedy, and its `stage` field names the step that failed:

| `stage` | What failed |
| --- | --- |
| `config` | The environment is invalid, such as a bad `WORK_DIR`, an empty `SESSION_CMD`, or an unreadable `PERSIST_SCROLLBACK`, `SCROLLBACK` or `IDLE_TIMEOUT`. |
| `static` | The bundled web page or its Content-Security-Policy is unusable. This is a build defect, not a setting, and no environment change fixes it. |
| `listen` | The address in `LISTEN_ADDR` could not be bound. |
| `serve` | The HTTP server stopped with an error while running. |
| `unknown` | A failure no step claimed. |

Key log queries and alert rules on `stage` rather than on the message text. The values are a stable contract with a test behind them, and the wording is not.

A rejected environment value never appears in that line. Only the variable's name and the accepted shape do, because a compose mistake is what puts a password on the wrong variable.

## Logs

The server writes one structured log line per request, with UTC timestamps whatever the container's `TZ`. It has no metrics endpoint.

Each request line records a `client_ip`. With `TRUSTED_PROXIES` unset, that is the direct peer and any `X-Forwarded-For` header is ignored, so the logged address cannot be faked. That is right when the server is reached directly. Behind a reverse proxy the direct peer is the proxy, so set `TRUSTED_PROXIES` to its addresses, for example `10.0.0.0/8,192.0.2.10`. Only a request whose peer is in that set has its `X-Forwarded-For` read. A malformed entry is logged and skipped, and startup goes on.

A terminal that attaches successfully through the `/ws` handshake gets no request line. The handshake ends the HTTP exchange, so such a line could only be written when the connection closes. It would carry a duration as long as the session and a status the server never sent. Every attach attempt is recorded separately as `terminal attach attempt`, before the handshake runs, with a shortened terminal id, the client address and the request id. That record is the audit trail for anyone presenting a terminal id.

A refused handshake keeps its request line with its real status. Examples are a rejected `Host`, a cross-origin request, missing credentials, or a plain HTTP request with no upgrade headers. That is what to search for when a browser cannot attach.

## Healthcheck

The image's `HEALTHCHECK` calls `/healthz` on `127.0.0.1` every 30 seconds, after a 15-second start period. `/healthz` answers `200 {"status":"ok"}` once the listener is bound. It answers `503` during startup and while a shutdown drains connections, so a load balancer stops sending traffic first.

- The probe reads its port from `LISTEN_ADDR`, so moving the listener keeps it working.
- When `AUTH_PASSWORD` is set, the probe logs in with `AUTH_USERNAME` and `AUTH_PASSWORD` through a curl config file on standard input rather than a command-line flag. The password therefore never appears in the container's process list.
- It reports readiness, not liveness. Nothing restarts the container when it is unhealthy, so a problem shows as `unhealthy` in `docker ps` without a restart loop.

## Routes

- `/` serves the web page.
- `/ws?session=<id>` is the terminal connection, one WebSocket per terminal.
- `/api/sessions` creates, lists and closes terminals. Creating one is rate-limited.
- `/api/sessions/{id}/pinned-title` names a terminal. `PUT` sets the name and `DELETE` returns to the automatic one.
- `/api/sessions/events` streams terminal status to the page.
- `/healthz` reports readiness.

## What this repository holds

The terminal itself is two shared libraries by the same author, [web-terminal-engine](https://github.com/cplieger/web-terminal-engine), the Go engine and its browser renderer, and [web-terminal-ui](https://github.com/cplieger/web-terminal-ui), the touch-first interface. This repository is two Go files that start each terminal, serve the bundled page and apply the security settings described in [Security](hardening.md).

Two other apps run on the same engine. [Web Terminal for Kiro](https://github.com/cplieger/web-terminal-kiro) is a touch-first, multi-tab terminal wired to the Kiro CLI, on desktop or phone. [marotte](https://github.com/cplieger/marotte) puts Kiro in your browser as a self-hosted agentic IDE, with chat, a file editor, a terminal and git in one tab.
