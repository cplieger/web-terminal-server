# check=error=true

# --- Builder: Go server + browser bundle (engine + UI compiled with tsc) ---
FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258 AS builder

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ENV GOTOOLCHAIN=auto

# hadolint ignore=DL3008
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    ca-certificates curl xz-utils && rm -rf /var/lib/apt/lists/*

# Go toolchain for the server binary.
# renovate: datasource=golang-version depName=golang
ARG GO_VERSION=1.26.5
RUN ARCH=$(dpkg --print-architecture) && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 120 --retry 3 --retry-delay 5 "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" \
    | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"

# tsc — the TypeScript 7 native compiler (a Go binary) — compiles the browser
# TS. Build-time only. Since TS7 shipped stable, the native compiler is the
# `typescript` package's per-platform `tsc` (@typescript/typescript-linux-<arch>,
# published in lockstep with the metapackage).
# renovate: datasource=npm depName=typescript
ARG TS_VERSION=7.0.2
RUN TS_ARCH=$([ "$(dpkg --print-architecture)" = "arm64" ] && echo "arm64" || echo "x64") && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 120 --retry 3 --retry-delay 5 \
      "https://registry.npmjs.org/@typescript/typescript-linux-${TS_ARCH}/-/typescript-linux-${TS_ARCH}-${TS_VERSION}.tgz" \
    | tar -xz -C /tmp

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . ./

# Fetch the engine + UI TypeScript from the npm registry (both publish TS
# source only, like @cplieger/reactive). Extracted side by side under one
# node_modules/@cplieger so tsc's bundler resolution finds the engine when
# compiling the UI's `@cplieger/web-terminal-engine` import.
# renovate: datasource=npm depName=@cplieger/web-terminal-engine
ARG CPLIEGER_WEB_TERMINAL_ENGINE_VERSION=3.10.1
# renovate: datasource=npm depName=@cplieger/web-terminal-ui
ARG CPLIEGER_WEB_TERMINAL_UI_VERSION=5.5.0
RUN mkdir -p node_modules/@cplieger/web-terminal-engine node_modules/@cplieger/web-terminal-ui && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 120 --retry 3 --retry-delay 5 "https://registry.npmjs.org/@cplieger/web-terminal-engine/-/web-terminal-engine-${CPLIEGER_WEB_TERMINAL_ENGINE_VERSION}.tgz" \
      | tar -xz -C node_modules/@cplieger/web-terminal-engine --strip-components=1 && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 120 --retry 3 --retry-delay 5 "https://registry.npmjs.org/@cplieger/web-terminal-ui/-/web-terminal-ui-${CPLIEGER_WEB_TERMINAL_UI_VERSION}.tgz" \
      | tar -xz -C node_modules/@cplieger/web-terminal-ui --strip-components=1

# Re-arm the bash SHELL before the bash-only RUNs below. It is already declared
# at the top of this stage and nothing changed it, but hadolint (>=2.15.0) resets
# its shell-dialect tracking to POSIX sh on any ARG or ENV that FOLLOWS a SHELL
# directive -- the Renovate-pinned ARGs above do exactly that -- and then
# shellchecks the rest of the stage as sh, calling this file's bash arrays
# (SC3054) and process substitution (SC3001) undefined. Re-declaring keeps those
# two checks live and real, where suppressing the codes on the instruction would
# switch them off for good. Docker-side this is a no-op: same shell, no layer.
# Drop it when upstream honours the first declaration again.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Compile both packages to static/vendor/. tsc is a compiler, not a bundler:
# it preserves the UI's bare `@cplieger/web-terminal-engine` import and its relative
# `./*.js` imports, which the served importmap and vendored dirs resolve at
# runtime. The committed static/index.html supplies the scaffold + importmap +
# the inline mount() call, so no app entry needs compiling.
RUN mapfile -t ui_ts < <(find node_modules/@cplieger/web-terminal-ui/src -name '*.ts') && \
    /tmp/package/lib/tsc \
        --module ESNext --target ESNext --moduleResolution bundler \
        --outDir static/vendor/cplieger-web-terminal-engine \
        --rootDir node_modules/@cplieger/web-terminal-engine/src \
        --skipLibCheck --strict \
        node_modules/@cplieger/web-terminal-engine/src/*.ts && \
    /tmp/package/lib/tsc \
        --module ESNext --target ESNext --moduleResolution bundler \
        --outDir static/vendor/cplieger-web-terminal-ui \
        --rootDir node_modules/@cplieger/web-terminal-ui/src \
        --skipLibCheck --strict \
        "${ui_ts[@]}"

# Concatenate the UI's CSS splits into the served bundle.
RUN set -eu; \
    : > static/style.css; \
    while IFS= read -r line || [ -n "$line" ]; do \
        case "$line" in ''|\#*) continue ;; esac; \
        cat "node_modules/@cplieger/web-terminal-ui/css/${line}" >> static/style.css; \
    done < node_modules/@cplieger/web-terminal-ui/css/MANIFEST

# Monaspace Neon NF webfonts for the monospace terminal display (box-drawing +
# icon glyphs that system monospace fonts render as tofu). Fetched from
# GitHub's own Monaspace repo, which publishes official nerd-fonts-patched
# WOFF2 webfonts (the nerd-fonts release repo is OTF-only; WOFF2 halves the
# served bytes, and outlines + PUA icon advances are identical to the
# previously bundled MonaspiceNe NFM OTFs — HORIZONTAL metrics only. The
# vertical ones are not: these faces declare 0.945em ascent + 0.200em descent
# where the patched OTFs carried 0.995em + 0.250em, which is shorter than the
# terminal's 17px cell and left a 1px unpainted stripe on every row of
# application background. web-terminal-ui's page.css restores the OTF pair with
# ascent-override/descent-override and pins it against the cell height in its
# own suite; a Monaspace bump that changes those tables again needs that
# override re-measured, not just these sha pins refreshed). sha256 per face:
# raw files at a git tag are as mutable as release assets, so the hashes are
# the integrity anchor.
# renovate: datasource=github-releases depName=githubnext/monaspace
ARG MONASPACE_VERSION=v1.400
# repin: dep=githubnext/monaspace url=https://raw.githubusercontent.com/githubnext/monaspace/{version}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-Regular.woff2
ARG MONASPACE_REGULAR_SHA256=8063ea45b6997c658035a4d876f996ecfa306c88fd0541d35d533fb1f9400c84
# repin: dep=githubnext/monaspace url=https://raw.githubusercontent.com/githubnext/monaspace/{version}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-Bold.woff2
ARG MONASPACE_BOLD_SHA256=45f56dceff8e569d61b6e3168fe208432e7bf0bc3e56e41b4d754cc575a063bd
# repin: dep=githubnext/monaspace url=https://raw.githubusercontent.com/githubnext/monaspace/{version}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-Italic.woff2
ARG MONASPACE_ITALIC_SHA256=3d77eb9a5ec9e32c5ac7ea49c4325e5d6c8e5fefda7317527de905130a88f3cf
# repin: dep=githubnext/monaspace url=https://raw.githubusercontent.com/githubnext/monaspace/{version}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-BoldItalic.woff2
ARG MONASPACE_BOLDITALIC_SHA256=5dffc9465be18eb63263671f1f3ba266ede49043cb6b3edcd65ea993c909b3aa
RUN mkdir -p static/vendor/fonts && \
    for face in Regular Bold Italic BoldItalic; do \
      curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 10 --max-time 120 --retry 3 --retry-delay 5 \
        -o "static/vendor/fonts/MonaspaceNeonNF-${face}.woff2" \
        "https://raw.githubusercontent.com/githubnext/monaspace/${MONASPACE_VERSION}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-${face}.woff2"; \
    done && \
    printf '%s  static/vendor/fonts/MonaspaceNeonNF-Regular.woff2\n%s  static/vendor/fonts/MonaspaceNeonNF-Bold.woff2\n%s  static/vendor/fonts/MonaspaceNeonNF-Italic.woff2\n%s  static/vendor/fonts/MonaspaceNeonNF-BoldItalic.woff2\n' \
      "$MONASPACE_REGULAR_SHA256" "$MONASPACE_BOLD_SHA256" "$MONASPACE_ITALIC_SHA256" "$MONASPACE_BOLDITALIC_SHA256" \
      | sha256sum -c -

# Wire-floor gate (cross-language compatibility): go.mod's engine module and
# the ARG-pinned npm client version move INDEPENDENTLY (Renovate bumps them in
# separate PRs, and a Go-only engine release publishes no npm package), so
# their pairing is governed by the engine's exported wire-compatibility floors,
# not by version strings looking alike. Assert both directional floors at build
# time — a declared-incompatible pairing would refuse every session at first
# connect (close code 4002) while /healthz stays green, so fail HERE instead.
# Client constants come from the vendored artifact's PUBLISHED MANIFEST
# (wire-compatibility.json, a package-root file the engine renders from its own
# TypeScript constants); server constants come from the engine's public Go API
# inside scripts/wirecheck. Neither half is scraped from source. This replaced a
# `sed` extraction of wire-compatibility.ts, which is the practice the engine
# published the manifest to end -- it breaks on any reformat of that line, and a
# reformat is not a wire change, so the gate would have failed for the wrong
# reason. The manifest is decoded by the engine's own terminal.ReadWireManifest,
# so its schema has one home rather than one per consumer.
# BUILT, not `go run`: the gate's exit code is its contract (0 compatible,
# 1 floor violated, 2 the gate itself is broken), and `go run` discards it --
# it prints "exit status 2" and exits 1 itself, collapsing "fix the gate" into
# "bump a pin". Dropping the DL3062 ignore with it: that rule fires on an
# unpinned `go run`/`go install <pkg>`, which is meaningless for a local path,
# and `go build ./scripts/wirecheck` does not trip it. An unneeded ignore
# suppresses a real future warning on this step.
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=tmpfs,target=/tmp/wirecheck-bin \
    WIRE_MANIFEST=node_modules/@cplieger/web-terminal-engine/wire-compatibility.json && \
    test -f "$WIRE_MANIFEST" || { echo "wire-floor-gate: $WIRE_MANIFEST missing from the vendored engine artifact (fix the gate, do not bump a pin)" >&2; exit 2; } && \
    go build -o /tmp/wirecheck-bin/wirecheck ./scripts/wirecheck && \
    /tmp/wirecheck-bin/wirecheck -manifest "$WIRE_MANIFEST"

# Build the static binary with assets embedded via go:embed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /web-terminal-server .

# --- Runtime: minimal Debian with a shell for the default WT_CMD ---
FROM debian:trixie-slim@sha256:3a39a0592364683e6bab97937b72cad5a8fa6dcbbee90edb3bb48c7f8e94f258

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# bash for the default command; curl for the healthcheck; ca-certificates for
# TLS from within the shell. Operators who set a different WT_CMD layer their
# own tools on top.
# hadolint ignore=DL3008
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    bash ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /web-terminal-server /usr/local/bin/web-terminal-server

# In a container the server must listen on all interfaces to be reachable via
# the published port or a reverse proxy (the binary's own default is loopback).
# SECURITY: do not publish this port to an untrusted network without auth — set
# WT_PASSWORD or front it with an authenticating reverse proxy. See README.
ENV WT_ADDR=:7681
ENV WT_CMD=/bin/bash
EXPOSE 7681

# Probe /healthz with credentials via curl's stdin config file (-K -) rather
# than -u, so the password never lands in the container's process args
# (ps / docker inspect). The sed escapes \ and " so a password containing
# either can't break out of the config's quoted value or inject another curl
# directive. Do NOT simplify to -u: that reintroduces both the argv exposure
# and the injection vector.
# DL3025 wants JSON notation, which cannot run this: the probe assembles a curl
# config file on stdin through two command substitutions and a pipe, precisely so
# the password never reaches argv. Exec form supports none of that, and adopting
# it would mean going back to -u and reintroducing the exposure the comment above
# forbids. This image also ships a shell entrypoint, so the distroless case the
# rule guards does not arise here.
# hadolint ignore=DL3025
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10s \
    CMD u=$(printf '%s' "${WT_USERNAME:-admin}" | sed 's/[\\"]/\\&/g'); \
        p=$(printf '%s' "${WT_PASSWORD:-}" | sed 's/[\\"]/\\&/g'); \
        printf 'user = "%s:%s"\n' "$u" "$p" | curl -sf -K - http://127.0.0.1:7681/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/web-terminal-server"]
