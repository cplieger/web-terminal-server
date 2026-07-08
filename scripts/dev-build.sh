#!/usr/bin/env bash
# Local dev build of web-terminal-server against the LOCAL working-tree engine
# (../web-terminal-engine) and UI (../web-terminal-ui), before either package is published.
#
# It overlays both TS packages into a build node_modules so tsgo can resolve
# the bare specifiers, compiles each to static/vendor/ (tsgo preserves bare +
# relative import specifiers, which the served importmap then resolves),
# concatenates the UI's CSS bundle, fetches the terminal font, and `go build`s
# (via go.work) with everything embedded. Produces ./web-terminal-server-bin.
#
# Not for CI or release — the Dockerfile fetches the published packages instead.
# Override the sibling checkouts with ENGINE_DIR=... / UI_DIR=...
set -euo pipefail
cd "$(dirname "$0")/.."

ENGINE_DIR="${ENGINE_DIR:-../web-terminal-engine}"
UI_DIR="${UI_DIR:-../web-terminal-ui}"
NM="build/node_modules/@cplieger" # overlay root (gitignored)

if [ ! -d "$ENGINE_DIR/web/src" ] || [ ! -d "$UI_DIR/src" ]; then
  echo "error: need engine at $ENGINE_DIR/web/src and UI at $UI_DIR/src" >&2
  exit 1
fi

echo "[1/5] overlay engine + UI TS into $NM"
rm -rf build static/vendor
mkdir -p "$NM/web-terminal-engine/src" "$NM/web-terminal-ui/src"
cp "$ENGINE_DIR/web/package.json" "$NM/web-terminal-engine/package.json"
for f in "$ENGINE_DIR"/web/src/*.ts; do
  case "$f" in *.test.ts | *fc-strict-setup*) continue ;; esac
  cp "$f" "$NM/web-terminal-engine/src/"
done
cp "$UI_DIR/package.json" "$NM/web-terminal-ui/package.json"
# The UI ships a nested src tree (src/kernel/, src/features/) since v3, so copy
# recursively, preserving subdirectories and excluding tests.
(cd "$UI_DIR/src" && find . -name '*.ts' ! -name '*.test.ts' ! -name 'fc-strict-setup.ts' -print0) \
  | while IFS= read -r -d '' f; do
    mkdir -p "$NM/web-terminal-ui/src/$(dirname "$f")"
    cp "$UI_DIR/src/$f" "$NM/web-terminal-ui/src/$f"
  done

TSGO_VER="$(sed -n 's/^ARG TSGO_VERSION=//p' Dockerfile)"
if [ -n "$TSGO_VER" ] && ! tsgo --version 2>/dev/null | grep -qF "$TSGO_VER"; then
  echo "  WARN: local tsgo != Dockerfile pin ($TSGO_VER); CDP harnesses may validate a bundle the shipped image will not reproduce" >&2
fi

echo "[2/5] compile engine -> static/vendor/cplieger-web-terminal-engine"
tsgo --module ESNext --target ESNext --moduleResolution bundler \
  --outDir static/vendor/cplieger-web-terminal-engine \
  --rootDir "$NM/web-terminal-engine/src" --skipLibCheck --strict \
  "$NM/web-terminal-engine/src"/*.ts

echo "[3/5] compile UI -> static/vendor/cplieger-web-terminal-ui"
# The UI files sit in the overlay node_modules, so tsgo's bundler resolution
# walks up and finds the sibling @cplieger/web-terminal-engine package for the bare
# import; the emitted JS keeps that specifier for the runtime importmap.
# Compile the whole nested src tree (index.ts + presets.ts + kernel/ + features/);
# find collects every non-test .ts (the overlay already excluded tests).
mapfile -t ui_ts < <(find "$NM/web-terminal-ui/src" -name '*.ts')
tsgo --module ESNext --target ESNext --moduleResolution bundler \
  --outDir static/vendor/cplieger-web-terminal-ui \
  --rootDir "$NM/web-terminal-ui/src" --skipLibCheck --strict \
  "${ui_ts[@]}"

echo "[4/5] CSS bundle + font"
: >static/style.css
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in '' | \#*) continue ;; esac
  cat "$UI_DIR/css/${line}" >>static/style.css
done <"$UI_DIR/css/MANIFEST"

FONT_VER="$(sed -n 's/^ARG NERDFONT_VERSION=//p' Dockerfile)"
[ -n "$FONT_VER" ] || {
  echo "error: could not read NERDFONT_VERSION from Dockerfile" >&2
  exit 1
}
# Cache per font version so a NERDFONT_VERSION bump forces a fresh
# download instead of copying a stale cached .otf into the build.
FONT_CACHE="${HOME}/.cache/web-terminal-fonts/${FONT_VER}"
fonts=(
  MonaspiceNeNerdFontMono-Regular.otf
  MonaspiceNeNerdFontMono-Bold.otf
  MonaspiceNeNerdFontMono-Italic.otf
  MonaspiceNeNerdFontMono-BoldItalic.otf
)
mkdir -p "$FONT_CACHE" static/vendor/fonts
# Re-download if ANY of the four .otf files is missing so a partial cache left
# by an interrupted fetch self-heals instead of embedding an incomplete font.
need_fonts=
for font in "${fonts[@]}"; do
  [ -f "$FONT_CACHE/$font" ] || need_fonts=1
done
if [ -n "$need_fonts" ]; then
  echo "  downloading Monaspace ${FONT_VER}..."
  curl -fsSL --connect-timeout 10 --max-time 60 --retry 2 \
    "https://github.com/ryanoasis/nerd-fonts/releases/download/${FONT_VER}/Monaspace.tar.xz" \
    | tar -xJ -C "$FONT_CACHE" "${fonts[@]}" \
    || echo "  WARN: font fetch failed (display will use a fallback font)"
fi
cp "$FONT_CACHE"/MonaspiceNeNerdFontMono-*.otf static/vendor/fonts/ 2>/dev/null || true

echo "[5/5] go build (assets embedded via go:embed)"
CGO_ENABLED=0 go build -trimpath -o web-terminal-server-bin .
echo "OK -> $(pwd)/web-terminal-server-bin ($(du -h web-terminal-server-bin | cut -f1))"
