// CDP live-verify: the two-pane split view against the emitter fixture.
// Server: LISTEN_ADDR=:7681 SESSION_CMD="sh scripts/emit-fixture.sh" ./web-terminal-server-bin
// Zero deps. Exit 0 = PASS, non-zero = FAIL. Usage: node scripts/cdp-split.cjs
const CDP = process.env.CDP_URL || "http://127.0.0.1:9222";
const URL = process.env.WT_URL || "http://127.0.0.1:7681/";
const SETTLE_MS = Number(process.env.SETTLE_MS || 8000);
const STEP_MS = Number(process.env.STEP_MS || 1500);
const SHOT_PATH = process.env.SHOT_PATH || "";
const WIDTH = 1000;
const HEIGHT = 700;
const MIN_PANE_PX = 360;
const RESIZE_INTERVAL_MS = 100;
const DRAG_MS = 400;
const DRAG_STEPS = 20;

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

// Before the wire upgrade a control frame is binary, 0x00 then JSON (CDP hands
// the payload base64-encoded); after it the JSON travels as a text frame.
function controlType(frame) {
  let text;
  if (frame.opcode === 1) {
    text = frame.payloadData;
  } else if (frame.opcode === 2) {
    const bytes = Buffer.from(frame.payloadData, "base64");
    if (bytes.length < 2 || bytes[0] !== 0) return null;
    text = bytes.subarray(1).toString("utf8");
  } else {
    return null;
  }
  try { return JSON.parse(text).type ?? null; } catch { return null; }
}
// CDP reports no close code, so the page's WebSocket is wrapped to record the
// code each close() asks for and the code the close event carries.
const WS_SHIM = `(() => {
  const log = [];
  const Native = window.WebSocket;
  class Recorded extends Native {
    constructor(url, protocols) {
      super(url, protocols);
      const entry = { url: String(url), closedByPage: false, requested: null, event: null };
      log.push(entry);
      this.addEventListener('close', (ev) => { entry.event = ev.code; });
      this.__entry = entry;
    }
    close(code, reason) {
      this.__entry.closedByPage = true;
      this.__entry.requested = code ?? null;
      super.close(code, reason);
    }
  }
  window.WebSocket = Recorded;
  window.__wsLog = log;
})()`;

const PANE = (side) => `(() => {
  const p = document.querySelector('.wt-root.wt-split > .wt-split-pane.wt-side-${side}');
  if (!p) return null;
  const out = p.querySelector('.term-output');
  const rows = out ? Array.from(out.children) : [];
  const abs = rows.map(r => Number(r.getAttribute('data-abs'))).filter(Number.isFinite);
  const cur = p.querySelector('.term-cursor-overlay');
  const cs = cur ? getComputedStyle(cur) : null;
  return {
    selected: p.classList.contains('wt-pane-selected'),
    hidden: p.classList.contains('wt-pane-hidden'),
    inert: p.hasAttribute('inert'),
    visibility: getComputedStyle(p).visibility,
    width: p.clientWidth,
    rows: rows.length,
    maxAbs: abs.length ? Math.max(...abs) : -1,
    text: rows.map(r => r.textContent).join('\\n'),
    cursorBg: cs ? cs.backgroundColor : null,
    cursorShadow: cs ? cs.boxShadow : null,
    cursorAnimation: cs ? cs.animationName : null,
    termFocus: p.querySelector('.term')?.classList.contains('focus') ?? null,
    inputFocused: p.querySelector('.term-input') === document.activeElement,
  };
})()`;
const SHELL = `(() => {
  const root = document.querySelector('.wt-root.wt-split');
  const handle = root.querySelector(':scope > .wt-split-handle');
  const btn = document.querySelector('.wt-tab-split');
  const chips = Array.from(document.querySelectorAll('.wt-tab'));
  return {
    open: root.classList.contains('wt-split-open'),
    collapsed: root.classList.contains('wt-split-collapsed'),
    panes: root.querySelectorAll(':scope > .wt-split-pane').length,
    ratio: getComputedStyle(root).getPropertyValue('--wt-split-ratio').trim(),
    handleDisplay: handle ? getComputedStyle(handle).display : null,
    handleFaces: handle ? handle.dataset.faces : null,
    handleValueNow: handle ? handle.getAttribute('aria-valuenow') : null,
    handleValueMin: handle ? handle.getAttribute('aria-valuemin') : null,
    handleValueMax: handle ? handle.getAttribute('aria-valuemax') : null,
    handleFocused: handle === document.activeElement,
    btnExpanded: btn ? btn.getAttribute('aria-expanded') : null,
    btnHidden: btn ? btn.hidden : null,
    btnTabindex: btn ? btn.getAttribute('tabindex') : null,
    btnFocused: btn === document.activeElement,
    active: document.activeElement ? document.activeElement.tagName + '.' + document.activeElement.className : null,
    chips: chips.map(c => ({ label: c.querySelector('.wt-tab-label')?.textContent ?? '', active: c.classList.contains('wt-tab-active'), expanded: c.getAttribute('aria-expanded'), focused: c === document.activeElement })),
    multiselectable: document.querySelector('.wt-tab-scroll')?.getAttribute('aria-multiselectable') ?? null,
  };
})()`;
const MENU = `(() => {
  const menu = document.querySelector('.wt-tab-menu');
  if (!menu || !menu.classList.contains('visible')) return null;
  return Array.from(menu.querySelectorAll('[role="menuitem"]')).map(b => ({ label: b.textContent, disabled: b.disabled }));
})()`;
const rectOf = (selector) => `(() => { const el = document.querySelector(${JSON.stringify(selector)}); if (!el) return null; const r = el.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`;

