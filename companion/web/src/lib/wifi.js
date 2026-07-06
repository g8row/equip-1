// Phone-side WiFi control for the device-hosted AP handoff.
//
// The equip-1 device can host its own WiFi hotspot (single-radio AIC8800: it
// is EITHER an AP or a station, never both). When there's no shared network,
// the phone joins that hotspot and talks to the device at the AP gateway.
//
// Android (API 29+) forbids silently joining a network. @capgo/capacitor-wifi
// wraps WifiNetworkSpecifier, which shows a one-tap system dialog and — with
// autoRouteTraffic — binds this app's traffic to the AP via
// ConnectivityManager.bindProcessToNetwork() so requests to the AP gateway
// actually route over WiFi. That binding is what makes AP_GATEWAY reachable.

import { isNative } from "./native";

// WPA/WPA2 pre-shared keys are 8–63 characters; an empty key means an open
// network. A 1–7 character key is guaranteed to be rejected, so we gate the UI
// on it rather than letting the join fail. Returns a message, or null if OK.
export function pskError(psk) {
  if (!psk) return null; // open network
  if (psk.length < 8) return "WiFi password must be at least 8 characters.";
  if (psk.length > 63) return "WiFi password can be at most 63 characters.";
  return null;
}

// The SSID the device advertises for its own hotspot (its BLE name,
// "equip-1"; see internal/ble/bluez.go).
export const AP_SSID = "equip-1";
// The AP passphrase is NOT hardcoded here — it comes from the device over
// BLE (the `ap_pass` field on the status characteristic; single source of
// truth is internal/network/connman.go `APPassphrase`, surfaced onto the
// status JSON by internal/ble/api.go). Callers must pass it into
// joinDeviceAp().
// ConnMan brings the tether bridge up as 192.168.0.1/24 (its built-in
// default), so the device API lives here once the phone is on the AP.
export const AP_GATEWAY = "http://192.168.0.1:8000";

// NOTE: never return the CapacitorWifi plugin proxy as the resolved value of a
// promise/async function. The proxy forwards every property access (including
// `.then`) to native, so JS's thenable-adoption would call `CapacitorWifi.then`
// → "not implemented on android". Always destructure it from the awaited module
// namespace (safe, not thenable) and only await real method results below.

// joinDeviceAp connects the phone to the device hotspot and binds app traffic
// to it. `passphrase` must come from the device's status characteristic
// (`ap_pass`), read over BLE before this is called. Throws if the user
// declines the system dialog or the join fails.
export async function joinDeviceAp(passphrase) {
  if (!isNative()) {
    throw new Error("WiFi join is only available in the mobile app");
  }
  if (!passphrase) {
    throw new Error("Missing AP passphrase — read the device status over BLE first");
  }
  const { CapacitorWifi } = await import("@capgo/capacitor-wifi");
  await CapacitorWifi.connect({
    ssid: AP_SSID,
    password: passphrase,
    autoRouteTraffic: true, // route this app's traffic over the AP → reach AP_GATEWAY
  });
}

// leaveDeviceAp releases the AP binding so the phone falls back to its normal
// network. Best-effort — never throws.
export async function leaveDeviceAp() {
  if (!isNative()) return;
  try {
    const { CapacitorWifi } = await import("@capgo/capacitor-wifi");
    await CapacitorWifi.disconnect();
  } catch {
    // Already disconnected, or the OS dropped it — nothing to do.
  }
}

// currentSsid returns the SSID the phone is currently on, or null if unknown.
export async function currentSsid() {
  if (!isNative()) return null;
  try {
    const { CapacitorWifi } = await import("@capgo/capacitor-wifi");
    const { ssid } = await CapacitorWifi.getSsid();
    return ssid || null;
  } catch {
    return null;
  }
}
