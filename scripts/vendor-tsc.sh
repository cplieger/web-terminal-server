#!/usr/bin/env bash
# Compile one vendored @cplieger package's TypeScript to served JS.
# `find -type f` (not a bare `-name`) so a symlink/FIFO named `*.ts` in a
# crafted tarball can't reach tsc or hang the build. Refuses an empty source
# list, since a mis-published tarball would otherwise emit nothing and exit 0.
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
