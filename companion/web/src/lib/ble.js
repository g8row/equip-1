// BLE service/characteristic UUIDs
export const SERVICE_UUID = 'e2710000-b5a3-f393-e0a9-e50e24dcca9e';
export const CHAR_STATUS = 'e2710001-b5a3-f393-e0a9-e50e24dcca9e';
export const CHAR_WIFI_CREDS = 'e2710002-b5a3-f393-e0a9-e50e24dcca9e';
export const CHAR_AP_CTRL = 'e2710003-b5a3-f393-e0a9-e50e24dcca9e';
export const CHAR_RECORD = 'e2710004-b5a3-f393-e0a9-e50e24dcca9e';
export const CHAR_NET_RESULT = 'e2710005-b5a3-f393-e0a9-e50e24dcca9e';
export const CHAR_WIFI_SCAN = 'e2710006-b5a3-f393-e0a9-e50e24dcca9e';

// Lazy-import BleClient to avoid bundling it in web builds
async function getBle() {
  const { BleClient } = await import('@capacitor-community/bluetooth-le');
  return BleClient;
}

export async function bleInit() {
  const BleClient = await getBle();
  await BleClient.initialize({ androidNeverForLocation: true });
}

// Device advertised name. Matched as a fallback because service-UUID scan
// filtering is unreliable on this hardware: the AIC8800 can't do BT5 extended
// advertising, so BlueZ's own connectable advertisement (name + standard 16-bit
// UUIDs) competes on-air with companion-net's legacy HCI advertisement (which
// carries our 128-bit service UUID). A phone doing a UUID-filtered scan often
// only catches BlueZ's packet and never matches. The device *name* is present
// in every packet, so we scan unfiltered and match on name OR service UUID.
const DEVICE_NAME = 'equip-1';

export async function scanForDevice(onFound, timeoutMs = 15000) {
  const BleClient = await getBle();
  return new Promise((resolve, reject) => {
    let found = null;

    const matches = (result) => {
      const name = result.device?.name || result.localName || '';
      if (name === DEVICE_NAME) return true;
      const uuids = result.device?.uuids || result.uuids || [];
      return uuids.some((u) => String(u).toLowerCase() === SERVICE_UUID);
    };

    const accept = (result) => {
      if (found) return;
      found = result.device;
      clearTimeout(timer);
      BleClient.stopLEScan().catch(() => {});
      if (onFound) onFound(result.device);
      resolve(result.device);
    };

    const timer = setTimeout(() => {
      BleClient.stopLEScan().catch(() => {});
      if (!found) reject(new Error('No equip-1 device found nearby'));
    }, timeoutMs);

    // Unfiltered scan (no `services`): on this hardware a UUID filter misses the
    // packet that actually carries our UUID. We filter in the callback instead.
    BleClient.requestLEScan({ allowDuplicates: true }, (result) => {
      if (matches(result)) accept(result);
    }).catch(reject);
  });
}

export async function connect(deviceId, onDisconnect) {
  const BleClient = await getBle();
  await BleClient.connect(deviceId, onDisconnect);
  // Brief stabilisation delay — AIC8800 GATT needs ~200ms after LE connection
  // before characteristics are reliably readable.
  await new Promise(r => setTimeout(r, 250));
}

export async function readStatus(deviceId) {
  const BleClient = await getBle();
  const data = await BleClient.read(deviceId, SERVICE_UUID, CHAR_STATUS);
  return JSON.parse(new TextDecoder().decode(data.buffer));
}

export async function writeWifiCreds(deviceId, ssid, psk) {
  const BleClient = await getBle();
  const payload = JSON.stringify({ ssid, psk });
  const data = new TextEncoder().encode(payload);
  await BleClient.writeWithoutResponse(deviceId, SERVICE_UUID, CHAR_WIFI_CREDS, {
    buffer: data.buffer
  });
}

export async function writeApControl(deviceId, enable) {
  const BleClient = await getBle();
  const data = new Uint8Array([enable ? 1 : 0]);
  await BleClient.writeWithoutResponse(deviceId, SERVICE_UUID, CHAR_AP_CTRL, {
    buffer: data.buffer
  });
}

export async function subscribeNetResult(deviceId, callback) {
  const BleClient = await getBle();
  await BleClient.startNotifications(deviceId, SERVICE_UUID, CHAR_NET_RESULT, (data) => {
    try {
      callback(JSON.parse(new TextDecoder().decode(data.buffer)));
    } catch {}
  });
}

export async function readWifiScan(deviceId) {
  const BleClient = await getBle();
  const data = await BleClient.read(deviceId, SERVICE_UUID, CHAR_WIFI_SCAN);
  return JSON.parse(new TextDecoder().decode(data.buffer));
}

export async function disconnect(deviceId) {
  const BleClient = await getBle();
  await BleClient.disconnect(deviceId).catch(() => {});
}
