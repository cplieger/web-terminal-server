# Configuration

This page explains every setting of web-terminal-server, for readers who want to change more than the quick start sets. The README's [Configuration reference](../README.md#configuration-reference) has the short table.

## Where settings live

All configuration is environment variables. The server reads them once at start, so recreate the container after a change. These variables are the whole set.

Where the image and a binary built from source differ, this page says so. The image sets `LISTEN_ADDR=:7681` and `SESSION_CMD=/bin/bash`.

## Login

- `AUTH_PASSWORD` turns on HTTP Basic auth. Every route then asks for it, the terminal connection at `/ws` included. Unset, there is no login.
- `AUTH_USERNAME` is the user name for that login. It defaults to `admin` and is used only when `AUTH_PASSWORD` is set.

A password made only of spaces gives almost no protection, and the server warns about it when it listens beyond loopback. Failed logins are rate-limited, and a request with the right password is never slowed.

## Host names

`ALLOWED_HOSTS` is a comma-separated list of exact host names and IPs this server answers for. A request with any other `Host` header is refused with `host not allowed`. This is what stops DNS rebinding, explained in [Security](hardening.md#dns-rebinding).

- Write bare names or IPs, such as `localhost,192.0.2.10,term.example.com`. A scheme, a path, a CIDR or a port does not belong here.
- A request from a loopback client to a loopback host name is always accepted. That keeps the healthcheck and a `curl` on the same host working, while a remote client sending a loopback `Host` is still refused.
- A malformed entry is dropped with a warning that gives only the count. If every entry is malformed, the server refuses every request except from loopback.
- Unset, the server accepts any host name. Without `AUTH_PASSWORD` it then warns at startup that DNS rebinding is open.

## What each terminal runs

- `SESSION_CMD` is the command each terminal runs. It defaults to `/bin/bash`. The value is split on spaces and nothing else, so put a command with quotes or pipes in a script and point `SESSION_CMD` at the script. An empty value stops startup.
- `WORK_DIR` is the folder each new terminal starts in. It must be an existing directory. A missing folder, an unreadable one and a file each stop startup with their own message. Unset, terminals start in the server's own working directory.
- `IDLE_TIMEOUT` closes a terminal that no browser has had open for that long. Write it as a duration such as `30m` or `2h`. Unset or `0` keeps terminals until they exit or the container stops. A negative or unreadable value stops startup.

## Scrollback

- `SCROLLBACK` is how many lines of history the server keeps per terminal. That is how far back you can scroll, and what a reconnecting page can replay. It defaults to `100000`. Memory is used only as history is produced, so a large value costs nothing until a terminal reaches it. To keep everything, set a number no terminal will reach. `0` keeps nothing beyond the screen.
- A value from `1` to `2000` is raised to `2001` with a warning. Below that depth the browser cannot load older history on demand and keeps its whole buffer instead, so less server history would cost the phone more memory.
- `SCROLLBACK` is the terminal engine's own variable, which is why its name has no prefix and why this server sets no default of its own.
- `PERSIST_SCROLLBACK` keeps each tab's newest 200 lines in the browser, so a reloaded page asks the server only for what it missed. It defaults to `true`. Set `false` to turn it off. Any value other than a boolean stops startup. What the browser stores, and for how long, is in [Security](hardening.md#lines-stored-in-the-browser).

## Network and logs

- `LISTEN_ADDR` is the listen address as `host:port`. The image listens on `:7681`, every interface inside the container. A binary built from source defaults to `127.0.0.1:7681`. The healthcheck reads the port from this value and always calls `127.0.0.1`, so changing the port is safe. Naming one interface other than loopback makes the container report `unhealthy` while it serves normally.
- `TRUSTED_PROXIES` is a comma-separated list of reverse-proxy CIDRs or bare IPs, such as `10.0.0.0/8,192.0.2.10`. Only a request from one of them has its `X-Forwarded-For` header read for the access log's `client_ip`. A malformed entry is skipped with a warning. Unset, the log records the direct peer. [How it works](how-it-works.md#logs) has the details.
- `LOG_LEVEL` is `debug`, `info`, `warn` or `error`, in any case. A level offset such as `warn+1` also works. An unknown value falls back to `info` with a warning. Log timestamps are UTC whatever the container's `TZ`.

A rejected value never appears in a log line or an error. The message names the variable and the shape it accepts, because a compose mistake can put a password on the wrong variable.

## A variable you see but do not set

Inside a terminal, `env` shows `WT_SESSION_REAP`. The terminal engine adds it to each terminal's own environment so it can find and stop that terminal's processes. It is not a setting. Do not set it.

## Mounts

Nothing is required. Mount whatever the terminals should reach, and point `WORK_DIR` at it. The example mounts `./work` at `/work`. The container runs as root, so files the terminals create there belong to root on the host.

## Ports

Port `7681` serves the web page, the terminal API and the terminal connections. Change it with `LISTEN_ADDR`, and the healthcheck follows.
