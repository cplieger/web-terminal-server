// CDP live-verify: repeated resize / rotation must not corrupt the absolute-
// index model. Drives several portrait<->landscape resizes against the live
// stream and asserts the DOM stays consistent: no duplicate data-abs rows (the
// class of bug where a redraw re-appends content instead of overwriting it), no
// gaps in the absolute indices, and the row count stays bounded (scrollback cap
// holds). Consolidates the old cdp-rotate + cdp-rotate2 diagnostics into one
// pass/fail check, using the generic emitter instead of grepping a specific
// app's banner text.
//
// Baseline setup: a server on the emitter, reachable from the browser:
//   WT_ADDR=:7681 WT_CMD="sh scripts/emit-fixture.sh" ./web-terminal-server-bin
//
// Zero deps. Exit 0 = PASS, non-zero = FAIL. Usage: node scripts/cdp-resize.cjs
const CDP = process.env.CDP_URL || "http://127.0.0.1:9222";
const URL = process.env.WT_URL || "http://127.0.0.1:7681/";
const SETTLE_MS = Number(process.env.SETTLE_MS || 10000);
const STEP_MS = Number(process.env.STEP_MS || 2500); // per-rotation settle + redraw
const ROW_CAP = Number(process.env.ROW_CAP || 5200); // store cap (~5000) + a window's worth

function rpc(ws, id, method, params) {
  return new Promise((resolve, reject) => {
    const onMsg = (ev) => {
      let m;
      try { m = JSON.parse(ev.data); } catch { return; }
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

// Absolute-index integrity of #term-output: order, contiguity, uniqueness.
const SNAPSHOT = `(() => {
  const out = document.querySelector('.term-output, #term-output');
  const rows = out ? Array.from(out.children) : [];
  const abs = rows.map(r => Number(r.getAttribute('data-abs'))).filter(n => Number.isFinite(n));
  const sorted = [...abs].sort((a,b)=>a-b);
  const seen = new Set(), dups = [];
  for (const n of abs) { if (seen.has(n)) dups.push(n); else seen.add(n); }
  const gaps = [];
  for (let i=1;i<sorted.length;i++){ const d=sorted[i]-sorted[i-1]; if (d!==1) gaps.push([sorted[i-1],sorted[i]]); }
  return JSON.stringify({
    rowCount: rows.length, absCount: abs.length, uniqueCount: seen.size,
    minAbs: sorted[0] ?? null, maxAbs: sorted[sorted.length-1] ?? null,
    dupCount: dups.length, dupSample: dups.slice(0,5),
    contiguous: gaps.length === 0, gapSample: gaps.slice(0,5)
  });
})()`;

(async () => {
  const target = await fetch(`${CDP}/json/new?about:blank`, { method: "PUT" }).then((r) => r.json());
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.addEventListener("open", res); ws.addEventListener("error", rej); });
  let id = 0;
  const errors = [];
  ws.addEventListener("message", (ev) => {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (m.method === "Runtime.consoleAPICalled" && m.params.type === "error") errors.push(m.params.args.map((a) => a.value ?? a.description ?? "").join(" "));
    if (m.method === "Runtime.exceptionThrown") errors.push("EXC: " + (m.params.exceptionDetails.exception?.description ?? m.params.exceptionDetails.text));
  });
  const ev = async (expression) => (await rpc(ws, ++id, "Runtime.evaluate", { expression, returnByValue: true })).result.value;
  const setSize = (w, h) => rpc(ws, ++id, "Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 2, mobile: true });
  // Force a resize event too — a metrics override alone doesn't always fire one.
  const fireResize = () => ev(`(() => { window.dispatchEvent(new Event('resize')); if (window.visualViewport) window.visualViewport.dispatchEvent(new Event('resize')); return 'ok'; })()`);

  await rpc(ws, ++id, "Page.enable", {});
  await rpc(ws, ++id, "Runtime.enable", {});
  await setSize(390, 844);
  await rpc(ws, ++id, "Page.navigate", { url: URL });
  await rpc(ws, ++id, "Page.bringToFront", {}).catch(() => {});
  await rpc(ws, ++id, "Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});

  await sleep(SETTLE_MS);
  const initial = JSON.parse(await ev(SNAPSHOT));
  console.log("INITIAL:", JSON.stringify(initial));

  const sizes = [[844, 390], [390, 844], [844, 390], [390, 844], [844, 390]];
  let n = 0;
  let last = initial;
  for (const [w, h] of sizes) {
    n++;
    await setSize(w, h);
    await sleep(300);
    await fireResize();
    await sleep(STEP_MS); // settle + server round-trip + redraw
    last = JSON.parse(await ev(SNAPSHOT));
    console.log(`rotate #${n} (${w}x${h}):`, JSON.stringify(last));
  }
  await rpc(ws, ++id, "Emulation.clearDeviceMetricsOverride", {}).catch(() => {});

  console.log("=== console errors ===");
  console.log(errors.length ? errors.slice(0, 10).join("\n") : "(none)");

  const checks = {
    "no console errors": errors.length === 0,
    "content is still present after the rotations": last.absCount > 0,
    "no duplicate rows accumulated across rotations": last.dupCount === 0,
    "every row has a unique absolute index": last.absCount === last.uniqueCount,
    "absolute indices remain contiguous (no gaps)": last.contiguous === true,
    "row count stayed bounded (scrollback cap held)": last.rowCount <= ROW_CAP,
  };
  console.log("=== verdict ===");
  let ok = true;
  for (const [k, v] of Object.entries(checks)) {
    console.log(`${v ? "PASS" : "FAIL"}  ${k}`);
    ok = ok && v;
  }
  console.log(ok ? "\nRESIZE VERIFY: PASS" : "\nRESIZE VERIFY: FAIL");

  ws.close();
  await fetch(`${CDP}/json/close/${target.id}`).catch(() => {});
  process.exit(ok ? 0 : 1);
})().catch((e) => { console.error("VERIFY ERROR:", e.message); process.exit(2); });
