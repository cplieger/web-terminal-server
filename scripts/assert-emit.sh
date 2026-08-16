#!/usr/bin/env bash
# Assert every module the served page imports was emitted, non-empty.
#
# The target list is DERIVED from the page's own importmap rather than hardcoded, which is
# the whole point: a hardcoded list goes stale the moment a library moves a module, and the
# failure is invisible until a browser resolves the import and 404s. This repo's page maps
# three specifiers today, one of which (`.../presets.js`) exists only because the UI keeps
# its presets at `src/presets.ts` right now.
#
# There is no /app.js to check: this page mounts the terminal from an inline module script,
# so every emitted target comes from the importmap.
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
