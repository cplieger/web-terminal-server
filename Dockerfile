# check=error=true

# --- Builder: Go server + browser bundle (engine + UI compiled with tsc) ---
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a AS builder

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ENV GOTOOLCHAIN=auto

# hadolint ignore=DL3008
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    ca-certificates curl xz-utils && rm -rf /var/lib/apt/lists/*

# Go toolchain for the server binary. Digest-verified per arch: TLS authenticates
# go.dev, not the bytes, and this tarball is the largest single input to the image.
# The digests come from go.dev's own release index, so Renovate's custom datasources
# rewrite the version and both shas as one group.
# renovate: datasource=golang-version depName=golang
ARG GO_VERSION=1.27.1
# renovate: datasource=custom.golang-amd64 depName=golang-amd64
ARG GO_SHA256_AMD64=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445  # go1.27.1
# renovate: datasource=custom.golang-arm64 depName=golang-arm64
ARG GO_SHA256_ARM64=3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec  # go1.27.1
RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
      amd64) GO_SHA256="$GO_SHA256_AMD64" ;; \
      arm64) GO_SHA256="$GO_SHA256_ARM64" ;; \
      *) echo "ERROR unsupported-arch: no Go digest pinned for $ARCH" >&2; exit 1 ;; \
    esac && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
      -o /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" && \
    printf '%s  /tmp/go.tar.gz\n' "$GO_SHA256" | sha256sum -c - && \
    tar -C /usr/local -xzf /tmp/go.tar.gz && \
    rm -f /tmp/go.tar.gz
ENV PATH="/usr/local/go/bin:${PATH}"

# tsc — the TypeScript 7 native compiler (a Go binary) — compiles the browser
# TS. Build-time only. Since TS7 shipped stable, the native compiler is the
# `typescript` package's per-platform `tsc` (@typescript/typescript-linux-<arch>,
# published in lockstep with the metapackage). Digest-verified per arch: npm
# publishes SHA-512 in its metadata and nothing recomputes it here, so the pin is
# recomputed by scripts/repin-sha.sh from the marker above each ARG. The arch
# dispatch FAILS on anything unrecognized rather than defaulting to x64, which
# would silently ship the wrong binary.
# renovate: datasource=npm depName=typescript
ARG TS_VERSION=7.0.2
# repin: dep=typescript url=https://registry.npmjs.org/@typescript/typescript-linux-x64/-/typescript-linux-x64-{version}.tgz
ARG TS_SHA256_X64=7ecad6f67377e831856367ab062ef394f21506a611405bf8ac0ff039348637d3
# repin: dep=typescript url=https://registry.npmjs.org/@typescript/typescript-linux-arm64/-/typescript-linux-arm64-{version}.tgz
ARG TS_SHA256_ARM64=c83d931ac9dd7549cde6e71246aa9d6a9812843023df3e277fe3b5dcf41dd0ea
RUN case "$(dpkg --print-architecture)" in \
      amd64) TS_ARCH=x64; TS_SHA256="$TS_SHA256_X64" ;; \
      arm64) TS_ARCH=arm64; TS_SHA256="$TS_SHA256_ARM64" ;; \
      *) echo "ERROR unsupported-arch: no tsc digest pinned for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
      -o /tmp/tsc.tgz "https://registry.npmjs.org/@typescript/typescript-linux-${TS_ARCH}/-/typescript-linux-${TS_ARCH}-${TS_VERSION}.tgz" && \
    printf '%s  /tmp/tsc.tgz\n' "$TS_SHA256" | sha256sum -c - && \
    tar -xzf /tmp/tsc.tgz -C /tmp && \
    rm -f /tmp/tsc.tgz

WORKDIR /build
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . ./

# Fetch the engine + UI TypeScript from the npm registry (both publish TS
# source only, like @cplieger/reactive). Extracted side by side under one
# node_modules/@cplieger so tsc's bundler resolution finds the engine when
# compiling the UI's `@cplieger/web-terminal-engine` import.
#
# Both are digest-verified: these two tarballs ARE the served client bundle, so an
# unverified fetch would let a registry compromise put arbitrary JS into the page
# that drives a remote shell. Downloaded to a file first because a pipe cannot be
# hashed before it is consumed.
# renovate: datasource=npm depName=@cplieger/web-terminal-engine
ARG CPLIEGER_WEB_TERMINAL_ENGINE_VERSION=5.2.0
# repin: dep=@cplieger/web-terminal-engine url=https://registry.npmjs.org/@cplieger/web-terminal-engine/-/web-terminal-engine-{version}.tgz
ARG CPLIEGER_WEB_TERMINAL_ENGINE_SHA256=75245585ffff54f64e89a05f6b1b715dd234fbfc3952768277c01268ff69f129
# renovate: datasource=npm depName=@cplieger/web-terminal-ui
ARG CPLIEGER_WEB_TERMINAL_UI_VERSION=7.3.2
# repin: dep=@cplieger/web-terminal-ui url=https://registry.npmjs.org/@cplieger/web-terminal-ui/-/web-terminal-ui-{version}.tgz
ARG CPLIEGER_WEB_TERMINAL_UI_SHA256=da6099b24f4d22cbcd27f3c6bc76f27b2b7a0e2a26fb23155b3121248b9957ef
RUN mkdir -p node_modules/@cplieger/web-terminal-engine node_modules/@cplieger/web-terminal-ui && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
      -o /tmp/engine.tgz "https://registry.npmjs.org/@cplieger/web-terminal-engine/-/web-terminal-engine-${CPLIEGER_WEB_TERMINAL_ENGINE_VERSION}.tgz" && \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
      -o /tmp/ui.tgz "https://registry.npmjs.org/@cplieger/web-terminal-ui/-/web-terminal-ui-${CPLIEGER_WEB_TERMINAL_UI_VERSION}.tgz" && \
    printf '%s  /tmp/engine.tgz\n%s  /tmp/ui.tgz\n' \
      "$CPLIEGER_WEB_TERMINAL_ENGINE_SHA256" "$CPLIEGER_WEB_TERMINAL_UI_SHA256" | sha256sum -c - && \
    tar -xzf /tmp/engine.tgz -C node_modules/@cplieger/web-terminal-engine --strip-components=1 && \
    tar -xzf /tmp/ui.tgz -C node_modules/@cplieger/web-terminal-ui --strip-components=1 && \
    rm -f /tmp/engine.tgz /tmp/ui.tgz

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

# Compile both packages to static/vendor/, then assert the emit and bundle the CSS.
#
# tsc is a compiler, not a bundler: it preserves the UI's bare
# `@cplieger/web-terminal-engine` import and its relative `./*.js` imports, which the
# served importmap and vendored dirs resolve at runtime. The committed static/index.html
# supplies the scaffold + importmap + the inline mount() call, so no app entry needs
# compiling.
#
# All three steps live in scripts/ so this build and scripts/dev-build.sh cannot drift,
# and so each refuses the failure a shell one-liner swallowed: an empty source list that
# emits nothing and exits 0, a page importing a module tsc never wrote, and a truncated or
# escaping CSS member replacing a working bundle. One RUN because they are one stage of
# the build and hadolint's DL3059 objects to the split.
RUN bash scripts/vendor-tsc.sh /tmp/package/lib/tsc engine \
        node_modules/@cplieger/web-terminal-engine/src static/vendor/cplieger-web-terminal-engine && \
    bash scripts/vendor-tsc.sh /tmp/package/lib/tsc ui \
        node_modules/@cplieger/web-terminal-ui/src static/vendor/cplieger-web-terminal-ui && \
    bash scripts/assert-emit.sh static/index.html static && \
    bash scripts/css-bundle.sh node_modules/@cplieger/web-terminal-ui/css static/style.css

# The terminal's two web fonts, both fetched into static/vendor/fonts/ so they
# land in the //go:embed static tree, and both TAG- or RELEASE-PINNED with a
# sha256 per asset: raw files at a git tag are as mutable as release assets, so
# the hashes are the integrity anchor rather than the ref.
#
# Monaspace Neon NF is the text face (box-drawing + icon glyphs that system
# monospace fonts render as tofu). Fetched from GitHub's own Monaspace repo,
# which publishes official nerd-fonts-patched WOFF2 webfonts (the nerd-fonts
# release repo is OTF-only; WOFF2 halves the served bytes, and outlines + PUA
# icon advances are identical to the previously bundled MonaspiceNe NFM OTFs).
# Its LICENSE travels with the faces: the SIL Open Font License 1.1 requires the
# copyright notice and the licence text to accompany every copy of the font, and
# serving the four woff2 files IS a copy. Every licence file lands under the
# name of the family it covers, because two differently-licensed families share
# this directory and a bare LICENSE beside five woff2 files names neither.
#
# The faces declare 0.945em ascent + 0.200em descent, which is shorter than the
# terminal's 17px cell. That used to leave a 1px unpainted stripe on every row
# of application background, and page.css used to answer it with
# ascent-override/descent-override — it no longer does, because the row
# background is the run padding now, so the cell owes the metrics nothing. What
# a Monaspace bump that moves those tables DOES need is the overlay's copied
# metrics re-measured — and no gate in this image can do that. The cell-contract
# gate below opens no font file, and the overlay's own build is forbidden from
# reading a Monaspace one, so its companion numbers are constants in cell.json
# rather than a measurement. Re-measuring them is a manual step in
# cplieger/web-terminal-glyphs whenever this pin moves.
# renovate: datasource=github-releases depName=githubnext/monaspace
ARG MONASPACE_VERSION=v1.400
# repin: dep=githubnext/monaspace url=https://raw.githubusercontent.com/githubnext/monaspace/{version}/LICENSE dest=MonaspaceNeonNF-LICENSE
ARG MONASPACE_LICENSE_SHA256=0e84e5f7dd6f05e74a00f2fb828ca43e489d954f5509ff0fa439ea18c0d35fe9
# repin: dep=githubnext/monaspace url=https://raw.githubusercontent.com/githubnext/monaspace/{version}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-Regular.woff2
ARG MONASPACE_REGULAR_SHA256=8063ea45b6997c658035a4d876f996ecfa306c88fd0541d35d533fb1f9400c84
# repin: dep=githubnext/monaspace url=https://raw.githubusercontent.com/githubnext/monaspace/{version}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-Bold.woff2
ARG MONASPACE_BOLD_SHA256=45f56dceff8e569d61b6e3168fe208432e7bf0bc3e56e41b4d754cc575a063bd
# repin: dep=githubnext/monaspace url=https://raw.githubusercontent.com/githubnext/monaspace/{version}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-Italic.woff2
ARG MONASPACE_ITALIC_SHA256=3d77eb9a5ec9e32c5ac7ea49c4325e5d6c8e5fefda7317527de905130a88f3cf
# repin: dep=githubnext/monaspace url=https://raw.githubusercontent.com/githubnext/monaspace/{version}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-BoldItalic.woff2
ARG MONASPACE_BOLDITALIC_SHA256=5dffc9465be18eb63263671f1f3ba266ede49043cb6b3edcd65ea993c909b3aa

# cplieger/web-terminal-glyphs is the tiling-glyph overlay: box drawing, block
# elements, shades, braille, Powerline and the Unicode mosaic blocks, drawn for
# Monaspace Neon NF's 1240/2000 em advance at 14px on a 17px row. It carries no
# letters, no digits and no space, which is why 00-tokens.css lists it FIRST in
# --font-mono and the text face behind it keeps its metrics and its look. One
# asset serves four @font-face descriptor sets; the outlines are upright by
# design and must never be slanted or thickened.
#
# LICENSE and NOTICE are served beside the font: this repo is public and the font
# file is redistributed under Apache-2.0, whose section 4 requires both to travel
# with it. cell.json is the released CELL CONTRACT — the companion's advance and
# metrics, and the 14px/17px cell those glyphs are drawn for — and it is fetched
# for the gate below rather than for the browser. The gate reads the CSS-side
# half of it (the stack and the cell numbers); the companion fields describe the
# font FILE, travel under the same release digest as the font, and are asserted
# against it in the overlay repo's own geometry tier. It lands in the same
# directory under the same per-family name so one derivation serves the image and
# scripts/dev-build.sh.
# renovate: datasource=github-releases depName=cplieger/web-terminal-glyphs
ARG WEB_TERMINAL_GLYPHS_VERSION=v1.1.2
# repin: dep=cplieger/web-terminal-glyphs url=https://github.com/cplieger/web-terminal-glyphs/releases/download/{version}/WebTerminalGlyphs.woff2
ARG WEB_TERMINAL_GLYPHS_SHA256=8f4720fa37eed4cdb3ca070d24fbbce85a5266b63c63e754d78a359509aeb94c
# repin: dep=cplieger/web-terminal-glyphs url=https://github.com/cplieger/web-terminal-glyphs/releases/download/{version}/cell.json dest=WebTerminalGlyphs-cell.json
ARG WEB_TERMINAL_GLYPHS_CELL_SHA256=67def948baef97343ab83d6eaeeb0573db598bd63d23982439bc3e1cb8471800
# repin: dep=cplieger/web-terminal-glyphs url=https://github.com/cplieger/web-terminal-glyphs/releases/download/{version}/LICENSE dest=WebTerminalGlyphs-LICENSE
ARG WEB_TERMINAL_GLYPHS_LICENSE_SHA256=c95bae1d1ce0235ecccd3560b772ec1efb97f348a79f0fbe0a634f0c2ccefe2c
# repin: dep=cplieger/web-terminal-glyphs url=https://github.com/cplieger/web-terminal-glyphs/releases/download/{version}/NOTICE dest=WebTerminalGlyphs-NOTICE
ARG WEB_TERMINAL_GLYPHS_NOTICE_SHA256=fceae1c7790ae9ae77e0dd0e4241c342bde487b2f50a4a09576b779c34c7b2a9

# `set -eu` plus a per-iteration `sha256sum -c` is the whole gate: a for-loop's
# exit status is only its LAST iteration's, so verifying after the loop would
# accept every earlier asset unchecked. Each `*)` arm fails the build when an
# asset has no matching sha ARG, so adding one to a list without its pin cannot
# ship unverified bytes.
RUN set -eu; mkdir -p static/vendor/fonts && \
    for face in Regular Bold Italic BoldItalic; do \
      case "$face" in \
        Regular)    face_sha="$MONASPACE_REGULAR_SHA256" ;; \
        Bold)       face_sha="$MONASPACE_BOLD_SHA256" ;; \
        Italic)     face_sha="$MONASPACE_ITALIC_SHA256" ;; \
        BoldItalic) face_sha="$MONASPACE_BOLDITALIC_SHA256" ;; \
        *) echo "ERROR font-sha-missing: no digest pinned for face $face" >&2; exit 1 ;; \
      esac; \
      curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
        -o "static/vendor/fonts/MonaspaceNeonNF-${face}.woff2" \
        "https://raw.githubusercontent.com/githubnext/monaspace/${MONASPACE_VERSION}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon/MonaspaceNeonNF-${face}.woff2"; \
      printf '%s  static/vendor/fonts/MonaspaceNeonNF-%s.woff2\n' "$face_sha" "$face" | sha256sum -c -; \
    done; \
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
      -o static/vendor/fonts/MonaspaceNeonNF-LICENSE \
      "https://raw.githubusercontent.com/githubnext/monaspace/${MONASPACE_VERSION}/LICENSE"; \
    printf '%s  static/vendor/fonts/MonaspaceNeonNF-LICENSE\n' "$MONASPACE_LICENSE_SHA256" | sha256sum -c -; \
    for asset in WebTerminalGlyphs.woff2 cell.json LICENSE NOTICE; do \
      case "$asset" in \
        WebTerminalGlyphs.woff2) asset_sha="$WEB_TERMINAL_GLYPHS_SHA256";         dest=WebTerminalGlyphs.woff2 ;; \
        cell.json)               asset_sha="$WEB_TERMINAL_GLYPHS_CELL_SHA256";    dest=WebTerminalGlyphs-cell.json ;; \
        LICENSE)                 asset_sha="$WEB_TERMINAL_GLYPHS_LICENSE_SHA256"; dest=WebTerminalGlyphs-LICENSE ;; \
        NOTICE)                  asset_sha="$WEB_TERMINAL_GLYPHS_NOTICE_SHA256";  dest=WebTerminalGlyphs-NOTICE ;; \
        *) echo "ERROR font-sha-missing: no digest pinned for glyph asset $asset" >&2; exit 1 ;; \
      esac; \
      curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
        -o "static/vendor/fonts/${dest}" \
        "https://github.com/cplieger/web-terminal-glyphs/releases/download/${WEB_TERMINAL_GLYPHS_VERSION}/${asset}"; \
      printf '%s  static/vendor/fonts/%s\n' "$asset_sha" "$dest" | sha256sum -c -; \
    done

# Content-address the fonts the bundle names, so a face whose bytes change under one
# upstream filename reaches a returning reader on the next load rather than when its
# max-age expires. main.go's staticCacheControl grants `immutable` off the NAME, so
# dropping this step degrades to revalidation instead of to a stale face. index.html is
# rewritten too: its font preload gates the first frame, so a stale href there spends a
# round trip on a 404 instead of starting the fetch the hint exists for.
RUN bash scripts/font-fingerprint.sh static/vendor/fonts static/style.css static/index.html

# Cell-contract gate (the served CSS vs the released contract): the overlay's
# glyphs are drawn for ONE cell — Monaspace's 1240/2000 em advance, its metrics
# copied, at 14px on a 17px row — and are wrong at any other, while the overlay
# pin and the UI pin move in separate Renovate PRs. So the pairing is governed by
# the released cell.json rather than by version strings, and a bump on either
# side that moves the cell must fail HERE: the alternative is a silently
# reflowed terminal, or an emboldened stand-in drawn over box drawing, behind a
# green build and a healthy /healthz. It runs after the fonts AND after the CSS
# bundle, because what it compares is the bundle this build just served plus the
# assets beside it.
#
# It opens NO font file, and that is the design rather than a gap: every asset
# fetched above is pinned by sha256 from the same release as cell.json, so the
# bytes are already fixed, and the font's own agreement with that document is
# asserted in the overlay repo's geometry tier. What only this image can see is
# the CSS a DIFFERENT release built beside the fonts a third pin fetched.
# `fontcheck -h` states the whole chain.
#
# BUILT, not `go run`, for the reason the wire-floor gate below states: the exit
# code is the contract (0 the contract holds, 1 a pin or the CSS has to move,
# 2 the gate itself is broken and no pin should move), and `go run` reports its
# own 1 for any non-zero program exit, collapsing "fix the gate" into "bump a
# pin". The binary goes into a tmpfs mount, so it lands in no layer.
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=tmpfs,target=/tmp/fontcheck-bin \
    go build -o /tmp/fontcheck-bin/fontcheck ./scripts/fontcheck && \
    /tmp/fontcheck-bin/fontcheck -cell static/vendor/fonts/WebTerminalGlyphs-cell.json -css static/style.css -fonts static/vendor/fonts

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

# Build the static binary with assets embedded via go:embed. Both caches are mounted:
# the wirecheck step above fills them, and without the mounts here that work is thrown
# away and the shipped binary's compile redoes the whole module graph every build.
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /web-terminal-server .

COPY scripts/collect-licenses.sh scripts/
RUN --mount=type=cache,target=/root/go/pkg/mod \
    sh scripts/collect-licenses.sh --name web-terminal-server .

# --- Runtime: minimal Debian with a shell for the default SESSION_CMD ---
FROM debian:trixie-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# bash for the default command; curl for the healthcheck; ca-certificates for
# TLS from within the shell. Operators who set a different SESSION_CMD layer
# their own tools on top.
# PKG_REFRESH busts the cache for this layer. Without it BuildKit restores the
# layer verbatim on every rebuild and `apt-get upgrade` never runs again, so the
# image keeps shipping whatever packages were current when the layer was first
# built (measured 2026-08: 11 days stale, with Debian security updates already
# out for util-linux). The central release/CI/scan builds pass today's UTC date.
# The `echo` is load-bearing: BuildKit keys a RUN on the build args it actually
# CONSUMES, so a merely-declared ARG would change nothing.
ARG PKG_REFRESH=static
# Re-declared after the ARG above: hadolint >= 2.15.0 drops a stage's SHELL
# dialect at the next ARG/ENV and shellchecks the rest of the stage as POSIX
# sh. Docker-side a no-op (same shell, no layer); it keeps the SC checks live.
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# hadolint ignore=DL3008
RUN echo "OS package refresh: ${PKG_REFRESH}" \
    && apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
    bash ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /web-terminal-server /usr/local/bin/web-terminal-server
COPY --from=builder /out/usr/share/licenses /usr/share/licenses

# In a container the server must listen on all interfaces to be reachable via
# the published port or a reverse proxy (the binary's own default is loopback).
# SECURITY: do not publish this port to an untrusted network without auth — set
# AUTH_PASSWORD or front it with an authenticating reverse proxy. See README.
ENV LISTEN_ADDR=:7681
ENV SESSION_CMD=/bin/bash
EXPOSE 7681

# Probe /healthz with credentials via curl's stdin config file (-K -) rather
# than -u, so the password never lands in the container's process args
# (ps / docker inspect). The sed escapes \ and " so a password containing
# either can't break out of the config's quoted value or inject another curl
# directive. Do NOT simplify to -u: that reintroduces both the argv exposure
# and the injection vector.
# The port is DERIVED from LISTEN_ADDR rather than hardcoded, so an operator who
# moves the listener keeps a working probe instead of an image that reports
# unhealthy while serving. --max-time 4 sits strictly below --timeout=5s, so a
# slow endpoint is reported by curl's exit code instead of being force-killed
# mid-report by Docker; -S makes the reason visible in `docker inspect`.
# DL3025 wants JSON notation, which cannot run this: the probe assembles a curl
# config file on stdin through two command substitutions and a pipe, precisely so
# the password never reaches argv. Exec form supports none of that, and adopting
# it would mean going back to -u and reintroducing the exposure the comment above
# forbids. This image also ships a shell entrypoint, so the distroless case the
# rule guards does not arise here.
# hadolint ignore=DL3025
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=15s \
    CMD u=$(printf '%s' "${AUTH_USERNAME:-admin}" | sed 's/[\\"]/\\&/g'); \
        p=$(printf '%s' "${AUTH_PASSWORD:-}" | sed 's/[\\"]/\\&/g'); \
        printf 'user = "%s:%s"\n' "$u" "$p" | curl -sfS --max-time 4 -K - "http://127.0.0.1:${LISTEN_ADDR##*:}/healthz" || exit 1

ENTRYPOINT ["/usr/local/bin/web-terminal-server"]
