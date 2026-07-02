// CDP live-verify for the disconnect->reconnect resume path (sleep/wake must not
// lose content). Drives a REAL disconnect->reconnect cycle against a
// web-terminal-server instance whose PTY is the deterministic emitter
// (scripts/emit-fixture.sh via WT_CMD), which keeps printing lines server-side
// the whole time. While the socket is severed the emitter accrues lines the
// client never saw; on reconnect the client resumes by absolute index
// (haveThrough) and the server replays exactly the missed lines. The pass
// criteria encode the bug's absence:
//
//   - the client was genuinely DARK during the outage (its maxAbs did not move)
//   - the highest absolute line index GREW once recovered (missed lines landed)
//   - the absolute indices in the DOM are CONTIGUOUS (no gap = nothing missing)
//   - no absolute index appears twice (no duplicate rows)
//   - no console errors/exceptions
//
// Inducing the outage: CDP's Network offline does NOT tear down an already-open
// WebSocket, so instead we wrap window.WebSocket (injected before app code via
// addScriptToEvaluateOnNewDocument — no production hook) to (a) record sockets
// and (b) while a `block` flag is set, immediately close any socket the client
// opens, so every reconnect attempt fails and the client stays dark. Recovery
// clears the flag and fires `pageshow` (the wake signal the UI binds
// reconnectNow to). Zero deps (Node 22 global WebSocket + fetch).
//
// Start the server on the emitter first (reachable from the sidecar host):
//   WT_ADDR=:7681 WT_CMD="sh scripts/emit-fixture.sh" ./web-terminal-server
//
// Usage: node scripts/cdp-resume.cjs
const CDP = process.env.CDP_URL || "http://127.0.0.1:9222";
const URL = process.env.WT_URL || "http://127.0.0.1:7681/";
const SETTLE_MS = Number(process.env.SETTLE_MS || 10000); // initial render (120-line burst + stream)
const OUTAGE_MS = Number(process.env.OUTAGE_MS || 6000); // dark window; emitter prints ~15 lines
const RECOVER_MS = Number(process.env.RECOVER_MS || 8000); // reconnect + resume replay + render

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
const evaluate = (ws, id, expression) =>
  rpc(ws, id, "Runtime.evaluate", { expression, returnByValue: true });

// Wrapper installed before any app script runs. While __wsCtl.block is true,
// every socket the client opens is closed immediately, so no frames flow and
// reconnect attempts keep failing — a faithful "network is dead" outage that
// (unlike CDP offline) actually severs an established WebSocket on demand.
const INSTRUMENT = `(() => {
  const Real = window.WebSocket;
  const ctl = { block: false, socks: [] };
  window.__wsCtl = ctl;
  function Wrapped(url, protocols) {
    const s = protocols === undefined ? new Real(url) : new Real(url, protocols);
    ctl.socks.push(s);
    if (ctl.block) { try { s.close(); } catch (e) {} }
    return s;
  }
  Wrapped.prototype = Real.prototype;
  Wrapped.CONNECTING = Real.CONNECTING; Wrapped.OPEN = Real.OPEN;
  Wrapped.CLOSING = Real.CLOSING; Wrapped.CLOSED = Real.CLOSED;
  window.WebSocket = Wrapped;
})()`;

// Snapshot of the absolute-index integrity of #term-output: every row carries
// data-abs; we read them all and check ordering, contiguity, and uniqueness.
const SNAPSHOT = `(() => {
  const out = document.getElementById('term-output');
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

async function snap(ws, id) {
  const r = await evaluate(ws, id, SNAPSHOT);
  return JSON.parse(r.result.value);
}

(async () => {
  // Open a blank tab first so the instrument script is installed BEFORE the
  // app document loads, then navigate to the server.
  const target = await fetch(`${CDP}/json/new?about:blank`, { method: "PUT" }).then((r) => r.json());
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
    if (m.method === "Runtime.consoleAPICalled" && m.params.type === "error") {
      errors.push("error: " + m.params.args.map((a) => a.value ?? a.description ?? "").join(" "));
    }
    if (m.method === "Runtime.exceptionThrown") {
      errors.push(
        "EXC: " +
          (m.params.exceptionDetails.exception?.description ?? m.params.exceptionDetails.text),
      );
    }
  });
  await rpc(ws, ++id, "Runtime.enable", {});
  await rpc(ws, ++id, "Page.enable", {});
  await rpc(ws, ++id, "Page.addScriptToEvaluateOnNewDocument", { source: INSTRUMENT });
  await rpc(ws, ++id, "Page.navigate", { url: URL });
  await rpc(ws, ++id, "Page.bringToFront", {}).catch(() => {});
  await rpc(ws, ++id, "Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});

  await sleep(SETTLE_MS);
  const before = await snap(ws, ++id);

  // --- induce a real outage: block reconnects, then sever the live socket ---
  await evaluate(
    ws,
    ++id,
    "window.__wsCtl.block = true; window.__wsCtl.socks.forEach(s => { try { s.close(); } catch(e){} }); 'severed'",
  );
  await sleep(OUTAGE_MS); // emitter keeps printing server-side; client is dark
  const during = await snap(ws, ++id);

  // --- recover: unblock + the wake signal the UI reconnects on ---
  await evaluate(ws, ++id, "window.__wsCtl.block = false; 'unblocked'");
  await evaluate(ws, ++id, "window.dispatchEvent(new Event('pageshow'))");
  await sleep(RECOVER_MS);
  const after = await snap(ws, ++id);

  console.log("=== console errors ===");
  console.log(errors.length ? errors.slice(0, 20).join("\n") : "(none)");
  console.log("=== before outage ===");
  console.log(JSON.stringify(before));
  console.log("=== during outage (client dark) ===");
  console.log(JSON.stringify(during));
  console.log("=== after recovery ===");
  console.log(JSON.stringify(after));

  const checks = {
    "no console errors": errors.length === 0,
    "client went dark during outage (maxAbs did not advance)": during.maxAbs === before.maxAbs,
    "missed lines landed after recovery (maxAbs grew)": after.maxAbs > before.maxAbs,
    "backfill is contiguous (no missing lines)": after.contiguous === true,
    "no duplicate rows after recovery": after.dupCount === 0,
    "row==unique invariant holds": after.absCount === after.uniqueCount,
  };
  console.log("=== verdict ===");
  let ok = true;
  for (const [k, v] of Object.entries(checks)) {
    console.log(`${v ? "PASS" : "FAIL"}  ${k}`);
    ok = ok && v;
  }
  console.log(ok ? "\nRESUME VERIFY: PASS" : "\nRESUME VERIFY: FAIL");

  ws.close();
  await fetch(`${CDP}/json/close/${target.id}`);
  process.exit(ok ? 0 : 1);
})().catch((e) => {
  console.error("VERIFY ERROR:", e.message);
  process.exit(2);
});
