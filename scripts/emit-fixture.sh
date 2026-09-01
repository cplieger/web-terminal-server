#!/bin/sh
# Deterministic PTY output fixture for the CDP live-verify harnesses:
#   SESSION_CMD="sh /path/to/scripts/emit-fixture.sh" ./web-terminal-server
# Bursts 120 lines then drips one every 0.4s, so scroll-hold and reconnect
# replay can be observed live. Ignores its args. Not used in production.
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
