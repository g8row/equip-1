import { isNative } from "./native";

// Cap how large a file we'll round-trip through JS memory as base64 before
// handing it to the native Share sheet. Filesystem.writeFile needs the whole
// payload as a base64 string, which is ~33% bigger than the source and held
// in memory twice (blob + string) — fine for a few hundred MB, not for a
// multi-GB DV capture. The existing browser download link streams straight
// to disk via Android's download manager regardless of size, so large files
// still have a working path, just not the native share sheet.
export const SHARE_MAX_BYTES = 150 * 1024 * 1024;

export function canShareNative(sizeBytes) {
  return isNative() && sizeBytes != null && sizeBytes <= SHARE_MAX_BYTES;
}

function blobToBase64(blob) {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onloadend = () => {
      // reader.result is "data:<mime>;base64,<data>" — Filesystem wants just the data.
      const result = reader.result || "";
      const comma = result.indexOf(",");
      resolve(comma >= 0 ? result.slice(comma + 1) : result);
    };
    reader.onerror = () => reject(reader.error || new Error("Could not read file"));
    reader.readAsDataURL(blob);
  });
}

/** Downloads a file from the device API and opens the native share sheet. */
export async function shareFile(url, name) {
  const [{ Filesystem, Directory }, { Share }] = await Promise.all([
    import("@capacitor/filesystem"),
    import("@capacitor/share"),
  ]);

  const res = await fetch(url);
  if (!res.ok) throw new Error(`Download failed: HTTP ${res.status}`);
  const blob = await res.blob();
  const base64 = await blobToBase64(blob);

  await Filesystem.writeFile({
    path: name,
    data: base64,
    directory: Directory.Cache,
  });
  const { uri } = await Filesystem.getUri({ path: name, directory: Directory.Cache });

  await Share.share({
    title: name,
    url: uri,
    dialogTitle: `Share ${name}`,
  });
}
