// CDP live-verify: soft-keyboard / viewport reflow. When the visual viewport
// shrinks (an on-screen keyboard opening on mobile), the UI must (1) resize the
// content area to the smaller viewport and (2) tell the server by sending a
// `resize` control-frame with fewer rows, so the PTY/screen shrink to match.
// Consolidates the old cdp-kbd (reflow) + cdp-kbd2 (resize-frame) diagnostics
// into one pass/fail check.
//
// Simulates the keyboard by redefining window.visualViewport.height (the iOS
// case: layout viewport stays, visual viewport shrinks) and dispatching the
// 'resize' event the UI listens to. Instruments WebSocket.send (before app code
// via addScriptToEvaluateOnNewDocument — no production hook) to capture v4 text
// controls and legacy 0x00-prefixed binary controls.
//
// Baseline setup: a server on the emitter, reachable from the browser:
//   WT_ADDR=:7681 WT_CMD="sh scripts/emit-fixture.sh" ./web-terminal-server-bin
//
// Zero deps. Exit 0 = PASS, non-zero = FAIL. Usage: node scripts/cdp-viewport.cjs
const CDP = process.env.CDP_URL || "http://127.0.0.1:9222";
const URL = process.env.WT_URL || "http://127.0.0.1:7681/";
const SETTLE_MS = Number(process.env.SETTLE_MS || 12000);
const KB = Number(process.env.KB || 320); // simulated soft-keyboard height (px)

function rpc(ws, id, method, params) {
  return new Promise((resolve, reject) => {
    const onMsg = (ev) => {
      let m;
      try {
        m = JSON.parse(ev.data);
      } catch {
        return;
      }
      if (m.id === id) {
        ws.removeEventListener("message", onMsg);
        m.error ? reject(new Error(method + ": " + JSON.stringify(m.error))) : resolve(m.result);
      }
    };
    ws.addEventListener("message", onMsg);
    ws.send(JSON.stringify({ id, method, params }));
  });
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Capture every v4 text control and legacy 0x00-prefixed binary control.
const INSTRUMENT = `(() => {
  const RealSend = WebSocket.prototype.send;
  window.__ctl = []; const dec = new TextDecoder();
  WebSocket.prototype.send = function(data) {
    try {
      if (typeof data === "string") {
        window.__ctl.push({ t: Date.now(), s: data });
      } else {
        let b = null;
        if (data instanceof ArrayBuffer) b = new Uint8Array(data);
        else if (ArrayBuffer.isView(data)) b = new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
        if (b && b.length > 1 && b[0] === 0x00) window.__ctl.push({ t: Date.now(), s: dec.decode(b.subarray(1)) });
      }
    } catch (e) {}
    return RealSend.apply(this, arguments);
  };
})()`;

const SNAP_TERM = `(() => { const t = document.querySelector('.term, #term');
  return JSON.stringify({ termCH: t ? t.clientHeight : 0, vvH: window.visualViewport ? Math.round(window.visualViewport.height) : null, innerH: window.innerHeight }); })()`;

const OPEN_KB = `(() => { const vv = window.visualViewport; if (!vv) return 'no-vv'; const h=${KB};
  window.__t0 = Date.now();
  Object.defineProperty(vv,'height',{configurable:true,get:()=>window.innerHeight-h});
  Object.defineProperty(vv,'offsetTop',{configurable:true,get:()=>0});
  vv.dispatchEvent(new Event('resize')); return 'ok'; })()`;

const DUMP_CTL = `(() => JSON.stringify({ t0: window.__t0 || 0, ctl: window.__ctl || [] }))()`;

(async () => {
  const target = await fetch(`${CDP}/json/new?about:blank`, { method: "PUT" }).then((r) =>
    r.json(),
  );
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.addEventListener("open", res);
    ws.addEventListener("error", rej);
  });
  let id = 0;
  const errors = [];
  ws.addEventListener("message", (ev) => {
    let m;
    try {
      m = JSON.parse(ev.data);
    } catch {
      return;
    }
    if (m.method === "Runtime.consoleAPICalled" && m.params.type === "error")
      errors.push(m.params.args.map((a) => a.value ?? a.description ?? "").join(" "));
    if (m.method === "Runtime.exceptionThrown")
      errors.push(
        "EXC: " +
          (m.params.exceptionDetails.exception?.description ?? m.params.exceptionDetails.text),
      );
  });
  const ev = async (expression) =>
    (
      await rpc(ws, ++id, "Runtime.evaluate", {
        expression,
        returnByValue: true,
        awaitPromise: true,
      })
    ).result.value;

  await rpc(ws, ++id, "Page.enable", {});
  await rpc(ws, ++id, "Runtime.enable", {});
  // A realistic mobile viewport so visualViewport is meaningful and the reflow is deterministic.
  await rpc(ws, ++id, "Emulation.setDeviceMetricsOverride", {
    width: 390,
    height: 844,
    deviceScaleFactor: 2,
    mobile: true,
  });
  await rpc(ws, ++id, "Page.addScriptToEvaluateOnNewDocument", { source: INSTRUMENT });
  await rpc(ws, ++id, "Page.navigate", { url: URL });
  await rpc(ws, ++id, "Page.bringToFront", {}).catch(() => {});
  await rpc(ws, ++id, "Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});

  await sleep(SETTLE_MS);
  const before = JSON.parse(await ev(SNAP_TERM));
  const openRes = await ev(OPEN_KB);
  await sleep(2500); // vv resize -> UI reflow -> resize frame -> server round-trip
  const after = JSON.parse(await ev(SNAP_TERM));
  const dump = JSON.parse(await ev(DUMP_CTL));

  const resizes = [];
  for (const f of dump.ctl) {
    try {
      const j = JSON.parse(f.s);
      if (j.type === "resize") resizes.push({ t: f.t, rows: j.rows, cols: j.cols });
    } catch (e) {}
  }
  const beforeResize = [...resizes].reverse().find((r) => r.t < dump.t0);
  const afterResizes = resizes.filter((r) => r.t >= dump.t0);
  const lastAfter = afterResizes[afterResizes.length - 1];

  console.log("=== console errors ===");
  console.log(errors.length ? errors.slice(0, 10).join("\n") : "(none)");
  console.log("open:", openRes);
  console.log("termCH:", before.termCH, "->", after.termCH, " vvH:", before.vvH, "->", after.vvH);
  console.log(
    "resize frames before kb:",
    JSON.stringify(beforeResize || null),
    " after kb:",
    JSON.stringify(afterResizes),
  );

  const checks = {
    "visualViewport is available to simulate the keyboard": openRes === "ok",
    "no console errors": errors.length === 0,
    "content area reflowed to the smaller viewport (term clientHeight shrank)":
      after.termCH > 0 && after.termCH < before.termCH,
    "a resize control-frame was sent after the keyboard opened": afterResizes.length > 0,
    "the resize told the server fewer rows": !!(
      beforeResize &&
      lastAfter &&
      lastAfter.rows < beforeResize.rows
    ),
  };
  console.log("=== verdict ===");
  let ok = true;
  for (const [k, v] of Object.entries(checks)) {
    console.log(`${v ? "PASS" : "FAIL"}  ${k}`);
    ok = ok && v;
  }
  console.log(ok ? "\nVIEWPORT VERIFY: PASS" : "\nVIEWPORT VERIFY: FAIL");

  ws.close();
  await fetch(`${CDP}/json/close/${target.id}`).catch(() => {});
  process.exit(ok ? 0 : 1);
})().catch((e) => {
  console.error("VERIFY ERROR:", e.message);
  process.exit(2);
});
