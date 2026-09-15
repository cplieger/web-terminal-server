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

# fetch_pinned <dep> <version-arg> <cache-name> <dest-dir>
#
# Downloads and verifies every asset the Dockerfile pins for one `# repin:`
# dep, from the markers themselves rather than from a second copy of the list:
# the URL template, the destination name and the sha ARG all come off the same
# two lines Renovate's postUpgradeTask rewrites, so this cannot drift from what
# the image fetches. A marker's `dest=` token names the destination file (it is
# how two projects both shipping a file called LICENSE are disambiguated);
# absent, the URL's own basename is used.
#
# Keying on the markers is what makes the asset list unable to drift from the
# pins: an asset with no marker has no sha256 ARG at all, so it could never be
# verified, and the image build refuses it on the case statement's `*)` arm.
fetch_pinned() {
  local dep=$1 version_arg=$2 cache_name=$3 dest_dir=$4
  local version
  version=$(dockerfileArg "$version_arg")
  [ -n "$version" ] || {
    echo "error: could not read ARG $version_arg from Dockerfile" >&2
    exit 1
  }

  # One "<dest> <url-template> <sha-arg>" row per marker for this dep. The ARG
  # must sit on the line immediately below its marker, which is the pairing
  # repin-sha.sh relies on too.
  local rows
  rows=$(awk -v dep="$dep" '
    /^#[[:space:]]*repin:/ {
      url = ""; dest = ""; d = ""
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^dep=/)  { d = substr($i, 5) }
        if ($i ~ /^url=/)  { url = substr($i, 5) }
        if ($i ~ /^dest=/) { dest = substr($i, 6) }
      }
      if (d == dep && url != "") {
        pending_url = url; pending_dest = dest
      }
      next
    }
    pending_url != "" {
      if ($0 ~ /^ARG [A-Za-z_][A-Za-z0-9_]*=/) {
        name = $0; sub(/^ARG /, "", name); sub(/=.*/, "", name)
        if (pending_dest == "") { pending_dest = pending_url; sub(/^.*\//, "", pending_dest) }
        printf "%s %s %s\n", pending_dest, pending_url, name
      }
      pending_url = ""
      next
    }
  ' Dockerfile)
  [ -n "$rows" ] || {
    echo "error: no '# repin: dep=$dep' marker in Dockerfile" >&2
    exit 1
  }

  # Cache key: the version AND a digest of every pin, so a repin at an
  # unchanged version still misses the cache. The `.complete` marker is written
  # only after every asset downloaded AND verified, so an interrupted fetch
  # self-heals with a full retry instead of embedding a partial asset.
  local dests=() urls=() shas=() combined=""
  local dest url_tmpl sha_arg sha
  while read -r dest url_tmpl sha_arg; do
    [ -n "$dest" ] || continue
    sha=$(dockerfileArg "$sha_arg")
    [ -n "$sha" ] || {
      echo "error: could not read ARG $sha_arg from Dockerfile" >&2
      exit 1
    }
    dests+=("$dest")
    urls+=("${url_tmpl//\{version\}/$version}")
    shas+=("$sha")
    combined="${combined}${sha}"
  done <<<"$rows"

  local key cache
  key=$(printf '%s\n%s' "$version" "$combined" | sha256sum | cut -c1-16)
  cache="${HOME}/.cache/${cache_name}/${version}-${key}"

  # Re-verify the whole cache before reusing it. `.complete` records that a
  # download verified ONCE, which is a different claim from "these bytes are
  # still the pinned ones": a cache entry can change after the marker is written
  # (interrupted external tooling, disk corruption, another process under the
  # same uid), and non-empty is no evidence at all — the WRONG font is non-empty.
  # A mismatch discards the whole keyed directory rather than repairing one
  # entry, because whatever changed one is not known to have stopped at one. The
  # image build verifies every byte it fetches, so a dev build trusting a warm
  # cache would be the single path that embeds an unverified font in the
  # //go:embed static tree.
  local need=0 i
  [ -f "$cache/.complete" ] || need=1
  if [ "$need" = 0 ]; then
    for i in "${!dests[@]}"; do
      [ -s "$cache/${dests[$i]}" ] || {
        need=1
        break
      }
      printf '%s  %s\n' "${shas[$i]}" "$cache/${dests[$i]}" | sha256sum -c --status - || {
        printf '  cached %s %s no longer matches its pin (%s); discarding the cache\n' \
          "$dep" "$version" "${dests[$i]}" >&2
        need=1
        break
      }
    done
  fi
  if [ "$need" = 1 ]; then
    echo "  downloading $dep $version (${#dests[@]} assets)..."
    rm -rf "$cache"
    mkdir -p "$cache"
    for i in "${!dests[@]}"; do
      curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL \
        --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
        -o "$cache/${dests[$i]}" "${urls[$i]}"
      printf '%s  %s\n' "${shas[$i]}" "$cache/${dests[$i]}" | sha256sum -c -
    done
    : >"$cache/.complete"
  fi

  # Copy rather than link, and let the caller own the destination's lifetime: a
  # dev build reuses the working tree, so an asset dropped from the Dockerfile
  # must not survive there and keep landing in the //go:embed static tree.
  mkdir -p "$dest_dir"
  for i in "${!dests[@]}"; do
    cp "$cache/${dests[$i]}" "$dest_dir/${dests[$i]}"
    # Verify what was actually COPIED, not what passed a check a moment ago: the
    # reuse check above and this copy are two operations on a path anything
    # sharing the uid can rewrite in between, and only the destination's bytes
    # reach the embedded tree.
    printf '%s  %s\n' "${shas[$i]}" "$dest_dir/${dests[$i]}" | sha256sum -c --quiet -
  done
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

echo "[1/7] overlay engine + UI TS into $NM"
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

echo "[2/7] compile engine + UI -> static/vendor/ (same script the Dockerfile runs)"
bash scripts/vendor-tsc.sh "$TSC_BIN" engine \
  "$NM/web-terminal-engine/src" static/vendor/cplieger-web-terminal-engine
bash scripts/vendor-tsc.sh "$TSC_BIN" ui \
  "$NM/web-terminal-ui/src" static/vendor/cplieger-web-terminal-ui

echo "[3/7] assert every module the page imports was emitted"
bash scripts/assert-emit.sh static/index.html static

echo "[4/7] CSS bundle + verified fonts"
bash scripts/css-bundle.sh "$UI_DIR/css" static/style.css

# Same sources, filenames and digests as the Dockerfile, derived from its own
# `# repin:` markers (fetch_pinned above). A silently-missing font resolves
# document.fonts.load() on zero matches and the terminal sizes itself against
# fallback metrics with no error — so a failed fetch is FATAL.
#
# Replace, not merge: a dev build reuses the working tree and `cp` cannot delete
# what the marker list no longer names, so a dropped face would keep landing in
# the //go:embed static tree. The per-dep caches stay.
rm -rf static/vendor/fonts
fetch_pinned githubnext/monaspace MONASPACE_VERSION \
  web-terminal-fonts static/vendor/fonts
fetch_pinned cplieger/web-terminal-glyphs WEB_TERMINAL_GLYPHS_VERSION \
  web-terminal-glyphs static/vendor/fonts

echo "[5/7] cell-contract gate (the released cell contract vs the CSS this build serves)"
# Mirrors the Dockerfile step: the overlay's glyphs are drawn for one cell, and
# here the CSS comes from a LOCAL UI checkout, which is exactly where it can
# move ahead of the released contract. Built and then invoked, never `go run`,
# which collapses the gate's exit 2 ("the gate is broken, do NOT bump a pin")
# into a plain 1.
fontcheck_bin="$(mktemp)"
trap 'rm -f "$fontcheck_bin"' EXIT
go build -o "$fontcheck_bin" ./scripts/fontcheck
"$fontcheck_bin" \
  -cell static/vendor/fonts/WebTerminalGlyphs-cell.json \
  -css static/style.css \
  -fonts static/vendor/fonts
rm -f "$fontcheck_bin"
trap - EXIT

echo "[6/7] wire-floor gate (local engine vs the client half it ships)"
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

echo "[7/7] go build (assets embedded via go:embed)"
CGO_ENABLED=0 go build -trimpath -o web-terminal-server-bin .
echo "OK -> $(pwd)/web-terminal-server-bin ($(du -h web-terminal-server-bin | cut -f1))"
