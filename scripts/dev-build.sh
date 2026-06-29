#!/usr/bin/env bash
# Local dev build of web-terminal-server against the LOCAL working-tree engine
# (../vterm) and UI (../web-terminal-ui), before either package is published.
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

ENGINE_DIR="${ENGINE_DIR:-../vterm}"
UI_DIR="${UI_DIR:-../web-terminal-ui}"
NM="build/node_modules/@cplieger" # overlay root (gitignored)

if [ ! -d "$ENGINE_DIR/web/src" ] || [ ! -d "$UI_DIR/src" ]; then
  echo "error: need engine at $ENGINE_DIR/web/src and UI at $UI_DIR/src" >&2
  exit 1
fi

echo "[1/5] overlay engine + UI TS into $NM"
rm -rf build static/vendor
mkdir -p "$NM/web-terminal/src" "$NM/web-terminal-ui/src"
cp "$ENGINE_DIR/web/package.json" "$NM/web-terminal/package.json"
for f in "$ENGINE_DIR"/web/src/*.ts; do
  case "$f" in *.test.ts | *fuzz* | *fc-strict-setup*) continue ;; esac
  cp "$f" "$NM/web-terminal/src/"
done
cp "$UI_DIR/package.json" "$NM/web-terminal-ui/package.json"
for f in "$UI_DIR"/src/*.ts; do
  case "$f" in *.test.ts | *fc-strict-setup*) continue ;; esac
  cp "$f" "$NM/web-terminal-ui/src/"
done

echo "[2/5] compile engine -> static/vendor/cplieger-web-terminal"
tsgo --module ESNext --target ESNext --moduleResolution bundler \
  --outDir static/vendor/cplieger-web-terminal \
  --rootDir "$NM/web-terminal/src" --skipLibCheck --strict \
  "$NM/web-terminal/src"/*.ts

echo "[3/5] compile UI -> static/vendor/cplieger-web-terminal-ui"
# The UI files sit in the overlay node_modules, so tsgo's bundler resolution
# walks up and finds the sibling @cplieger/web-terminal package for the bare
# import; the emitted JS keeps that specifier for the runtime importmap.
tsgo --module ESNext --target ESNext --moduleResolution bundler \
  --outDir static/vendor/cplieger-web-terminal-ui \
  --rootDir "$NM/web-terminal-ui/src" --skipLibCheck --strict \
  "$NM/web-terminal-ui/src"/*.ts

echo "[4/5] CSS bundle + font"
: >static/style.css
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in '' | \#*) continue ;; esac
  cat "$UI_DIR/css/${line}" >>static/style.css
done <"$UI_DIR/css/MANIFEST"

FONT_CACHE="${HOME}/.cache/web-terminal-fonts"
FONT_VER="v3.4.0"
mkdir -p "$FONT_CACHE" static/vendor/fonts
if [ ! -f "$FONT_CACHE/MonaspiceNeNerdFontMono-Regular.otf" ]; then
  echo "  downloading Monaspace ${FONT_VER}..."
  curl -fsSL --connect-timeout 10 --max-time 60 --retry 2 \
    "https://github.com/ryanoasis/nerd-fonts/releases/download/${FONT_VER}/Monaspace.tar.xz" \
    | tar -xJ -C "$FONT_CACHE" \
      MonaspiceNeNerdFontMono-Regular.otf \
      MonaspiceNeNerdFontMono-Bold.otf \
      MonaspiceNeNerdFontMono-Italic.otf \
      MonaspiceNeNerdFontMono-BoldItalic.otf || echo "  WARN: font fetch failed (display will use a fallback font)"
fi
cp "$FONT_CACHE"/MonaspiceNeNerdFontMono-*.otf static/vendor/fonts/ 2>/dev/null || true

echo "[5/5] go build (assets embedded via go:embed)"
CGO_ENABLED=0 go build -trimpath -o web-terminal-server-bin .
echo "OK -> $(pwd)/web-terminal-server-bin ($(du -h web-terminal-server-bin | cut -f1))"
