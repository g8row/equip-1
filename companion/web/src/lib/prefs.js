import { isNative } from "./native";

// Persisted key/value storage for things that must survive app restarts
// (currently: the paired device's address). Native: @capacitor/preferences —
// backed by SharedPreferences/UserDefaults, which (unlike the WebView's
// localStorage) Android won't clear out from under us. Web build: localStorage.
//
// IMPORTANT: never `return` the Capacitor plugin proxy (Preferences) as the
// resolved value of an async function. Resolving a promise with the proxy makes
// JS adopt it as a thenable and invoke `proxy.then()`, which the native bridge
// rejects ("not implemented on android") AND leaves the awaiting promise
// pending forever — silently wedging whatever awaited it. Destructure
// Preferences from the awaited module namespace *inline* and only await its
// real methods (.get/.set), which return ordinary promises.

export async function getPref(key) {
  if (isNative()) {
    try {
      const { Preferences } = await import("@capacitor/preferences");
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
      const { Preferences } = await import("@capacitor/preferences");
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
