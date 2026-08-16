#!/usr/bin/env bash
# Local dev build of web-terminal-server against the LOCAL working-tree engine
# (../web-terminal-engine) and UI (../web-terminal-ui), before either package is published.
#
# It overlays both TS packages into a build node_modules so tsc can resolve the bare
# specifiers, compiles each to static/vendor/ (tsc preserves bare + relative import
# specifiers, which the served importmap then resolves), asserts the emit, bundles the
# UI's CSS, fetches and verifies the terminal font, runs the wire-floor gate, and
# `go build`s (via go.work) with everything embedded. Produces ./web-terminal-server-bin.
#
# Every step it shares with the image build goes through the same script the Dockerfile
# calls (scripts/vendor-tsc.sh, scripts/assert-emit.sh, scripts/css-bundle.sh), so the two
# paths cannot drift. That matters here more than in the Dockerfile: this is the path that
# builds against an UNPUBLISHED engine, which is exactly where the wire floors can
# disagree and where a moved module can vanish from the emit.
#
# Not for CI or release — the Dockerfile fetches the published packages instead.
# Override the sibling checkouts with ENGINE_DIR=... / UI_DIR=...
set -euo pipefail
cd "$(dirname "$0")/.."

ENGINE_DIR="${ENGINE_DIR:-../web-terminal-engine}"
UI_DIR="${UI_DIR:-../web-terminal-ui}"
NM="build/node_modules/@cplieger" # overlay root (gitignored)

# dockerfileArg reads one ARG's value, stopping at whitespace or a `#` comment. The
# unanchored `s/^ARG X=//p` form this used to carry swallowed any trailing
# `# <name> <version>` trailer, which the digest pins below now have.
dockerfileArg() {
  sed -n "s/^ARG $1=\\([^[:space:]#]*\\).*/\\1/p" Dockerfile
}

# --- preflight: validate every input BEFORE the destructive overlay -----------------
# rm -rf comes next, so a missing input must be reported while the tree is still intact.
# The file lists captured here are the SAME lists the copy step consumes, so preflight and
# execution cannot disagree about what exists.
[ -d "$ENGINE_DIR/web/src" ] || {
  echo "error: need the engine's TS at $ENGINE_DIR/web/src (override with ENGINE_DIR=)" >&2
  exit 1
}
[ -d "$UI_DIR/src" ] || {
  echo "error: need the UI's TS at $UI_DIR/src (override with UI_DIR=)" >&2
  exit 1
}
for f in "$ENGINE_DIR/web/package.json" "$UI_DIR/package.json" "$UI_DIR/css/MANIFEST"; do
  [ -f "$f" ] || {
    echo "error: $f is missing; the checkout is incomplete" >&2
    exit 1
  }
done

mapfile -t engine_sources < <(find "$ENGINE_DIR/web/src" -maxdepth 1 -type f -name '*.ts' \
  ! -name '*.test.ts' ! -name 'fc-strict-setup.ts')
[ "${#engine_sources[@]}" -gt 0 ] || {
  echo "error: engine-src-empty: no .ts files under $ENGINE_DIR/web/src" >&2
  exit 1
}
mapfile -t ui_sources < <(cd "$UI_DIR/src" && find . -type f -name '*.ts' \
  ! -name '*.test.ts' ! -name 'fc-strict-setup.ts')
[ "${#ui_sources[@]}" -gt 0 ] || {
  echo "error: ui-src-empty: no .ts files under $UI_DIR/src" >&2
  exit 1
}

TS_VER="$(dockerfileArg TS_VERSION)"
[ -n "$TS_VER" ] || {
  echo "error: could not read TS_VERSION from Dockerfile" >&2
  exit 1
}
FONT_VER="$(dockerfileArg MONASPACE_VERSION)"
[ -n "$FONT_VER" ] || {
  echo "error: could not read MONASPACE_VERSION from Dockerfile" >&2
  exit 1
}

echo "[1/6] overlay engine + UI TS into $NM"
rm -rf build static/vendor
mkdir -p "$NM/web-terminal-engine/src" "$NM/web-terminal-ui/src"
cp "$ENGINE_DIR/web/package.json" "$NM/web-terminal-engine/package.json"
for f in "${engine_sources[@]}"; do
  cp "$f" "$NM/web-terminal-engine/src/"
done
cp "$UI_DIR/package.json" "$NM/web-terminal-ui/package.json"
# The UI ships a nested src tree (src/kernel/, src/features/) since v3, so preserve
# subdirectories.
for f in "${ui_sources[@]}"; do
  mkdir -p "$NM/web-terminal-ui/src/$(dirname "$f")"
  cp "$UI_DIR/src/$f" "$NM/web-terminal-ui/src/$f"
done
# The wire-floor gate reads the engine's published manifest from the package root, the
# same file the npm tarball carries.
if [ -f "$ENGINE_DIR/web/wire-compatibility.json" ]; then
  cp "$ENGINE_DIR/web/wire-compatibility.json" "$NM/web-terminal-engine/wire-compatibility.json"
fi

