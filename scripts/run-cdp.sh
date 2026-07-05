#!/usr/bin/env bash
# Run the CDP live-verify suite end to end and aggregate a single pass/fail.
# Not human-reliant: each harness asserts and exits 0/non-zero, and this runner
# returns non-zero if any harness fails.
#
# It provisions everything locally: a headless Chromium (a real one on PATH, or
# the Playwright-cached build) exposing the DevTools Protocol, and a
# web-terminal-server bound to loopback running the deterministic fixture. Then
# it runs each harness and tears the lot down.
#
# Overrides:
#   CDP_URL   use an existing DevTools endpoint instead of spawning Chromium
#             (e.g. a shared sidecar: CDP_URL=http://192.0.2.77:9222). When
#             set, the server must be reachable from THAT browser, so WT_URL
#             must also be set and this runner will not manage the server.
#   WT_BIN    path to a built server binary (default ./web-terminal-server-bin,
#             built by scripts/dev-build.sh so static/vendor is populated).
#   CHROMIUM  explicit Chromium/Chrome binary to launch.
#   CDP_PORT  DevTools port to spawn on (default 9222).
#   WT_PORT   server port (default 7681).
set -u

DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$(dirname "$DIR")"
CDP_PORT="${CDP_PORT:-9222}"
WT_PORT="${WT_PORT:-7681}"
WT_HOST="127.0.0.1"

CHROME_PID=""
SERVER_PID=""
CHROME_PROFILE=""

cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  [ -n "$CHROME_PID" ] && kill "$CHROME_PID" 2>/dev/null
  [ -n "$CHROME_PROFILE" ] && rm -rf "$CHROME_PROFILE" 2>/dev/null
  return 0
}
trap cleanup EXIT INT TERM

find_chromium() {
  if [ -n "${CHROMIUM:-}" ]; then printf '%s\n' "$CHROMIUM"; return 0; fi
  for b in chromium chromium-browser google-chrome google-chrome-stable chrome; do
    if command -v "$b" >/dev/null 2>&1; then command -v "$b"; return 0; fi
  done
  # Playwright cache (headless shell first, then full chromium).
  for p in \
    "$HOME"/.cache/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-linux*/chrome-headless-shell \
    "$HOME"/.cache/ms-playwright/chromium-*/chrome-linux*/chrome; do
    for f in $p; do [ -x "$f" ] && { printf '%s\n' "$f"; return 0; }; done
  done
  return 1
}

wait_for() { # url label
  for _ in $(seq 1 50); do
    curl -fsS "$1" >/dev/null 2>&1 && return 0
    sleep 0.3
  done
  echo "timed out waiting for $2 ($1)" >&2
  return 1
}

start_chromium() {
  local bin base
  local -a flag=()
  bin="$(find_chromium)" || { echo "no Chromium found; install one or set CHROMIUM= or CDP_URL=" >&2; exit 3; }
  base="$(basename "$bin")"
  # chrome-headless-shell is always headless; full chrome needs --headless=new.
  case "$base" in
    *headless*) flag=() ;;
    *) flag=(--headless=new) ;;
  esac
  CHROME_PROFILE="$(mktemp -d)"
  echo "launching $bin on :$CDP_PORT"
  "$bin" "${flag[@]}" --disable-gpu --no-first-run --no-default-browser-check \
    --remote-debugging-port="$CDP_PORT" --remote-allow-origins='*' \
    --user-data-dir="$CHROME_PROFILE" about:blank >/dev/null 2>&1 &
  CHROME_PID=$!
  wait_for "http://$WT_HOST:$CDP_PORT/json/version" "Chromium DevTools"
}

start_server() { # wt_cmd
  local bin="${WT_BIN:-$REPO/web-terminal-server-bin}"
  if [ ! -x "$bin" ]; then
    echo "server binary $bin not found; building with dev-build.sh" >&2
    ( cd "$REPO" && bash scripts/dev-build.sh ) || { echo "dev-build.sh failed" >&2; exit 4; }
  fi
  WT_ADDR="$WT_HOST:$WT_PORT" WT_CMD="$1" WT_SCROLLBACK=5000 "$bin" >/dev/null 2>&1 &
  SERVER_PID=$!
  wait_for "http://$WT_HOST:$WT_PORT/healthz" "web-terminal-server"
}

stop_server() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null
  wait "$SERVER_PID" 2>/dev/null
  SERVER_PID=""
}

FAILED=""
run() { # label script
  echo ""
  echo "########## $1 ##########"
  if node "$DIR/$2"; then
    echo ">>> $1: PASS"
  else
    echo ">>> $1: FAIL"
    FAILED="$FAILED $1"
  fi
}

# Group 1: harnesses that run against the continuous-emitter fixture.
# Listed once so a new harness can't be silently dropped from either branch.
GROUP1=(render resume input viewport resize)
run_group1() { for h in "${GROUP1[@]}"; do run "$h" "cdp-$h.cjs"; done; }

# --- provision the DevTools endpoint ---
command -v node >/dev/null 2>&1 || { echo "node (v22+) is required to run the CDP harnesses" >&2; exit 3; }
if [ -n "${CDP_URL:-}" ]; then
  if [ -z "${WT_URL:-}" ]; then
    echo "CDP_URL is set but WT_URL is not: with an external browser this runner cannot" >&2
    echo "manage the server (it must be reachable from that browser). Set WT_URL too, or" >&2
    echo "unset CDP_URL to run fully local." >&2
    exit 2
  fi
  echo "using external DevTools endpoint $CDP_URL and server $WT_URL (not managing either)"
  export CDP_URL WT_URL
  run_group1
  echo "NOTE: skipping the scrollback test (needs the server on emit-ed3.sh; start it and" >&2
  echo "      run: WT_URL=... node scripts/cdp-scrollback.cjs)" >&2
else
  command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 3; }
  start_chromium
  export CDP_URL="http://$WT_HOST:$CDP_PORT"
  export WT_URL="http://$WT_HOST:$WT_PORT/"

  # Group 1: the continuous emitter.
  start_server "sh $DIR/emit-fixture.sh"
  run_group1
  stop_server

  # Group 2: the ED3 fixture.
  start_server "sh $DIR/emit-ed3.sh"
  run "scrollback" cdp-scrollback.cjs
  stop_server
fi

echo ""
echo "==================== SUMMARY ===================="
if [ -n "$FAILED" ]; then
  echo "FAILED:$FAILED"
  exit 1
fi
echo "ALL CDP HARNESSES PASSED"
