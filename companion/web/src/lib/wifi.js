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

// Must match the device side: internal/ble/bluez.go `apPassphrase` and the
// SSID the device advertises (its BLE name, "equip-1").
export const AP_SSID = "equip-1";
export const AP_PASSPHRASE = "equip1device";
// ConnMan brings the tether bridge up as 192.168.0.1/24 (its built-in
// default), so the device API lives here once the phone is on the AP.
export const AP_GATEWAY = "http://192.168.0.1:8000";

// NOTE: never return the CapacitorWifi plugin proxy as the resolved value of a
// promise/async function. The proxy forwards every property access (including
// `.then`) to native, so JS's thenable-adoption would call `CapacitorWifi.then`
// → "not implemented on android". Always destructure it from the awaited module
// namespace (safe, not thenable) and only await real method results below.

// joinDeviceAp connects the phone to the device hotspot and binds app traffic
// to it. Throws if the user declines the system dialog or the join fails.
export async function joinDeviceAp() {
  if (!isNative()) {
    throw new Error("WiFi join is only available in the mobile app");
  }
  const { CapacitorWifi, NetworkSecurityType } = await import("@capgo/capacitor-wifi");
  // Use addNetwork (a persistent, *saved* network via WifiNetworkSuggestion),
  // NOT connect() (WifiNetworkSpecifier). A specifier network is "local-only":
  // no INTERNET capability, hidden from the status bar, never saved — and,
  // critically, Chromium's WebRTC refuses to use it, so WHEP gathers zero ICE
  // candidates and the live video stays black (confirmed live on-device). A
  // saved network shows the WiFi icon, persists, installs a normal on-link route
  // to 192.168.0.0/24 (so AP_GATEWAY stays reachable without the old
  // bindProcessToNetwork hack), and is enumerable by WebRTC. The device AP is
  // WPA3-SAE. addNetwork resolves once the OS add-network dialog is accepted;
  // the actual association takes a moment, so callers poll AP_GATEWAY.
  await CapacitorWifi.addNetwork({
    ssid: AP_SSID,
    password: AP_PASSPHRASE,
    securityType: NetworkSecurityType?.SAE ?? 4, // WPA3 Personal (SAE)
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

// phoneWifiIp returns the phone's own IPv4 on the current WiFi (e.g.
// "192.168.0.2"), or null. LAN auto-discovery needs this: on native the app is
// served from https://localhost, so window.location.hostname yields no usable
// subnet and discovery would only scan hardcoded guess ranges. Seeding it with
// the phone's real /24 makes discovery scan the network it's actually on.
export async function phoneWifiIp() {
  if (!isNative()) return null;
  try {
    const { CapacitorWifi } = await import("@capgo/capacitor-wifi");
    const { ipAddress } = await CapacitorWifi.getIpAddress();
    return /^\d+\.\d+\.\d+\.\d+$/.test(ipAddress || "") ? ipAddress : null;
  } catch {
    return null;
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
