let _native = null;
export function isNative() {
  if (_native === null) {
    try {
      _native = typeof window !== 'undefined' &&
                window.Capacitor?.isNativePlatform?.() === true;
    } catch {
      _native = false;
    }
  }
  return _native;
}