let targetId = null;
(async () => {
  const target = await fetch(`${CDP}/json/new?about:blank`, { method: "PUT" }).then((r) => r.json());
  targetId = target.id;
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.addEventListener("open", res); ws.addEventListener("error", rej); });
  let id = 0;
  const errors = [];
  const sockets = new Map();
  let puts = 0;
  ws.addEventListener("message", (ev) => {
    let m;
    try { m = JSON.parse(ev.data); } catch { return; }
    if (m.method === "Runtime.consoleAPICalled" && m.params.type === "error") errors.push(m.params.args.map((a) => a.value ?? a.description ?? "").join(" "));
    if (m.method === "Runtime.exceptionThrown") errors.push("EXC: " + (m.params.exceptionDetails.exception?.description ?? m.params.exceptionDetails.text));
    if (m.method === "Network.webSocketCreated") sockets.set(m.params.requestId, { url: m.params.url, open: true, sentResize: 0 });
    if (m.method === "Network.webSocketClosed") { const s = sockets.get(m.params.requestId); if (s) s.open = false; }
    if (m.method === "Network.webSocketFrameSent") {
      const s = sockets.get(m.params.requestId);
      if (s && controlType(m.params.response) === "resize") s.sentResize++;
    }
    if (m.method === "Network.requestWillBeSent" && m.params.request.method === "PUT" && m.params.request.url.endsWith("/api/sessions/layout")) puts++;
  });
  const ev = async (expression, awaitPromise = false) => {
    const r = await rpc(ws, ++id, "Runtime.evaluate", { expression, returnByValue: true, awaitPromise });
    if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description ?? r.exceptionDetails.text);
    return r.result.value;
  };
  const setSize = (w, h) => rpc(ws, ++id, "Emulation.setDeviceMetricsOverride", { width: w, height: h, deviceScaleFactor: 1, mobile: false });
  const mouse = (type, x, y, extra = {}) => rpc(ws, ++id, "Input.dispatchMouseEvent", { type, x, y, button: "left", clickCount: 1, ...extra });
  const click = async (selector, button = "left") => {
    const r = await ev(rectOf(selector));
    if (r === null) throw new Error(`click: nothing matches ${selector}`);
    await mouse("mousePressed", r.x, r.y, { button });
    await mouse("mouseReleased", r.x, r.y, { button });
    return r;
  };
  // Enter and Space carry their character, which is what makes a focused button click.
  const KEY_TEXT = { Enter: "\r", " ": " " };
  const key = async (keyName, opts = {}) => {
    const base = { key: keyName, code: opts.code ?? keyName, windowsVirtualKeyCode: opts.vk, modifiers: opts.modifiers ?? 0, autoRepeat: opts.repeat ?? false };
    const text = KEY_TEXT[keyName];
    await rpc(ws, ++id, "Input.dispatchKeyEvent", { type: "keyDown", ...base, ...(text ? { text } : {}) });
    if (!opts.holdDown) await rpc(ws, ++id, "Input.dispatchKeyEvent", { type: "keyUp", ...base });
  };
  const keyUp = (keyName, opts = {}) => rpc(ws, ++id, "Input.dispatchKeyEvent", { type: "keyUp", key: keyName, code: opts.code ?? keyName, windowsVirtualKeyCode: opts.vk, modifiers: 0 });
  const VK = { Tab: 9, Enter: 13, Escape: 27, F10: 121, End: 35, Home: 36, ArrowLeft: 37, ArrowRight: 39 };
  const sessionSockets = () => [...sockets.values()].filter((s) => s.url.includes("session="));
  const layout = () => ev(`fetch('/api/sessions/layout').then(r => r.json())`, true);
  const tabTo = async (predicate, max = 12) => {
    for (let i = 0; i < max; i++) {
      await key("Tab", { vk: VK.Tab });
      if (await ev(predicate)) return i + 1;
    }
    return -1;
  };
  const checks = {};
  const check = (label, pass) => { checks[label] = pass; console.log(`${pass ? "PASS" : "FAIL"}  ${label}`); };
  const shell = async () => ev(SHELL);
  const pane = async (side) => ev(PANE(side));

  await rpc(ws, ++id, "Page.enable", {});
  await rpc(ws, ++id, "Runtime.enable", {});
  await rpc(ws, ++id, "Network.enable", {});
  await rpc(ws, ++id, "Page.addScriptToEvaluateOnNewDocument", { source: WS_SHIM });
  await setSize(WIDTH, HEIGHT);
  await rpc(ws, ++id, "Page.navigate", { url: URL });
  await rpc(ws, ++id, "Page.bringToFront", {}).catch(() => {});
  await rpc(ws, ++id, "Emulation.setFocusEmulationEnabled", { enabled: true }).catch(() => {});
  await sleep(SETTLE_MS);

  console.log("=== 1. topology before the split ===");
  let s = await shell();
  console.log(JSON.stringify({ open: s.open, panes: s.panes, btnExpanded: s.btnExpanded, btnTabindex: s.btnTabindex, btnHidden: s.btnHidden, active: s.active }));
  check("split root with one pane, split closed", s.open === false && s.panes === 1);
  check("split button present, aria-expanded=false, no tabindex, not hidden", s.btnExpanded === "false" && s.btnTabindex === null && s.btnHidden === false);
  check("one session socket before the split", sessionSockets().filter((x) => x.open).length === 1);
  const firstLayout = await layout();
  check("layout record answers with the shown session on the left", firstLayout.open === false && typeof firstLayout.left === "string" && firstLayout.right === null);

  console.log("=== 2. Tab from the page start reaches the button, Enter opens the split ===");
  // With the split closed the one pane's input is not a Tab stop, so the row is
  // the first thing Tab reaches from the page start.
  await ev(`document.activeElement.blur(); 'ok'`);
  const fromStart = await tabTo(`document.activeElement === document.querySelector('.wt-tab-split')`, 6);
  s = await shell();
  console.log(JSON.stringify({ tabPresses: fromStart, active: s.active }));
  check("Tab from the page start reaches the split button (chip, +, split)", fromStart === 3 && s.btnFocused);
  await key("Enter", { vk: VK.Enter });
  await sleep(STEP_MS);
  s = await shell();
  let left = await pane("left");
  let right = await pane("right");
  console.log(JSON.stringify({ open: s.open, panes: s.panes, btnExpanded: s.btnExpanded, btnFocused: s.btnFocused, ratio: s.ratio, handleDisplay: s.handleDisplay, rightInert: right?.inert, rightRows: right?.rows }));
  check("Enter opens the split: two panes, handle shown, aria-expanded=true", s.open && s.panes === 2 && s.handleDisplay === "block" && s.btnExpanded === "true");
  check("focus stays on the button after a keyboard toggle", s.btnFocused);
  check("the new right pane is empty and inert, the left selected", right !== null && right.inert && right.rows === 0 && left.selected);
  check("still one session socket while the right pane is empty", sessionSockets().filter((x) => x.open).length === 1);
  await click(".wt-tab-new");
  await sleep(SETTLE_MS / 2);
  s = await shell();
  left = await pane("left");
  right = await pane("right");
  console.log(JSON.stringify({ chips: s.chips, multiselectable: s.multiselectable, leftRows: left.rows, rightRows: right.rows, rightSelected: right.selected, faces: s.handleFaces, sockets: sessionSockets().filter((x) => x.open).map((x) => x.url.replace(/^.*session=/, "session=")) }));
  check("+ lands the new tab in the empty right pane and selects it", right.rows > 0 && right.selected && !left.selected && s.handleFaces === "right");
  check("two session sockets, one per shown pane", sessionSockets().filter((x) => x.open).length === 2);
  const shownChips = (x) => x.chips.filter((c) => c.active);
  check("both shown chips render active with aria-expanded=true, the rest false, row multiselectable", shownChips(s).length === 2 && shownChips(s).every((c) => c.expanded === "true") && s.chips.filter((c) => !c.active).every((c) => c.expanded === "false") && s.multiselectable === "true");
  check("both panes receive the emitter's output", left.maxAbs > 0 && right.maxAbs > 0);

  console.log("=== 3. typing echoes in its own pane only ===");
  await click(".wt-split-pane.wt-side-left .term");
  await sleep(200);
  // The tabs feature moves focus after a keyboard snap only once a hardware key
  // was seen in a terminal; headless Chromium reports no pointer, so nothing else would.
  await key("ArrowRight", { vk: VK.ArrowRight });
  await rpc(ws, ++id, "Input.insertText", { text: "qzx-left-7" });
  await sleep(STEP_MS);
  left = await pane("left");
  right = await pane("right");
  check("a click in the left pane selects it (class, handle edge)", left.selected && !right.selected && (await shell()).handleFaces === "left");
  check("text typed into the left pane echoes in the left output only", left.text.includes("qzx-left-7") && !right.text.includes("qzx-left-7"));
  await click(".wt-split-pane.wt-side-right .term");
  await sleep(200);
  await rpc(ws, ++id, "Input.insertText", { text: "qzx-right-3" });
  await sleep(STEP_MS);
  left = await pane("left");
  right = await pane("right");
  check("a click in the right pane selects it back", right.selected && !left.selected);
  check("text typed into the right pane echoes in the right output only", right.text.includes("qzx-right-3") && !left.text.includes("qzx-right-3"));

  console.log("=== 4. the cursor follows the selection class, not only focus ===");
  // The blink phase flips every 530ms, so the filled state is sampled over a cycle.
  const sampleCursor = async (side) => {
    const samples = [];
    for (let i = 0; i < 8; i++) { samples.push(await pane(side)); await sleep(150); }
    return samples;
  };
  let unsel = await sampleCursor("left");
  console.log("unselected cursor:", JSON.stringify(unsel.map((p) => [p.cursorBg, p.cursorShadow, p.cursorAnimation])[0]));
  check("unselected pane's cursor is hollow: transparent, inset shadow, no blink", unsel.every((p) => p.cursorBg === "rgba(0, 0, 0, 0)" && p.cursorShadow !== "none" && p.cursorAnimation === "none"));
  let sel = await sampleCursor("right");
  const filled = (p) => p.cursorBg !== "rgba(0, 0, 0, 0)";
  console.log("selected cursor (focused):", JSON.stringify(sel.map((p) => p.cursorBg)));
  check("selected pane's cursor fills with the text colour while focused", sel.some(filled) && sel.every((p) => p.cursorShadow === "none"));
  // The terminal owns Tab (a byte to the PTY), so the walk to the button starts on the row.
  await ev(`document.querySelector('.wt-split-pane.wt-pane-selected .term-input').blur(); document.querySelectorAll('.wt-tab.wt-tab-active')[1].focus(); 'ok'`);
  const toSplitButton = await tabTo(`document.activeElement === document.querySelector('.wt-tab-split')`, 4);
  s = await shell();
  right = await pane("right");
  console.log(JSON.stringify({ tabPresses: toSplitButton, active: s.active, rightTermFocus: right.termFocus, rightSelected: right.selected }));
  check("Tab from a chip reaches the split button", toSplitButton > 0 && s.btnFocused);
  check("moving focus to the tab row leaves the selection where it was", right.selected && right.termFocus === false);
  sel = await sampleCursor("right");
  console.log("selected cursor (unfocused):", JSON.stringify(sel.map((p) => p.cursorBg)));
  check("selected pane's cursor stays filled with focus on the tab row (class-driven rule)", sel.some(filled) && sel.every((p) => p.cursorShadow === "none"));

  console.log("=== 5. the keyboard path: Enter on the button toggles ===");
  const chipCount = s.chips.length;
  await key("Enter", { vk: VK.Enter });
  await sleep(STEP_MS);
  s = await shell();
  console.log(JSON.stringify({ open: s.open, btnExpanded: s.btnExpanded, btnFocused: s.btnFocused, chips: s.chips, ratio: s.ratio }));
  check("Enter on the focused split button closes the split, aria-expanded=false, focus kept", !s.open && s.btnExpanded === "false" && s.btnFocused);
  check("the hidden pane's tab stays in the row as an ordinary chip", s.chips.length === chipCount && shownChips(s).length === 1 && s.chips.every((c) => c.expanded === null) && s.multiselectable === null);
  check("closing the split releases the ratio to 0.5", s.ratio === "0.5");
  await key(" ", { code: "Space", vk: 32 });
  await sleep(STEP_MS);
  s = await shell();
  right = await pane("right");
  left = await pane("left");
  console.log(JSON.stringify({ open: s.open, btnExpanded: s.btnExpanded, btnFocused: s.btnFocused, leftRows: left.rows, rightRows: right.rows }));
  check("Space reopens it with the survivor shown on the left and the right pane empty", s.open && s.btnExpanded === "true" && s.btnFocused && left.rows > 0 && right.rows === 0);
  const survivorSide = left.rows > 0 ? "left" : "right";
  const emptySide = survivorSide === "left" ? "right" : "left";

  console.log("=== 6. snap items from a right-click and from the keyboard ===");
  await click(".wt-tab:not(.wt-tab-active)", "right");
  await sleep(300);
  let menu = await ev(MENU);
  console.log("right-click menu:", JSON.stringify(menu));
  const snapItems = (m) => (m ?? []).filter((i) => i.label === "Snap to left" || i.label === "Snap to right");
  check("a right-click on an unshown chip lists Snap to left and Snap to right, both enabled", snapItems(menu).length === 2 && snapItems(menu).every((i) => !i.disabled));
  await click(".wt-split-pane.wt-pane-selected .term");
  await sleep(200);
  check("a click away dismisses the tab menu", (await ev(MENU)) === null);
  await ev(`document.querySelector('.wt-tab.wt-tab-active').focus(); 'ok'`);
  // Shift+F10 as a raw key down is what Chromium turns into the keyboard-raised
  // contextmenu event (button -1); a keyDown with text does not.
  await rpc(ws, ++id, "Input.dispatchKeyEvent", { type: "rawKeyDown", key: "F10", code: "F10", windowsVirtualKeyCode: VK.F10, modifiers: 8 });
  await rpc(ws, ++id, "Input.dispatchKeyEvent", { type: "keyUp", key: "F10", code: "F10", windowsVirtualKeyCode: VK.F10, modifiers: 8 });
  await sleep(300);
  menu = await ev(MENU);
  console.log("Shift+F10 menu:", JSON.stringify(menu));
  const shownChipSnaps = snapItems(menu);
  check("Shift+F10 on the focused shown chip opens the menu with the other side's snap item enabled", shownChipSnaps.length === 2 && shownChipSnaps.find((i) => i.label === `Snap to ${emptySide}`)?.disabled === false && shownChipSnaps.find((i) => i.label === `Snap to ${survivorSide}`)?.disabled === true);
  const putsBeforeSnap = puts;
  await ev(`Array.from(document.querySelectorAll('.wt-tab-menu [role="menuitem"]')).find(b => b.textContent === 'Snap to ${emptySide}').focus(); 'ok'`);
  await key("Enter", { vk: VK.Enter });
  await sleep(STEP_MS);
  s = await shell();
  left = await pane("left");
  right = await pane("right");
  const snapped = emptySide === "left" ? left : right;
  const vacated = emptySide === "left" ? right : left;
  const rec = await layout();
  console.log(JSON.stringify({ snappedRows: snapped.rows, snappedSelected: snapped.selected, snappedInputFocused: snapped.inputFocused, vacatedRows: vacated.rows, vacatedInert: vacated.inert, active: s.active, layout: rec, puts: puts - putsBeforeSnap }));
  check("Enter on Snap moves the shown tab to the other side and empties the old one", snapped.rows > 0 && snapped.selected && vacated.rows === 0 && vacated.inert);
  check("after a keyboard snap the moved pane's textarea has focus", snapped.inputFocused && s.active === "TEXTAREA.term-input");
  check("the snap wrote the record once with the tab on the new side", puts - putsBeforeSnap === 1 && rec.open === true && rec[emptySide] !== null && rec[survivorSide] === null && rec.selected === emptySide);
  await click(`.wt-tab:not(.wt-tab-active)`);
  await sleep(SETTLE_MS / 2);
  left = await pane("left");
  right = await pane("right");
  check("clicking an ordinary chip fills the empty pane", left.rows > 0 && right.rows > 0);

  console.log("=== 7. the divider: its Tab neighbours, arrow keys, Home and End ===");
  await ev(`document.querySelector('.wt-split-handle').focus(); 'ok'`);
  s = await shell();
  check("the divider takes focus without moving the selection", s.handleFocused && s.handleFaces === (left.selected ? "left" : "right"));
  await key("Tab", { vk: VK.Tab });
  const afterTab = await pane("right");
  await ev(`document.querySelector('.wt-split-handle').focus(); 'ok'`);
  await key("Tab", { vk: VK.Tab, modifiers: 8 });
  const afterShiftTab = await pane("left");
  console.log(JSON.stringify({ tabLandsOnRightInput: afterTab.inputFocused, shiftTabLandsOnLeftInput: afterShiftTab.inputFocused }));
  check("Tab from the divider lands on the right pane's input, Shift+Tab on the left's", afterTab.inputFocused && afterShiftTab.inputFocused);
  await ev(`document.querySelector('.wt-split-handle').focus(); 'ok'`);
  const putsBeforeKeys = puts;
  const leftWidth = async () => (await pane("left")).width;
  const widths = [];
  for (let i = 0; i < 5; i++) await key("ArrowLeft", { vk: VK.ArrowLeft, holdDown: true, repeat: i > 0 });
  await keyUp("ArrowLeft", { vk: VK.ArrowLeft });
  await sleep(400);
  widths.push(await leftWidth());
  const putsAfterHold = puts - putsBeforeKeys;
  await key("Home", { vk: VK.Home });
  await sleep(400);
  widths.push(await leftWidth());
  const putsAfterHome = puts - putsBeforeKeys;
  await key("ArrowLeft", { vk: VK.ArrowLeft });
  await sleep(400);
  widths.push(await leftWidth());
  const putsAfterRefused = puts - putsBeforeKeys;
  await key("End", { vk: VK.End });
  await sleep(400);
  widths.push(await leftWidth());
  const putsAfterEnd = puts - putsBeforeKeys;
  s = await shell();
  console.log(JSON.stringify({ widths, valueNow: s.handleValueNow, puts: [putsAfterHold, putsAfterHome, putsAfterRefused, putsAfterEnd], panes: [(await pane("left")).rows > 0, (await pane("right")).rows > 0] }));
  const near = (a, b) => Math.abs(a - b) <= 1;
  check("5 held ArrowLeft presses move the left edge 80px (495 -> 415)", near(widths[0], 415));
  check("Home puts the left pane at the 360px minimum", near(widths[1], MIN_PANE_PX));
  check("a further ArrowLeft at the minimum is refused, not a close", near(widths[2], MIN_PANE_PX) && (await pane("left")).rows > 0);
  check("End puts the right pane at the minimum (left 630)", near(widths[3], WIDTH - 10 - MIN_PANE_PX));
  check("the record is written once per key release and not for the refused key", putsAfterHold === 1 && putsAfterHome === 2 && putsAfterRefused === 2 && putsAfterEnd === 3);
  check("both panes are still shown after the keyboard nudges", (await pane("left")).rows > 0 && (await pane("right")).rows > 0);

  console.log("=== 8. the divider: a 200px drag with 20 moves over 400ms ===");
  await ev(`document.querySelector('.wt-split-handle').focus(); 'ok'`);
  await key("Home", { vk: VK.Home });
  await sleep(400);
  await ev(`document.querySelector('.wt-split-pane.wt-pane-selected .term-input').focus(); 'ok'`);
  for (const sk of sessionSockets()) sk.sentResize = 0;
  let h = await ev(rectOf(".wt-split-handle"));
  const startLeft = await leftWidth();
  const dragStarted = Date.now();
  await mouse("mousePressed", h.x, h.y);
  for (let i = 1; i <= DRAG_STEPS; i++) {
    await mouse("mouseMoved", h.x + (200 * i) / DRAG_STEPS, h.y);
    await sleep(DRAG_MS / DRAG_STEPS);
  }
  const midWidth = await leftWidth();
  const heldClass = await ev(`document.querySelector('.wt-split-handle').classList.contains('wt-handle-held')`);
  const sentBeforeRelease = sessionSockets().filter((x) => x.open).map((x) => x.sentResize);
  const dragElapsed = Date.now() - dragStarted;
  await mouse("mouseReleased", h.x + 200, h.y);
  await sleep(400);
  const sentAfterRelease = sessionSockets().filter((x) => x.open).map((x) => x.sentResize);
  const endWidth = await leftWidth();
  // The leading send plus one per elapsed 100ms; the round trips stretch the
  // nominal 400ms, so the bound follows the measured duration.
  const maxBefore = Math.ceil(dragElapsed / RESIZE_INTERVAL_MS) + 1;
  console.log(JSON.stringify({ startLeft, midWidth, endWidth, heldClass, dragElapsed, maxBefore, sentBeforeRelease, sentAfterRelease, ratio: (await shell()).ratio }));
  check("the panes follow the pointer during the drag (handle held)", heldClass === true && midWidth > startLeft + 150);
  check("resize frames per socket before release stay within one per 100ms plus the leading send", sentBeforeRelease.every((n) => n >= 1 && n <= maxBefore));
  check("at most one more resize frame per socket on release", sentAfterRelease.every((n, i) => n - sentBeforeRelease[i] <= 1));
  check("the released ratio is committed (left grew by ~200px)", near(endWidth, startLeft + 200) || Math.abs(endWidth - (startLeft + 200)) <= 8);

  console.log("=== 9. the divider: squeezing a pane under 360px closes it on release ===");
  const chipsBefore = (await shell()).chips.length;
  const recBeforeSqueeze = await layout();
  h = await ev(rectOf(".wt-split-handle"));
  await mouse("mousePressed", h.x, h.y);
  const targetX = 300;
  for (let i = 1; i <= 10; i++) {
    await mouse("mouseMoved", h.x + ((targetX - h.x) * i) / 10, h.y);
    await sleep(30);
  }
  const closingClass = await ev(`document.querySelector('.wt-split-pane.wt-side-left').classList.contains('wt-pane-closing')`);
  await mouse("mouseReleased", targetX, h.y);
  await sleep(STEP_MS);
  s = await shell();
  // A close re-sides the survivor to the left, so the pane that was on the right
  // is now the left one and the squeezed pane sits hidden on the right.
  left = await pane("left");
  right = await pane("right");
  const recAfterSqueeze = await layout();
  console.log(JSON.stringify({ closingClass, open: s.open, chips: s.chips.length, survivorRows: left.rows, survivorWidth: left.width, survivorSelected: left.selected, squeezedRows: right.rows, squeezedHidden: right.hidden, before: recBeforeSqueeze, after: recAfterSqueeze }));
  check("dragging the left pane under 360px dims it (wt-pane-closing)", closingClass === true);
  check("releasing there closes the squeezed pane; the other tab survives and fills the view", !s.open && recAfterSqueeze.open === false && recAfterSqueeze.left === recBeforeSqueeze.right && left.rows > 0 && left.selected && left.width === WIDTH && right.rows === 0 && right.hidden);
  check("the closed pane's tab stays open in the row", s.chips.length === chipsBefore);

  console.log("=== 10. the minimum across a resize: 800px clamps without a write, 730px meets at 0.5, 720px collapses ===");
  await click(".wt-tab-split");
  await sleep(STEP_MS);
  await click(".wt-tab:not(.wt-tab-active)");
  await sleep(SETTLE_MS / 2);
  left = await pane("left");
  right = await pane("right");
  check("both panes shown again before the resizes", left.rows > 0 && right.rows > 0);
  await ev(`document.querySelector('.wt-split-handle').focus(); 'ok'`);
  for (let i = 0; i < 5; i++) await key("ArrowLeft", { vk: VK.ArrowLeft, holdDown: true, repeat: i > 0 });
  await keyUp("ArrowLeft", { vk: VK.ArrowLeft });
  await sleep(400);
  await ev(`document.querySelector('.wt-split-pane.wt-pane-selected .term-input').focus(); 'ok'`);
  const committed = await layout();
  const committedLeft = await leftWidth();
  const putsBeforeResize = puts;
  const ratioOf = (x) => Number(x.ratio);
  await setSize(800, HEIGHT);
  await sleep(STEP_MS);
  s = await shell();
  left = await pane("left");
  right = await pane("right");
  const recAt800 = await layout();
  console.log(JSON.stringify({ committedHandle: committed.handle, committedLeft, at800: { ratio: s.ratio, valueNow: s.handleValueNow, valueMin: s.handleValueMin, valueMax: s.handleValueMax, widths: [left.width, right.width], open: s.open, collapsed: s.collapsed, btnHidden: s.btnHidden }, puts: puts - putsBeforeResize, recordHandle: recAt800.handle }));
  check("a committed 415/990 is under the 800px minimum (the clamp has something to do)", near(committedLeft, 415) && committed.handle * (800 - 10) < MIN_PANE_PX);
  check("at 800px the split stays open and uncollapsed with its handle and button", s.open && !s.collapsed && s.handleDisplay === "block" && s.btnHidden === false);
  check("at 800px both panes keep the 360px minimum (left 360, right 430)", left.width >= MIN_PANE_PX && right.width >= MIN_PANE_PX && near(left.width, MIN_PANE_PX) && near(right.width, 800 - 10 - MIN_PANE_PX));
  check("the effective ratio reads 360/790 and the separator's bounds 46..54", Math.abs(ratioOf(s) - MIN_PANE_PX / 790) < 0.001 && s.handleValueNow === "46" && s.handleValueMin === "46" && s.handleValueMax === "54");
  check("the clamp writes no record: no PUT, the record keeps the committed handle", puts === putsBeforeResize && recAt800.handle === committed.handle);
  await setSize(730, HEIGHT);
  await sleep(STEP_MS);
  s = await shell();
  left = await pane("left");
  right = await pane("right");
  console.log(JSON.stringify({ at730: { ratio: s.ratio, valueNow: s.handleValueNow, widths: [left.width, right.width], open: s.open, collapsed: s.collapsed, btnHidden: s.btnHidden }, puts: puts - putsBeforeResize }));
  check("at exactly 730px the bounds meet at 0.5, both panes 360, split not collapsed, button shown", s.open && !s.collapsed && s.btnHidden === false && ratioOf(s) === 0.5 && near(left.width, MIN_PANE_PX) && near(right.width, MIN_PANE_PX) && puts === putsBeforeResize);
  await setSize(WIDTH, HEIGHT);
  await sleep(STEP_MS);
  s = await shell();
  left = await pane("left");
  const recAt1000 = await layout();
  console.log(JSON.stringify({ at1000: { ratio: s.ratio, valueNow: s.handleValueNow, leftWidth: left.width }, puts: puts - putsBeforeResize, recordHandle: recAt1000.handle }));
  check("back at 1000px the committed ratio is restored (left 415) without a write", Math.abs(ratioOf(s) - committed.handle) < 0.001 && near(left.width, committedLeft) && puts === putsBeforeResize && recAt1000.handle === committed.handle);
  await setSize(720, HEIGHT);
  await sleep(STEP_MS);
  s = await shell();
  left = await pane("left");
  right = await pane("right");
  const hiddenSide = left.selected ? "right" : "left";
  const hiddenBefore = hiddenSide === "left" ? left : right;
  console.log(JSON.stringify({ open: s.open, collapsed: s.collapsed, handleDisplay: s.handleDisplay, btnHidden: s.btnHidden, hiddenVisibility: hiddenBefore.visibility, hiddenInert: hiddenBefore.inert, sockets: sessionSockets().filter((x) => x.open).length }));
  check("at 720px the split collapses: open, collapsed, handle and split button hidden", s.open && s.collapsed && s.handleDisplay === "none" && s.btnHidden === true);
  check("the unselected pane is visibility:hidden and inert, its socket kept", hiddenBefore.visibility === "hidden" && hiddenBefore.inert && sessionSockets().filter((x) => x.open).length === 2);
  // A rAF interval of two periods is one missed vsync.
  const frameWindow = async () => {
    await ev(`window.__frames = new Promise(res => { const t = []; let last = performance.now(); const t0 = last; const tick = (now) => { t.push(now - last); last = now; if (now - t0 < 400) requestAnimationFrame(tick); else res(t.slice(1)); }; requestAnimationFrame(tick); }); 'armed'`);
    await sleep(100);
    await setSize(700, HEIGHT);
    await sleep(120);
    await setSize(720, HEIGHT);
    const intervals = await ev(`window.__frames`, true);
    await sleep(400);
    const sorted = [...intervals].sort((a, b) => a - b);
    const median = sorted[Math.floor(sorted.length / 2)] ?? 0;
    const max = Math.max(...intervals);
    return { frames: intervals.length, medianMs: Math.round(median * 10) / 10, maxMs: Math.round(max * 10) / 10, dropped: intervals.filter((x) => x >= 2 * median).length };
  };
  const windows = [];
  for (let i = 0; i < 3; i++) windows.push(await frameWindow());
  const hiddenAfter = await pane(hiddenSide);
  console.log(JSON.stringify({ windows, hiddenMaxAbs: [hiddenBefore.maxAbs, hiddenAfter.maxAbs] }));
  check("the hidden pane still receives frames while collapsed (its output advanced)", hiddenAfter.maxAbs > hiddenBefore.maxAbs);
  check("every resize window with the hidden pane painting has 10+ frames and zero dropped", windows.every((w) => w.frames >= 10 && w.dropped === 0));
  await setSize(WIDTH, HEIGHT);
  await sleep(STEP_MS);
  s = await shell();
  check("back at 1000px the split is restored with its handle", s.open && !s.collapsed && s.handleDisplay === "block" && s.btnHidden === false);
  check("both panes are still shown after the collapse", (await pane("left")).rows > 0 && (await pane("right")).rows > 0);

  console.log("=== 11. the record survives a reload ===");
  const before = await layout();
  const beforeChips = (await shell()).chips.map((c) => c.label);
  await rpc(ws, ++id, "Page.reload", {});
  await sleep(SETTLE_MS);
  const after = await layout();
  s = await shell();
  left = await pane("left");
  right = await pane("right");
  console.log(JSON.stringify({ before, after, ratio: s.ratio, chips: s.chips.map((c) => c.label), leftRows: left.rows, rightRows: right.rows }));
  check("after a reload the same sessions sit in the same panes with the same handle", s.open && after.left === before.left && after.right === before.right && after.handle === before.handle && after.selected === before.selected && left.rows > 0 && right.rows > 0);
  check("the tab row lists the same tabs after the reload", JSON.stringify(s.chips.map((c) => c.label)) === JSON.stringify(beforeChips));
  check("the handle position is restored into the ratio variable", Math.abs(Number(s.ratio) - before.handle) < 0.001);

  console.log("=== 12. closing the split releases exactly one socket, cleanly ===");
  const openBefore = sessionSockets().filter((x) => x.open);
  const wsLog = () => ev(`JSON.stringify(window.__wsLog.filter(e => e.url.includes('session=')))`).then(JSON.parse);
  const closedBeforeClose = (await wsLog()).filter((e) => e.event !== null).length;
  const recBeforeClose = await layout();
  await click(".wt-tab-split");
  await sleep(STEP_MS);
  const closedNow = openBefore.filter((x) => !x.open);
  const closedEntries = (await wsLog()).filter((e) => e.event !== null);
  s = await shell();
  const survivor = await pane("left");
  const recAfterClose = await layout();
  console.log(JSON.stringify({ open: s.open, closedSockets: closedNow.length, closes: closedEntries.slice(closedBeforeClose), openInPage: (await wsLog()).filter((e) => e.event === null).length, survivorWidth: survivor.width, before: recBeforeClose, after: recAfterClose }));
  // forgetSession releases the socket with a bare close(); a close frame with no
  // status code is reported as 1005 (RFC 6455 section 7.1.5).
  const NO_STATUS_RECEIVED = 1005;
  const openInPage = (await wsLog()).filter((e) => e.event === null).length;
  check("closing the split closes exactly one session socket", closedNow.length === 1 && closedEntries.length - closedBeforeClose === 1 && openInPage === 1);
  check("the released socket was closed by the page with a bare close() (event code 1005)", closedEntries.slice(closedBeforeClose).every((e) => e.closedByPage && e.requested === null && e.event === NO_STATUS_RECEIVED));
  check("the selected pane's tab survives on the left and fills the full width", !s.open && recAfterClose.open === false && recAfterClose.left === recBeforeClose[recBeforeClose.selected] && recAfterClose.right === null && survivor.rows > 0 && survivor.width === WIDTH);

  if (SHOT_PATH) {
    await click(".wt-tab-split");
    await sleep(STEP_MS);
    await click(".wt-tab:not(.wt-tab-active)");
    await sleep(STEP_MS);
    const shot = await rpc(ws, ++id, "Page.captureScreenshot", { format: "png" });
    require("node:fs").writeFileSync(SHOT_PATH, Buffer.from(shot.data, "base64"));
    console.log("screenshot:", SHOT_PATH);
  }

  console.log("=== console errors ===");
  console.log(errors.length ? errors.slice(0, 10).join("\n") : "(none)");
  check("no console errors", errors.length === 0);

  const ok = Object.values(checks).every(Boolean);
  console.log(ok ? "\nSPLIT VERIFY: PASS" : "\nSPLIT VERIFY: FAIL");
  ws.close();
  await fetch(`${CDP}/json/close/${target.id}`).catch(() => {});
  process.exit(ok ? 0 : 1);
})().catch(async (e) => {
  console.error("VERIFY ERROR:", e.message);
  // A tab left behind keeps its sockets on the server and skews the next run's counts.
  if (targetId) await fetch(`${CDP}/json/close/${targetId}`).catch(() => {});
  process.exit(2);
});
