# Monitoring and alerts

This page says how to know that web-terminal-server is up and what its log records, for anyone who runs it. The server serves no metrics page and ships no alert rules, because a person is at the terminal whenever it matters. An uptime prober on `/healthz` tells you when it is down.

## What you get

| You run | You get | You set up |
| --- | --- | --- |
| Prometheus, Mimir or VictoriaMetrics, with Alertmanager | An alert when `/healthz` stops answering `200`. This app ships no rules of its own | A probe of `/healthz` and the health page rule from the monitoring guide's [Knowing that an app is alive](https://github.com/cplieger/docs/blob/main/docs/monitoring.md#knowing-that-an-app-is-alive) |
| Loki and Alertmanager, no metric store | No alerts. The log records each failure, and no rule reads it | An uptime prober on `/healthz` |
| Grafana-managed alerting only | No rules to import. An alert when `/healthz` stops answering `200` | A probe of `/healthz` and an alert rule on it in Grafana |
| Docker or Unraid only | Docker health, which notifies nobody | An uptime prober on `/healthz`, or something that watches Docker health |

## Logs

The server writes logfmt lines to standard error, with UTC timestamps. `LOG_LEVEL` sets the level, `info` by default.

No alert rule reads the log. Search it by the `stage` key when the server does not start. Its one `ERROR` line, `web-terminal-server exited with error`, carries `stage` and the remedy in `error`. [Startup failures](how-it-works.md#startup-failures) lists the stage values, which stay the same between releases. The `terminal attach attempt` line records each request that presents a terminal id, with a shortened id, the client address and the request id. [Logs](how-it-works.md#logs) describes the request lines.

Every access line for `/healthz` is logged at `debug`, whatever its status, because a prober repeats the same answer every interval. The server logs `health state changed` at `INFO` with `healthy=true` once its listener is bound, and at `WARN` with `healthy=false draining=true` when a shutdown begins. A probe refused for its host name logs a `WARN` line from the host check.

## Knowing the app is alive

| Route | Method | Port | Login | Answers |
| --- | --- | --- | --- | --- |
| `/healthz` | `GET`, `HEAD` | `LISTEN_ADDR`, `7681` in the image | none, even with `AUTH_PASSWORD` set. `ALLOWED_HOSTS` applies, except to the image's own `web-terminal-server health` check, unless `LISTEN_ADDR` names a host other than `localhost` | `200 {"status":"ok"}`, or `503 {"status":"unhealthy","reasons":[...]}` |

**What healthy means.** The server is listening and is not shutting down. When a shutdown begins, `/healthz` answers `503` with the reason `starting up or shutting down` until the listener closes. Any other method answers `405`. The answer carries only that verdict, so it needs no login. A prober that still sends the login keeps working. A program that hangs in one tab affects only that tab, with you in front of it. The server answers for every tab, so it stays healthy.

**Docker health.** The image checks itself every 30 seconds with `web-terminal-server health`, which asks `/healthz` on the container's loopback, or on the address `LISTEN_ADDR` names, and gives up after 4 seconds. Docker marks the container unhealthy after 3 failures, and allows 15 seconds after a start. Docker's health status notifies nobody on its own, and Docker does not restart an unhealthy container.

**Who is told.**

| You run | A dead server | A wedged server |
| --- | --- | --- |
| Prometheus with the guide's health page rule | the health page rule | the same, once `/healthz` stops answering `200` |
| Loki only | an uptime prober on `/healthz` | the same |
| Docker or Unraid only | an uptime prober on `/healthz`, or something that watches Docker health | the same |

**Probing from outside.** Check `GET http://<host>:7681/healthz` and pass on status 200 only. Do not match the body. Use a 5-second timeout, check every 60 seconds, alert after 3 failures, and wait 15 seconds after a first start.

When you set `ALLOWED_HOSTS`, address the server by a name in it, or it refuses the request with `403`. A reverse proxy that asks for a login refuses a prober that does not send it, so probe the port directly on your network, or give the prober the proxy's login. A Kubernetes liveness probe that must ignore readiness uses a `tcpSocket` probe on the port.
