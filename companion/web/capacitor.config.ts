import { CapacitorConfig } from '@capacitor/cli';

const config: CapacitorConfig = {
  appId: 'com.equip1.companion',
  appName: 'equip-1',
  webDir: 'dist',
  android: {
    // MIXED_CONTENT_ALWAYS_ALLOW (0): lets the https://localhost WebView bundle
    // fetch plain HTTP from the device API on the local network.
    mixedContentMode: 0,
    buildOptions: {
      keystorePath: undefined,
      keystoreAlias: undefined,
    }
  }
  // Note: BleClient.displayStrings (native "requestDevice" picker dialog
  // copy) was removed here — lib/ble.js uses requestLEScan, never
  // requestDevice, so that config was never read.
};

export default config;
