#!/usr/bin/env bash
# Local dev build against the working-tree engine (../web-terminal-engine) and
# UI (../web-terminal-ui), before either is published. Shares vendor-tsc.sh /
# assert-emit.sh / css-bundle.sh with the Dockerfile so the two paths can't
# drift — load-bearing here since this path builds against an UNPUBLISHED
# engine, where the wire floors can disagree and a moved module can vanish.
# Produces ./web-terminal-server-bin. Not for CI or release.
# Override sibling checkouts with ENGINE_DIR=... / UI_DIR=...
set -euo pipefail
cd "$(dirname "$0")/.."

ENGINE_DIR="${ENGINE_DIR:-../web-terminal-engine}"
UI_DIR="${UI_DIR:-../web-terminal-ui}"
NM="build/node_modules/@cplieger" # overlay root (gitignored)

# Reads one ARG's value, stopping at whitespace or a `#` comment (a trailing
# `# <name> <version>` trailer follows the digest pins below).
dockerfileArg() {
  sed -n "s/^ARG $1=\\([^[:space:]#]*\\).*/\\1/p" Dockerfile
}

# Validate every input before the destructive rm -rf below, and capture the
# SAME file lists the copy step consumes, so the two can't disagree.
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
# UI ships a nested src tree (src/kernel/, src/features/) since v3.
for f in "${ui_sources[@]}"; do
  mkdir -p "$NM/web-terminal-ui/src/$(dirname "$f")"
  cp "$UI_DIR/src/$f" "$NM/web-terminal-ui/src/$f"
done
# Wire-floor gate reads the engine's manifest from the package root, same file
# the npm tarball carries.
if [ -f "$ENGINE_DIR/web/wire-compatibility.json" ]; then
  cp "$ENGINE_DIR/web/wire-compatibility.json" "$NM/web-terminal-engine/wire-compatibility.json"
fi

# Fetch the pinned native TS7 compiler on demand (typescript@7 ships the
# native Go compiler as `tsc`), matching the Dockerfile's TS_VERSION.
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

# Same source, filenames and digests as the Dockerfile. A silently-missing
# font resolves document.fonts.load() on zero matches and the terminal sizes
# itself against fallback metrics with no error — so a failed fetch is FATAL.
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

# Key the cache on version + a digest of all four pins so a repin with an
# unchanged version still invalidates it.
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
# Replace, not merge, so a face dropped from the list can't survive the embed.
rm -rf static/vendor/fonts
mkdir -p static/vendor/fonts
cp "$FONT_CACHE"/MonaspaceNeonNF-*.woff2 static/vendor/fonts/

echo "[5/6] wire-floor gate (local engine vs the client half it ships)"
# Catches a half-finished wire change here rather than at the browser's first
# connect (close 4002): the Go module resolves through go.work to the local
# checkout while the client half comes from that checkout's own manifest.
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
