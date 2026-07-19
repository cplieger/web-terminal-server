// Live-verify: an app-issued ED3 (CSI 3 J, "erase saved lines") propagates the
// scrollbackCleared signal to clients end to end. This is the generic form of
// the old cdp-rotate2 diagnostic (which detected it via a specific app's
// repeated banner).
//
// This is a WIRE-level check, not a browser/DOM one, and it is deliberately
// CLIENT-TRIGGERED for determinism: a real browser's async load (font metrics,
// its own resize, the resume handshake) races an app that emits ED3 on a fixed
// timer, so observing the transient through the DOM is inherently flaky. Here
// the client connects, lets the fixture's burst settle, and only THEN sends a
// newline that the fixture is blocked reading — so the ED3 fires under the
// client's control, with no timer race. It then asserts the server delivered a
// screen frame carrying the scrollbackCleared flag with a non-zero base.
//
// The client-side effect (dropping history when the bit arrives) is unit-tested
// in the engine's store.test.ts; the wire decode in wire-v2.test.ts. This pins
// the remaining link: the server's ED3 -> wire propagation through a live PTY.
//
// Drive it against the ED3 fixture (run-cdp.sh wires this up):
//   WT_ADDR=:7681 WT_CMD="sh scripts/emit-ed3.sh" ./web-terminal-server-bin
//
// Screen-frame layout (engine's terminal/wire_binary.go): byte 0 = msg_type
// (0=screen), bytes 9..16 = base (uint64 LE), byte 26 = cursor_flags whose bit4
// (0x10) is scrollbackCleared.
//
// Zero deps (Node 22 global WebSocket). Exit 0 = PASS, non-zero = FAIL.
// Usage: node scripts/cdp-scrollback.cjs
const WT_URL = process.env.WT_URL || "http://127.0.0.1:7681/";
const SESSIONS_URL = new URL("/api/sessions", WT_URL);
const SETTLE_MS = Number(process.env.SETTLE_MS || 3000); // burst renders + base accrues
const AFTER_MS = Number(process.env.AFTER_MS || 3000); // trigger -> ED3 -> frame

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function decodeScreen(buf) {
  if (buf.length < 27 || buf[0] !== 0) {
    return null;
  }
  const dv = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  return { base: Number(dv.getBigUint64(9, true)), scrollbackCleared: (buf[26] & 0x10) !== 0 };
}

(async () => {
  const createResponse = await fetch(SESSIONS_URL, { method: "POST" });
  if (!createResponse.ok) {
    throw new Error(
      `create session: ${createResponse.status} ${createResponse.statusText}: ${await createResponse.text()}`,
    );
  }
  const session = await createResponse.json();
  if (typeof session?.id !== "string" || session.id.length === 0) {
    throw new Error(`create session returned no id: ${JSON.stringify(session)}`);
  }
  const wsURL = new URL("/ws", WT_URL);
  wsURL.protocol = wsURL.protocol === "https:" ? "wss:" : "ws:";
  wsURL.searchParams.set("session", session.id);

  const ws = new WebSocket(wsURL);
  ws.binaryType = "arraybuffer";
  let wsError = null;
  let screenFrames = 0;
  let maxBase = 0;
  let triggeredAt = 0;
  const sbcAfterTrigger = []; // bases of scrollbackCleared frames seen after the trigger

  ws.addEventListener("error", () => {
    wsError = "websocket error connecting to " + wsURL;
  });
  ws.addEventListener("message", (ev) => {
    if (!(ev.data instanceof ArrayBuffer)) {
      return;
    }
    const f = decodeScreen(new Uint8Array(ev.data));
    if (!f) {
      return;
    }
    screenFrames++;
    maxBase = Math.max(maxBase, f.base);
    if (f.scrollbackCleared && triggeredAt > 0) {
      sbcAfterTrigger.push(f.base);
    }
  });

  await new Promise((res) => {
    ws.addEventListener("open", res);
    ws.addEventListener("error", res);
  });
  if (wsError) {
    console.log(JSON.stringify({ wsError }));
    console.log("SCROLLBACK-CLEAR VERIFY: FAIL");
    process.exit(1);
  }
  // Establish PTY dimensions; the server flushes once sized.
  ws.send(
    new Uint8Array([
      0,
      ...new TextEncoder().encode(JSON.stringify({ type: "resize", cols: 80, rows: 24 })),
    ]),
  );

  await sleep(SETTLE_MS); // let the 200-line burst render and commit to history
  const baseBeforeTrigger = maxBase;

  // Deterministic trigger: raw input (a newline) that the fixture is blocked
  // reading. It emits ED3 only now, after the burst has settled.
  triggeredAt = Date.now();
  ws.send(new TextEncoder().encode("\n"));

  await sleep(AFTER_MS);
  try {
    ws.close();
  } catch {
    /* ignore */
  }
  await fetch(new URL(`/api/sessions/${encodeURIComponent(session.id)}`, WT_URL), {
    method: "DELETE",
  }).catch(() => {});

  console.log("=== observed ===");
  console.log(JSON.stringify({ screenFrames, baseBeforeTrigger, sbcAfterTrigger }));

  const checks = {
    "connected and received screen frames": screenFrames > 0,
    "the burst committed history before the trigger (base advanced)": baseBeforeTrigger > 0,
    "an ED3 scrollbackCleared frame arrived after the trigger": sbcAfterTrigger.length > 0,
    "the scrollbackCleared frame carries a non-zero base (history to drop)": sbcAfterTrigger.some(
      (b) => b > 0,
    ),
  };
  console.log("=== verdict ===");
  let ok = true;
  for (const [k, v] of Object.entries(checks)) {
    console.log(`${v ? "PASS" : "FAIL"}  ${k}`);
    ok = ok && v;
  }
  console.log(ok ? "\nSCROLLBACK-CLEAR VERIFY: PASS" : "\nSCROLLBACK-CLEAR VERIFY: FAIL");
  process.exit(ok ? 0 : 1);
})().catch((e) => {
  console.error("VERIFY ERROR:", e.message);
  process.exit(2);
});