# No local package.json here; fetch the pinned native TS7 compiler (tsc) on demand,
# matching the Dockerfile's TS_VERSION so the local bundle reproduces the shipped one
# (`typescript@7` ships the native Go compiler as `tsc`).
TSC_BIN="build/tsc-bin/tsc"
mkdir -p build/tsc-bin
cat >"$TSC_BIN" <<EOF
#!/usr/bin/env bash
exec npx --yes --package "typescript@${TS_VER}" tsc "\$@"
EOF
chmod +x "$TSC_BIN"

echo "[2/6] compile engine + UI -> static/vendor/ (same script the Dockerfile runs)"
bash scripts/vendor-tsc.sh "$TSC_BIN" engine \
  "$NM/web-terminal-engine/src" static/vendor/cplieger-web-terminal-engine
bash scripts/vendor-tsc.sh "$TSC_BIN" ui \
  "$NM/web-terminal-ui/src" static/vendor/cplieger-web-terminal-ui

echo "[3/6] assert every module the page imports was emitted"
bash scripts/assert-emit.sh static/index.html static

echo "[4/6] CSS bundle + verified font"
bash scripts/css-bundle.sh "$UI_DIR/css" static/style.css

# The SAME source, filenames AND digests the Dockerfile uses. A dev build that fetched a
# different family served files the CSS does not reference, so every @font-face 404'd and
# the terminal sized itself against fallback metrics — silently, because
# document.fonts.load() resolves on zero matches. The digests come from the Dockerfile's
# own pins, so the two paths cannot embed different bytes, and a failed fetch is FATAL
# here rather than a warning: a dev build that silently drops the font reproduces the
# fallback-metrics bug this comment exists about.
faces=(Regular Bold Italic BoldItalic)
declare -A face_sha=(
  [Regular]="$(dockerfileArg MONASPACE_REGULAR_SHA256)"
  [Bold]="$(dockerfileArg MONASPACE_BOLD_SHA256)"
  [Italic]="$(dockerfileArg MONASPACE_ITALIC_SHA256)"
  [BoldItalic]="$(dockerfileArg MONASPACE_BOLDITALIC_SHA256)"
)
for face in "${faces[@]}"; do
  [ -n "${face_sha[$face]}" ] || {
    echo "error: font-sha-missing: no MONASPACE_${face^^}_SHA256 in the Dockerfile" >&2
    exit 1
  }
done

# Key the cache on the version AND a digest of all four pins, so a repin with an unchanged
# version still invalidates it, and mark it complete only once every face verified.
pin_key="$(printf '%s\n' "$FONT_VER" "${face_sha[@]}" | sha256sum | cut -c1-16)"
FONT_CACHE="${HOME}/.cache/web-terminal-fonts/${FONT_VER}-${pin_key}"
FONT_BASE="https://raw.githubusercontent.com/githubnext/monaspace/${FONT_VER}/fonts/Web%20Fonts/NerdFonts%20Web%20Fonts/Monaspace%20Neon"
if [ ! -f "$FONT_CACHE/.complete" ]; then
  rm -rf "$FONT_CACHE"
  mkdir -p "$FONT_CACHE"
  for face in "${faces[@]}"; do
    echo "  downloading MonaspaceNeonNF-${face} (${FONT_VER})..."
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL \
      --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
      -o "$FONT_CACHE/MonaspaceNeonNF-${face}.woff2" \
      "${FONT_BASE}/MonaspaceNeonNF-${face}.woff2"
    printf '%s  %s\n' "${face_sha[$face]}" "$FONT_CACHE/MonaspaceNeonNF-${face}.woff2" | sha256sum -c -
  done
  touch "$FONT_CACHE/.complete"
fi
# Replace rather than merge, so a face dropped from the list cannot survive in the embed.
rm -rf static/vendor/fonts
mkdir -p static/vendor/fonts
cp "$FONT_CACHE"/MonaspaceNeonNF-*.woff2 static/vendor/fonts/

echo "[5/6] wire-floor gate (local engine vs the client half it ships)"
# This is the path where the two halves can genuinely disagree: the Go module resolves
# through go.work to the local checkout while the client half comes from that same
# checkout's manifest, so a half-finished wire change is caught here rather than at a
# browser's first connect (close 4002). Built to a temp path and removed on exit,
# including the failure exit that is the gate's whole purpose.
wirecheck_bin="$(mktemp)"
trap 'rm -f "$wirecheck_bin"' EXIT
manifest="$NM/web-terminal-engine/wire-compatibility.json"
if [ -f "$manifest" ]; then
  go build -o "$wirecheck_bin" ./scripts/wirecheck
  "$wirecheck_bin" -manifest "$manifest"
else
  echo "  WARN: $ENGINE_DIR/web/wire-compatibility.json is absent, so the wire floors are unchecked" >&2
fi
rm -f "$wirecheck_bin"
trap - EXIT

echo "[6/6] go build (assets embedded via go:embed)"
CGO_ENABLED=0 go build -trimpath -o web-terminal-server-bin .
echo "OK -> $(pwd)/web-terminal-server-bin ($(du -h web-terminal-server-bin | cut -f1))"
