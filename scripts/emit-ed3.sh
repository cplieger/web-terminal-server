#!/bin/sh
# Deterministic ED3 (CSI 3 J) fixture for scripts/cdp-scrollback.cjs: bursts
# scrollback, emits a PRE marker, then blocks on stdin so the client controls
# when the clear fires (client-timed, not a race against a fixed clock). Not
# production.
i=1
while [ "$i" -le 200 ]; do
  printf 'scrollback line %d\r\n' "$i"
  i=$((i + 1))
done
printf 'PRE-ED3-MARKER\r\n'
# Block until the client sends a line; that read is the deterministic trigger.
read -r _trigger
printf '\033[3J\033[2J\033[HPOST-ED3-MARKER\r\n'
while true; do
  sleep 3600
done
