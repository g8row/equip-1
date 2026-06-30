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
  },
  plugins: {
    BleClient: {
      displayStrings: {
        scanning: 'Scanning for equip-1...',
        cancel: 'Cancel',
        availableDevices: 'Nearby equip-1 devices',
        noDeviceFound: 'No equip-1 device found',
      }
    }
  }
};

export default config;
