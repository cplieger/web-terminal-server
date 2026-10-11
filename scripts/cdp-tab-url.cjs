// CDP live-verify: each tab's page address and the split's, against the emitter fixture.
// Server: LISTEN_ADDR=:7681 SESSION_CMD="sh scripts/emit-fixture.sh" ./web-terminal-server-bin
// Zero deps. Exit 0 = PASS, non-zero = FAIL. Usage: node scripts/cdp-tab-url.cjs
const CDP = process.env.CDP_URL || "http://127.0.0.1:9222";
const URL = process.env.WT_URL || "http://127.0.0.1:7681/";
const SETTLE_MS = Number(process.env.SETTLE_MS || 6000);
const STEP_MS = Number(process.env.STEP_MS || 1500);
const MINTED = /^#[a-z2-7]{8}$/;
// The <title> static/index.html is served with; the page names itself "<tab> · <this>".
const SERVED_TITLE = process.env.WT_SERVED_TITLE || "Web Terminal";

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

const STATE = `(async () => {
  const [list, layout] = await Promise.all([
    fetch('/api/sessions').then(r => r.json()),
    fetch('/api/sessions/layout').then(r => r.json()),
  ]);
  const alias = (id) => list.find(s => s.id === id)?.alias ?? null;
  const root = document.querySelector('.wt-root.wt-split');
  return {
    hash: location.hash,
    length: history.length,
    open: root ? root.classList.contains('wt-split-open') : null,
    left: alias(layout.left),
    right: alias(layout.right),
    aliases: list.map(s => s.alias),
    toast: document.querySelector('.wt-toast')?.textContent ?? '',
    sentinel: window.__wtSentinel ?? null,
    title: document.title,
    activeLabel: document.querySelector('.wt-tab-scroll .wt-tab.wt-tab-active .wt-tab-label')?.textContent ?? null,
  };
})()`;

let targetId = null;
(async () => {
  const target = await fetch(`${CDP}/json/new?about:blank`, { method: "PUT" }).then((r) => r.json());
  targetId = target.id;
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.addEventListener("open", res); ws.addEventListener("error", rej); });
  let id = 0;
  const errors = [];
  ws.addEventListener("message", (ev) => {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (m.method === "Runtime.exceptionThrown") errors.push("EXC: " + (m.params.exceptionDetails.exception?.description ?? m.params.exceptionDetails.text));
  });
  const ev = async (expression, awaitPromise = false) => {
    const r = await rpc(ws, ++id, "Runtime.evaluate", { expression, returnByValue: true, awaitPromise });
    if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? r.exceptionDetails.text);
    return r.result.value;
  };
  const state = () => ev(STATE, true);
  const click = async (selector) => {
    const r = await ev(`(() => { const el = document.querySelector(${JSON.stringify(selector)}); if (!el) return null; const b = el.getBoundingClientRect(); return { x: b.left + b.width / 2, y: b.top + b.height / 2 }; })()`);
    if (r === null) throw new Error(`click: nothing matches ${selector}`);
    await rpc(ws, ++id, "Input.dispatchMouseEvent", { type: "mousePressed", x: r.x, y: r.y, button: "left", clickCount: 1 });
    await rpc(ws, ++id, "Input.dispatchMouseEvent", { type: "mouseReleased", x: r.x, y: r.y, button: "left", clickCount: 1 });
  };
  const checks = {};
  const check = (label, pass, detail) => {
    checks[label] = pass;
    console.log(`${pass ? "PASS" : "FAIL"}  ${label}${pass ? "" : "  " + JSON.stringify(detail)}`);
  };

  await rpc(ws, ++id, "Page.enable", {});
  await rpc(ws, ++id, "Runtime.enable", {});
  await rpc(ws, ++id, "Emulation.setDeviceMetricsOverride", { width: 1000, height: 700, deviceScaleFactor: 1, mobile: false });
  await rpc(ws, ++id, "Page.navigate", { url: URL });
  await sleep(SETTLE_MS);

  let s = await state();
  check("a fresh load names the shown tab by its minted alias", MINTED.test(s.hash) && s.hash === `#${s.left}`, s);
  const first = s.hash;

  const before = s.length;
  await click(".wt-tab-new");
  await sleep(STEP_MS);
  s = await state();
  check("a new tab pushes its own alias", MINTED.test(s.hash) && s.hash !== first && s.length === before + 1, s);
  check(
    "the browser tab is titled after the active tab, then the app",
    s.activeLabel !== null && s.title === `${s.activeLabel} · ${SERVED_TITLE}`,
    s,
  );
  const second = s.hash;

  await click(".wt-tab-scroll .wt-tab");
  await sleep(STEP_MS);
  s = await state();
  check("clicking the first tab writes its address back", s.hash === first, s);
  check(
    "the title follows the switch without stacking the app name",
    s.activeLabel !== null && s.title === `${s.activeLabel} · ${SERVED_TITLE}`,
    s,
  );

  await rpc(ws, ++id, "Page.reload", {});
  await sleep(SETTLE_MS);
  s = await state();
  check("a reload keeps that tab shown", s.hash === first && `#${s.left}` === first, s);

  await ev("window.__wtSentinel = 'same document'");
  await ev(`location.hash = ${JSON.stringify(`${first},${second.slice(1)}`)}`);
  await sleep(STEP_MS);
  s = await state();
  check(
    "an edited split fragment opens both panes in place",
    s.open === true && `#${s.left}` === first && `#${s.right}` === second && s.sentinel === "same document",
    s,
  );

  await ev("history.back()");
  await sleep(STEP_MS);
  s = await state();
  check("back returns to the single tab", s.hash === first && s.open === false && s.sentinel === "same document", s);

  await ev("location.hash = '#zzzzzzzz'");
  await sleep(STEP_MS);
  s = await state();
  check("an unknown alias is told and corrected", s.toast === "That tab is no longer open" && s.hash === first, s);

  check("no uncaught page exception", errors.length === 0, errors);

  const failed = Object.entries(checks).filter(([, ok]) => !ok).map(([label]) => label);
  console.log(failed.length === 0 ? "\nTAB URL: ALL PASS" : `\nTAB URL: ${failed.length} FAILED: ${failed.join("; ")}`);
  await fetch(`${CDP}/json/close/${targetId}`).catch(() => {});
  process.exit(failed.length === 0 ? 0 : 1);
})().catch(async (err) => {
  console.error("FAIL  harness error:", err);
  if (targetId) await fetch(`${CDP}/json/close/${targetId}`).catch(() => {});
  process.exit(2);
});
