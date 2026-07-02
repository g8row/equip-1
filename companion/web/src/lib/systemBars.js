// Android system-bar (status bar) appearance.
//
// The status bar has its own background that we set natively (web content
// can't paint it on this device). Verified live: overlaysWebView(true) +
// setBackgroundColor gives a solid-colored bar with the page content correctly
// offset below it via the safe-area insets — overlay(false) instead double-
// counted the top inset and pushed the header down. We keep the bar dark to
// match the canvas, with light icons, and tint it accent-red while recording as
// a glanceable system-level "REC" cue.
//
// Colors mirror web/src/components/ui/tokens.css (--bg, --accent) — keep in sync.

import { isNative } from "./native";

const BAR_BG = "#0b0b0b"; // --bg
const BAR_REC = "#cc1e1e"; // --accent

let _mod = null;
async function statusBar() {
  if (!_mod) _mod = await import("@capacitor/status-bar");
  return _mod;
}

export async function initSystemBars() {
  if (!isNative()) return;
  try {
    const { StatusBar, Style } = await statusBar();
    await StatusBar.setOverlaysWebView({ overlay: true });
    await StatusBar.setStyle({ style: Style.Dark }); // light icons on dark bar
    await StatusBar.setBackgroundColor({ color: BAR_BG });
  } catch {
    // Web / plugin unavailable — nothing to do.
  }
}

// Tint the status bar to signal recording state. No-op on web.
export async function setRecordingBar(recording) {
  if (!isNative()) return;
  try {
    const { StatusBar } = await statusBar();
    await StatusBar.setBackgroundColor({ color: recording ? BAR_REC : BAR_BG });
  } catch {
    // ignore
  }
}
