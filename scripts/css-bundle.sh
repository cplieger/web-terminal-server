#!/usr/bin/env bash
# Concatenate the vendored UI's CSS members, in MANIFEST order, into one bundle.
#
# The recipe was inlined twice before this script existed (Dockerfile and dev-build.sh) in
# the naive `: > out; while read line; cat >> out` shape, which gives up five things this
# does not:
#
#   - Atomic assembly. A member that fails to read halfway through replaced a working
#     bundle with a truncated one; here the temp sibling is only renamed after every member
#     was read.
#   - Containment. Every MANIFEST entry is resolved and required to stay under the css
#     root, so neither a `../` entry nor a symlink in a crafted tarball can pull an
#     arbitrary file into a served asset.
#   - Regular-file guards. `cat` on a FIFO member hangs the build forever, with no deadline
#     in either the image build or a dev build.
#   - A non-empty check per member, and a refusal of an empty or fully-commented MANIFEST,
#     which otherwise yields a zero-byte stylesheet and an unstyled terminal.
#   - Cleanup on interrupt.
#
# Usage: css-bundle.sh <css-root> <out-file>
set -euo pipefail

[ $# -eq 2 ] || {
  printf 'usage: css-bundle.sh <css-root> <out-file>\n' >&2
  exit 2
}
css_root=$1
out=$2

[ -L "$css_root" ] && {
  printf 'ERROR css-root-symlink: %s is a symlink\n' "$css_root" >&2
  exit 1
}
[ -d "$css_root" ] || {
  printf 'ERROR css-root-missing: %s is not a directory\n' "$css_root" >&2
  exit 1
}

manifest="$css_root/MANIFEST"
[ -f "$manifest" ] || {
  printf 'ERROR css-manifest-missing: %s\n' "$manifest" >&2
  exit 1
}

root_real=$(realpath "$css_root")
manifest_real=$(realpath "$manifest")
case "$manifest_real" in
  "$root_real"/*) ;;
  *)
    printf 'ERROR css-manifest-escapes: %s resolves outside %s\n' "$manifest" "$root_real" >&2
    exit 1
    ;;
esac

tmp=$(mktemp "${out}.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM

member_count=0
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    '' | '#'*) continue ;;
  esac
  member="$css_root/$line"
  [ -L "$member" ] && {
    printf 'ERROR css-member-symlink: %s\n' "$member" >&2
    exit 1
  }
  [ -f "$member" ] || {
    printf 'ERROR css-member-missing: %s is not a regular file\n' "$member" >&2
    exit 1
  }
  [ -s "$member" ] || {
    printf 'ERROR css-member-empty: %s\n' "$member" >&2
    exit 1
  }
  member_real=$(realpath "$member")
  case "$member_real" in
    "$root_real"/*) ;;
    *)
      printf 'ERROR css-member-escapes: %s resolves outside %s\n' "$line" "$root_real" >&2
      exit 1
      ;;
  esac
  cat "$member" >>"$tmp"
  printf '\n' >>"$tmp"
  member_count=$((member_count + 1))
done <"$manifest"

if [ "$member_count" -eq 0 ]; then
  printf 'ERROR css-manifest-empty: %s names no members (an empty stylesheet renders an unstyled terminal)\n' "$manifest" >&2
  exit 1
fi

mv "$tmp" "$out"
trap - EXIT HUP INT TERM
printf 'css bundle: %d members -> %s\n' "$member_count" "$out"
