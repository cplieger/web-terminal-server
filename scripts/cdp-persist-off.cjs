// CDP live-verify: with PERSIST_SCROLLBACK=false the page deletes the lines a
// browser stored while persistence was on, and leaves other site data alone.
// Server: LISTEN_ADDR=:7681 PERSIST_SCROLLBACK=false SESSION_CMD="sh scripts/emit-fixture.sh" ./web-terminal-server-bin
// Zero deps. Exit 0 = PASS, non-zero = FAIL. Usage: node scripts/cdp-persist-off.cjs
const CDP = process.env.CDP_URL || "http://127.0.0.1:9222";
const URL = process.env.WT_URL || "http://127.0.0.1:7681/";
const SETTLE_MS = Number(process.env.SETTLE_MS || 4000);
// localScrollbackStorage's default prefix (web-terminal-ui scrollback-storage.ts).
const STORED_KEY = "wt.scrollback.left-over";
const OTHER_KEY = "not-the-terminal's";

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

let targetId = null;
(async () => {
  const target = await fetch(`${CDP}/json/new?about:blank`, { method: "PUT" }).then((r) =>
    r.json(),
  );
  targetId = target.id;
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.addEventListener("open", res);
    ws.addEventListener("error", rej);
  });
  let id = 0;
  const ev = async (expression) => {
    const r = await rpc(ws, ++id, "Runtime.evaluate", { expression, returnByValue: true });
    if (r.exceptionDetails) {
      throw new Error(r.exceptionDetails.exception?.description ?? r.exceptionDetails.text);
    }
    return r.result.value;
  };

  await rpc(ws, ++id, "Page.enable", {});
  await rpc(ws, ++id, "Page.navigate", { url: URL });
  await sleep(SETTLE_MS);
  const persistMeta = await ev(
    `document.querySelector('meta[name="wt-persist-scrollback"]')?.getAttribute("content") ?? null`,
  );
  // An entry as a page with persistence on would have left it.
  await ev(
    `localStorage.setItem(${JSON.stringify(STORED_KEY)}, "{}"); localStorage.setItem(${JSON.stringify(OTHER_KEY)}, "kept")`,
  );
  await rpc(ws, ++id, "Page.reload", {});
  await sleep(SETTLE_MS);
  const after = await ev(
    `({ stored: localStorage.getItem(${JSON.stringify(STORED_KEY)}), other: localStorage.getItem(${JSON.stringify(OTHER_KEY)}), started: document.querySelector(".wt-root") !== null })`,
  );
  await ev(`localStorage.removeItem(${JSON.stringify(OTHER_KEY)})`);

  const checks = {
    "the server serves persistence off": persistMeta === "off",
    "the terminal started": after.started,
    "the stored scrollback entry is gone after a load": after.stored === null,
    "other site data is untouched": after.other === "kept",
  };
  console.log(JSON.stringify({ persistMeta, after }));
  let ok = true;
  for (const [k, v] of Object.entries(checks)) {
    console.log(`${v ? "PASS" : "FAIL"}  ${k}`);
    ok = ok && v;
  }
  ws.close();
  console.log(ok ? "\nPERSIST-OFF VERIFY: PASS" : "\nPERSIST-OFF VERIFY: FAIL");
  return ok;
})()
  .then(async (ok) => {
    await fetch(`${CDP}/json/close/${targetId}`).catch(() => {});
    process.exit(ok ? 0 : 1);
  })
  .catch(async (e) => {
    const message = String(e?.message ?? e).replace(/\n|\r/g, "");
    console.error(`VERIFY ERROR: ${message}`);
    if (targetId !== null) {
      await fetch(`${CDP}/json/close/${targetId}`).catch(() => {});
    }
    process.exit(2);
  });
