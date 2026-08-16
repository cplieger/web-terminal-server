#!/usr/bin/env bash
# Compile one vendored @cplieger package's TypeScript to served JS.
#
# The single home for the tsc invocation. The recipe was spelled four times before this
# script existed (twice in the Dockerfile, twice in dev-build.sh) and the copies had
# already drifted: two used a flat `src/*.ts` glob, which silently drops any nested
# module, and none refused an empty source list, so a mis-published tarball produced an
# empty output directory and a green build whose page fails in the module resolver.
#
# `find -type f` is deliberate rather than a bare `-name`: a directory, symlink or FIFO
# named `*.ts` in a crafted or mis-published tarball otherwise reaches tsc, and a FIFO
# hangs the build with no deadline.
#
# Usage: vendor-tsc.sh <tsc-binary> <label> <src-dir> <out-dir>
set -euo pipefail

[ $# -eq 4 ] || {
  printf 'usage: vendor-tsc.sh <tsc-binary> <label> <src-dir> <out-dir>\n' >&2
  exit 2
}
tsc=$1
label=$2
src=$3
out=$4

[ -x "$tsc" ] || {
  printf 'ERROR %s-tsc-missing: %s is not executable\n' "$label" "$tsc" >&2
  exit 1
}
[ -d "$src" ] || {
  printf 'ERROR %s-src-missing: %s is not a directory\n' "$label" "$src" >&2
  exit 1
}

mapfile -t sources < <(find "$src" -type f -name '*.ts')
if [ "${#sources[@]}" -eq 0 ]; then
  printf 'ERROR %s-src-empty: no .ts files under %s (a mis-published tarball would emit nothing and still exit 0)\n' "$label" "$src" >&2
  exit 1
fi

"$tsc" \
  --module ESNext \
  --target ESNext \
  --moduleResolution bundler \
  --outDir "$out" \
  --rootDir "$src" \
  --skipLibCheck \
  --strict \
  "${sources[@]}"
