// CDP live-verify: the web-terminal stack (engine + UI) renders PTY output
// correctly in a real browser — the display half the Go tests can't reach.
// Opens the server, lets the fixture render, and asserts (pass/fail, no human
// eyeballing) that the stack painted the stream without errors, duplicates, or
// a dead renderer.
//
// Baseline setup (see scripts/run-cdp.sh for the automated version): a
// web-terminal-server on the deterministic emitter, reachable from the browser:
//   WT_ADDR=:7681 WT_CMD="sh scripts/emit-fixture.sh" ./web-terminal-server-bin
//
// Env: CDP_URL (DevTools endpoint), WT_URL (server), WAIT (render settle ms).
// Zero deps (Node 22 global WebSocket + fetch). Cleans up the tab it opens.
// Exit 0 = PASS, non-zero = FAIL. Usage: node scripts/cdp-render.cjs [waitMs]
const CDP = process.env.CDP_URL || "http://127.0.0.1:9222";
const URL = process.env.WT_URL || "http://127.0.0.1:7681/";
const WAIT = Number(process.argv[2] || process.env.WAIT || 12000);

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

(async () => {
  const target = await fetch(`${CDP}/json/new?${encodeURIComponent(URL)}`, { method: "PUT" }).then((r) => r.json());
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.addEventListener("open", res); ws.addEventListener("error", rej); });
  let id = 0;
  const errors = [];
  ws.addEventListener("message", (ev) => {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (m.method === "Runtime.consoleAPICalled" && (m.params.type === "error" || m.params.type === "warning")) {
      errors.push(m.params.type + ": " + m.params.args.map((a) => a.value ?? a.description ?? "").join(" "));
    }
    if (m.method === "Runtime.exceptionThrown") {
      errors.push("EXC: " + (m.params.exceptionDetails.exception?.description ?? m.params.exceptionDetails.text));
    }
  });
  await rpc(ws, ++id, "Runtime.enable", {});
  await rpc(ws, ++id, "Page.enable", {});
  await rpc(ws, ++id, "Page.bringToFront", {}).catch(() => {});
  await rpc(ws, ++id, "Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});

  await sleep(WAIT);

  const expr = `(async () => {
    const rafFired = await new Promise((res) => { let f=false; requestAnimationFrame(()=>{f=true;}); setTimeout(()=>res(f),250); });
    const out = document.querySelector('.term-output, #term-output');
    const term = document.querySelector('.term, #term');
    const rows = out ? Array.from(out.children) : [];
    const text = rows.map(r => (r.textContent||'').replace(/\\u00a0/g,' ').replace(/\\s+$/,'')).filter(t => t.length);
    let maxRunDup=1,cur=1; for(let i=1;i<text.length;i++){ if(text[i]===text[i-1]&&text[i]!==''){cur++;maxRunDup=Math.max(maxRunDup,cur);}else cur=1; }
    return JSON.stringify({
      visibilityState: document.visibilityState, rafFired,
      rowCount: rows.length, nonEmptyLines: text.length,
      firstLines: text.slice(0,2), lastLines: text.slice(-2),
      maxConsecutiveDup: maxRunDup,
      termClientH: term?.clientHeight ?? 0, termScrollH: term?.scrollHeight ?? 0,
      loadingGone: !document.getElementById('loading') || getComputedStyle(document.getElementById('loading')).opacity === '0',
    });
  })()`;
  const snap = JSON.parse((await rpc(ws, ++id, "Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true })).result.value);

  console.log("=== console errors/warnings ===");
  console.log(errors.length ? errors.slice(0, 20).join("\n") : "(none)");
  console.log("=== snapshot ===");
  console.log(JSON.stringify(snap, null, 2));

  const checks = {
    "no console errors or exceptions": errors.length === 0,
    "renderer is live (requestAnimationFrame fires)": snap.rafFired === true,
    "the fixture stream rendered (non-empty rows present)": snap.nonEmptyLines > 0,
    "no duplicate consecutive rows (each fixture line is distinct)": snap.maxConsecutiveDup === 1,
    "scroll geometry is sane (scrollHeight >= clientHeight)": snap.termScrollH >= snap.termClientH && snap.termClientH > 0,
  };
  console.log("=== verdict ===");
  let ok = true;
  for (const [k, v] of Object.entries(checks)) {
    console.log(`${v ? "PASS" : "FAIL"}  ${k}`);
    ok = ok && v;
  }
  console.log(ok ? "\nRENDER VERIFY: PASS" : "\nRENDER VERIFY: FAIL");

  ws.close();
  await fetch(`${CDP}/json/close/${target.id}`).catch(() => {});
  process.exit(ok ? 0 : 1);
})().catch((e) => { console.error("VERIFY ERROR:", e.message); process.exit(2); });
