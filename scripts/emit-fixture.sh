#!/bin/sh
# Deterministic PTY output fixture for the CDP live-verify harnesses
# (scripts/cdp-*.cjs). Point the server at it so the browser has real,
# predictable output to render and scroll:
#
#   WT_CMD="sh /path/to/scripts/emit-fixture.sh" ./web-terminal-server
#
# It bursts 120 numbered lines (enough to overflow the viewport and accrue
# scrollback), then emits one line every 0.4s forever, so "the read position
# holds while new output arrives" and "reconnect replays the missed lines"
# can be observed live. Ignores its args. Not used in production.
i=1
while [ "$i" -le 120 ]; do
  printf 'emitter line %d -- the quick brown fox jumps over the lazy dog\r\n' "$i"
  i=$((i + 1))
done
while true; do
  printf 'emitter line %d -- the quick brown fox jumps over the lazy dog\r\n' "$i"
  i=$((i + 1))
  sleep 0.4
done
