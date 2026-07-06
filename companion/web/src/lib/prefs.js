import { isNative } from "./native";

// Lazy-import to avoid bundling native plugin code into the web build,
// mirroring the pattern in lib/ble.js / lib/haptics.js.
async function getPreferences() {
  const { Preferences } = await import("@capacitor/preferences");
  return Preferences;
}

// Persisted key/value storage for things that must survive app restarts
// (currently: the paired device's address). Native: @capacitor/preferences
// (already a dependency, previously never imported) — it's backed by
// SharedPreferences/UserDefaults rather than the WebView's localStorage,
// which Android is free to clear independently. Web build: localStorage,
// unchanged.
export async function getPref(key) {
  if (isNative()) {
    try {
      const Preferences = await getPreferences();
      const { value } = await Preferences.get({ key });
      return value ?? null;
    } catch {
      // Plugin unavailable for some reason — fall through to localStorage
      // rather than losing the value entirely.
    }
  }
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

export async function setPref(key, value) {
  if (isNative()) {
    try {
      const Preferences = await getPreferences();
      await Preferences.set({ key, value });
      return;
    } catch {
      // fall through to localStorage
    }
  }
  try {
    localStorage.setItem(key, value);
  } catch {
    // best-effort — a full-storage web context just won't persist this
  }
}
