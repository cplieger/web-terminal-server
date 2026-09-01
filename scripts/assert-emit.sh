#!/usr/bin/env bash
# Asserts every module the page's importmap references was actually emitted.
# The target list is DERIVED from the importmap rather than hardcoded, so a
# library moving a module fails here instead of 404ing in the browser.
#
# Usage: assert-emit.sh <index.html> <static-root>
set -euo pipefail

[ $# -eq 2 ] || {
  printf 'usage: assert-emit.sh <index.html> <static-root>\n' >&2
  exit 2
}
page=$1
root=$2

[ -f "$page" ] || {
  printf 'ERROR page-missing: %s\n' "$page" >&2
  exit 1
}

targets=$(sed -n '/<script type="importmap">/,/<\/script>/p' "$page" | grep -o '"/[^"]*"' | tr -d '"' || true)
if [ -z "$targets" ]; then
  printf 'ERROR importmap-empty: no module paths extracted from %s (the extraction is broken, or the page lost its importmap)\n' "$page" >&2
  exit 1
fi

status=0
for t in $targets; do
  f="$root${t}"
  if [ ! -s "$f" ]; then
    printf 'ERROR tsc-emit-missing: the page imports %s but %s is absent or empty\n' "$t" "$f" >&2
    status=1
  fi
done
exit "$status"
