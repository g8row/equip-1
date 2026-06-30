import { isNative } from "./native";

// Lazy-import to avoid bundling native plugin code into the web build,
// mirroring the pattern in lib/ble.js. All calls are fire-and-forget and
// silently no-op on web or if the plugin is unavailable for any reason —
// haptics are a nicety, never something a flow should block or fail on.

async function getHaptics() {
  if (!isNative()) return null;
  try {
    const mod = await import("@capacitor/haptics");
    return mod;
  } catch {
    return null;
  }
}

export async function hapticImpact(style = "Medium") {
  const haptics = await getHaptics();
  if (!haptics) return;
  try {
    await haptics.Haptics.impact({ style: haptics.ImpactStyle[style] });
  } catch {
    // ignore
  }
}

export async function hapticNotification(type = "Success") {
  const haptics = await getHaptics();
  if (!haptics) return;
  try {
    await haptics.Haptics.notification({ type: haptics.NotificationType[type] });
  } catch {
    // ignore
  }
}
