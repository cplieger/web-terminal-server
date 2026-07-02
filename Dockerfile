# check=error=true

# --- Builder: Go server + browser bundle (engine + UI compiled with tsgo) ---
FROM debian:trixie-slim@sha256:28de0877c2189802884ccd20f15ee41c203573bd87bb6b883f5f46362d24c5c2 AS builder

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ENV GOTOOLCHAIN=auto

# hadolint ignore=DL3008
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    ca-certificates curl xz-utils && rm -rf /var/lib/apt/lists/*

# Go toolchain for the server binary.
# renovate: datasource=golang-version depName=golang
ARG GO_VERSION=1.26.4
RUN ARCH=$(dpkg --print-architecture) && \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" \
    | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:${PATH}"

# tsgo (TypeScript 7 native preview) compiles the browser TS. Build-time only.
# renovate: datasource=npm depName=@typescript/native-preview
ARG TSGO_VERSION=7.0.0-dev.20260615.1
RUN TSGO_ARCH=$([ "$(dpkg --print-architecture)" = "arm64" ] && echo "arm64" || echo "x64") && \
    curl -fsSL \
      "https://registry.npmjs.org/@typescript/native-preview-linux-${TSGO_ARCH}/-/native-preview-linux-${TSGO_ARCH}-${TSGO_VERSION}.tgz" \
    | tar -xz -C /tmp

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . ./

# Fetch the engine + UI TypeScript from the npm registry (both publish TS
# source only, like @cplieger/reactive). Extracted side by side under one
# node_modules/@cplieger so tsgo's bundler resolution finds the engine when
# compiling the UI's `@cplieger/web-terminal-engine` import.
# renovate: datasource=npm depName=@cplieger/web-terminal-engine
ARG CPLIEGER_WEB_TERMINAL_ENGINE_VERSION=1.4.0
# renovate: datasource=npm depName=@cplieger/web-terminal-ui
ARG CPLIEGER_WEB_TERMINAL_UI_VERSION=2.1.3
RUN mkdir -p node_modules/@cplieger/web-terminal-engine node_modules/@cplieger/web-terminal-ui && \
    curl -fsSL "https://registry.npmjs.org/@cplieger/web-terminal-engine/-/web-terminal-engine-${CPLIEGER_WEB_TERMINAL_ENGINE_VERSION}.tgz" \
      | tar -xz -C node_modules/@cplieger/web-terminal-engine --strip-components=1 && \
    curl -fsSL "https://registry.npmjs.org/@cplieger/web-terminal-ui/-/web-terminal-ui-${CPLIEGER_WEB_TERMINAL_UI_VERSION}.tgz" \
      | tar -xz -C node_modules/@cplieger/web-terminal-ui --strip-components=1

# Compile both packages to static/vendor/. tsgo is a compiler, not a bundler:
# it preserves the UI's bare `@cplieger/web-terminal-engine` import and its relative
# `./*.js` imports, which the served importmap and vendored dirs resolve at
# runtime. The committed static/index.html supplies the scaffold + importmap +
# the inline mount() call, so no app entry needs compiling.
RUN /tmp/package/lib/tsgo \
        --module ESNext --target ESNext --moduleResolution bundler \
        --outDir static/vendor/cplieger-web-terminal-engine \
        --rootDir node_modules/@cplieger/web-terminal-engine/src \
        --skipLibCheck --strict \
        node_modules/@cplieger/web-terminal-engine/src/*.ts && \
    /tmp/package/lib/tsgo \
        --module ESNext --target ESNext --moduleResolution bundler \
        --outDir static/vendor/cplieger-web-terminal-ui \
        --rootDir node_modules/@cplieger/web-terminal-ui/src \
        --skipLibCheck --strict \
        node_modules/@cplieger/web-terminal-ui/src/*.ts

# Concatenate the UI's CSS splits into the served bundle.
RUN set -eu; \
    : > static/style.css; \
    while IFS= read -r line || [ -n "$line" ]; do \
        case "$line" in ''|\#*) continue ;; esac; \
        cat "node_modules/@cplieger/web-terminal-ui/css/${line}" >> static/style.css; \
    done < node_modules/@cplieger/web-terminal-ui/css/MANIFEST

# Nerd Font for the monospace terminal display (box-drawing + icon glyphs that
# system monospace fonts render as tofu).
# renovate: datasource=github-releases depName=ryanoasis/nerd-fonts
ARG NERDFONT_VERSION=v3.4.0
RUN mkdir -p static/vendor/fonts && \
    curl -fsSL "https://github.com/ryanoasis/nerd-fonts/releases/download/${NERDFONT_VERSION}/Monaspace.tar.xz" \
      | tar -xJ -C static/vendor/fonts \
          MonaspiceNeNerdFontMono-Regular.otf \
          MonaspiceNeNerdFontMono-Bold.otf \
          MonaspiceNeNerdFontMono-Italic.otf \
          MonaspiceNeNerdFontMono-BoldItalic.otf

# Build the static binary with assets embedded via go:embed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /web-terminal-server .

# --- Runtime: minimal Debian with a shell for the default WT_CMD ---
FROM debian:trixie-slim@sha256:28de0877c2189802884ccd20f15ee41c203573bd87bb6b883f5f46362d24c5c2

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

HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10s \
    CMD curl -sf -u "${WT_USERNAME:-admin}:${WT_PASSWORD:-}" http://127.0.0.1:7681/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/web-terminal-server"]
