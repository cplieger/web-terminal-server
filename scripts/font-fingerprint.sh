#!/usr/bin/env bash
# Content-address the web fonts the CSS bundle names: hash each referenced file, rename it
# to <stem>.<8 hex><ext>, and rewrite every /vendor/fonts/ reference in the bundle and in
# each extra file. The bundle is served no-cache, so a new bundle points at the new font
# URL on the next load. The page is an extra file because its font preload gates the first
# frame, so a stale href there costs a round trip on a 404.
#
# Usage: font-fingerprint.sh <fonts-dir> <css-file> [extra-file...]
set -euo pipefail

[ $# -ge 2 ] || {
  printf 'usage: font-fingerprint.sh <fonts-dir> <css-file> [extra-file...]\n' >&2
  exit 2
}
fonts_dir=$1
css=$2
shift 2
targets=("$css" "$@")

# The loop below renames inside this directory, so refuse a symlinked or non-directory
# root before writing anything.
[ -L "$fonts_dir" ] && {
  printf 'ERROR fonts-dir-symlink: %s is a symlink\n' "$fonts_dir" >&2
  exit 1
}
[ -d "$fonts_dir" ] || {
  printf 'ERROR fonts-dir-missing: %s is not a directory\n' "$fonts_dir" >&2
  exit 1
}
fonts_root=$(realpath "$fonts_dir")
for target in "${targets[@]}"; do
  [ -f "$target" ] || {
    printf 'ERROR target-not-regular: %s is not a regular file\n' "$target" >&2
    exit 1
  }
done

# Anchored on the url( or attribute quote that OPENS the reference, because a prose
# mention of the path in a CSS comment is not a face; tolerant of the quoting and
# whitespace inside it, which belong to the published UI's own CSS; and grep -o rather
# than a sed substitution because two url()s can share a line. A reference this misses
# keeps naming a file the rename already moved, which is the one silent failure mode.
font_refs() {
  {
    grep -oE "url\([[:space:]]*[\"']?/vendor/fonts/[^\"')[:space:]]+" "$1" || true
    grep -oE "=[\"']/vendor/fonts/[^\"']+" "$1" || true
  } | sed 's|.*/vendor/fonts/||' | sort -u
}

# Escape the regex metacharacters a filename can legitimately carry, so the rewrite below
# matches the name rather than a pattern.
escape_re() {
  printf '%s' "$1" | sed 's|[.[\*^$/]|\\&|g'
}

names=$(font_refs "$css")
[ -n "$names" ] || {
  printf 'ERROR css-names-no-font: %s names no /vendor/fonts asset\n' "$css" >&2
  exit 1
}

stamped=0
for name in $names; do
  # A URL component reaches a filesystem path here, so refuse anything but a plain
  # filename rather than resolving it.
  case "$name" in
    */* | '' | . | ..)
      printf 'ERROR font-name-is-path: %s names a path rather than a file\n' "$name" >&2
      exit 1
      ;;
  esac
  # A name already carrying a hash is left alone, so a second run over one tree is a
  # no-op rather than stamping a hash onto a hash.
  [[ $name =~ \.[0-9a-f]{8}\.[^.]+$ ]] && continue
  src="$fonts_dir/$name"
  # An asset the fetch did not produce would reach the reader as the SPA fallback under
  # a font URL, so this fails the build rather than skipping.
  [ -f "$src" ] || {
    printf 'ERROR font-asset-missing: %s\n' "$src" >&2
    exit 1
  }
  case "$(realpath "$src")" in
    "$fonts_root"/*) ;;
    *)
      printf 'ERROR font-asset-escapes: %s resolves outside %s\n' "$name" "$fonts_root" >&2
      exit 1
      ;;
  esac
  hash=$(sha256sum "$src" | cut -c1-8)
  [[ $hash =~ ^[0-9a-f]{8}$ ]] || {
    printf 'ERROR font-hash-unusable: sha256sum yielded %q for %s\n' "$hash" "$name" >&2
    exit 1
  }
  ext=""
  stem=$name
  case "$name" in
    *.*)
      ext=".${name##*.}"
      stem="${name%.*}"
      ;;
  esac
  hashed="$stem.$hash$ext"
  mv "$src" "$fonts_dir/$hashed"
  name_re=$(escape_re "$name")
  # The trailing delimiter is what stops a name being rewritten inside a longer one, and
  # it admits whitespace because `url( x )` is legal CSS.
  for target in "${targets[@]}"; do
    tmp=$(mktemp "$target.XXXXXX")
    sed "s|/vendor/fonts/$name_re\([\"')[:space:]]\)|/vendor/fonts/$hashed\1|g" "$target" >"$tmp"
    mv "$tmp" "$target"
  done
  stamped=$((stamped + 1))
done

# An unstamped reference left here would 404 at runtime, so the rewrite must have reached
# every one -- including a page that preloads a face the bundle no longer declares.
for target in "${targets[@]}"; do
  missed=$(font_refs "$target" | grep -Ev '\.[0-9a-f]{8}\.[^.]+$' || true)
  [ -z "$missed" ] || {
    printf 'ERROR reference-unstamped: %s still names %s\n' "$target" "$(printf '%s' "$missed" | tr '\n' ' ')" >&2
    exit 1
  }
done

printf 'font-fingerprint: %s assets content-addressed in %s, %s files rewritten\n' \
  "$stamped" "$fonts_dir" "${#targets[@]}"
