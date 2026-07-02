#!/bin/sh
# Deterministic fixture for the ED3 (CSI 3 J, "erase saved lines") end-to-end
# check driven by scripts/cdp-scrollback.cjs. It bursts a large scrollback,
# prints a PRE marker, then BLOCKS on stdin. The client triggers the clear by
# sending a newline once it has settled — so the ED3 is client-timed, not on a
# fixed clock the observer would race. On the trigger it emits ED3 (clear saved
# lines) + ED2/home (clear the visible screen) + a POST marker, then idles.
# Single deterministic pass. Not production.
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
