import { createTerminal, localScrollbackStorage } from "@cplieger/web-terminal-ui";
import { presetTabbed } from "@cplieger/web-terminal-ui/presets";

// static_persist.go stamps this marker from PERSIST_SCROLLBACK at startup.
const persist =
  document.querySelector('meta[name="wt-persist-scrollback"]')?.getAttribute("content") === "on";

// The root goes in as a SELECTOR and the preset as an ARROW so both resolve
// inside createTerminal's failure boundary: a missing #terminal or a throwing
// preset renders the library's "Terminal failed to start" panel instead of a
// spinner that never stops.
createTerminal("#terminal", {
  // attentionIcons promises that static/ serves favicon-{input,done,alert} in all
  // three icon formats (main_test.go asserts it against the embedded tree).
  features: () => presetTabbed({ attentionIcons: true }),
  split: true,
  loading: document.getElementById("loading"),
  // Omitted rather than passed as undefined when off: the option's presence is
  // what enables the feature.
  ...(persist ? { persistScrollback: localScrollbackStorage() } : {}),
});
