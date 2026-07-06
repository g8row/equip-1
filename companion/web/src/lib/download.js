// T4.11: native export path for full-size captures.
//
// share.js's shareFile() reads the whole response into memory as one base64
// string before handing it to Filesystem.writeFile — fine for the share
// sheet (capped at SHARE_MAX_BYTES) but not viable for a multi-GB DV
// capture. This streams the response body instead: each chunk off the
// fetch reader is base64-encoded and flushed to disk immediately via
// Filesystem.writeFile (first chunk, creating the file) / appendFile
// (subsequent chunks), so at most one ~1 MB chunk is ever held in JS memory.
//
// The plain `<a download>` path (Files.jsx) stays the primary export on web,
// where the browser's own download manager streams to disk without any of
// this — this module only matters inside the Capacitor WebView, which has
// no DownloadListener wired up and swallows that link for anything past a
// trivial size.

// Bytes per flush to disk. Small enough that the base64-inflated in-memory
// copy (~1.33x) is trivial, large enough that a multi-GB file doesn't turn
// into tens of thousands of plugin round-trips.
const CHUNK_BYTES = 1024 * 1024;

// btoa() only accepts a string, so a raw byte chunk has to become one first.
// String.fromCharCode.apply blows up past ~60-100k arguments on some engines,
// so build the string in sub-chunks instead of doing bytes.length in one call.
const STRING_CHUNK = 0x8000;

function bytesToBase64(bytes) {
  let binary = "";
  for (let i = 0; i < bytes.length; i += STRING_CHUNK) {
    binary += String.fromCharCode.apply(null, bytes.subarray(i, i + STRING_CHUNK));
  }
  return btoa(binary);
}

/**
 * Streams `url` to a file named `name` in Directory.Documents, in ~1 MB
 * chunks, without ever holding the full response in memory.
 *
 * Options:
 *   - onProgress(receivedBytes, totalBytes|null): called after each chunk
 *     is flushed to disk. totalBytes comes from the response's
 *     Content-Length header and is null if the server didn't send one.
 *   - signal: an AbortSignal; aborting cancels the underlying fetch and
 *     deletes the partial file.
 *
 * Resolves with the file's native `uri` (Filesystem.getUri) on completion.
 */
export async function downloadToDevice(url, name, { onProgress, signal } = {}) {
  const { Filesystem, Directory } = await import("@capacitor/filesystem");

  const res = await fetch(url, { signal });
  if (!res.ok) {
    throw new Error(`Download failed: HTTP ${res.status}`);
  }
  if (!res.body || !res.body.getReader) {
    throw new Error("Streaming download not supported on this platform");
  }

  const contentLength = Number(res.headers.get("content-length"));
  const totalBytes = Number.isFinite(contentLength) && contentLength > 0 ? contentLength : null;

  const reader = res.body.getReader();
  let received = 0;
  let wroteAny = false;
  // Buffer partial reads until we have a full ~1 MB chunk to flush — fetch
  // stream chunk sizes are not under our control and are typically smaller
  // than CHUNK_BYTES.
  let pending = [];
  let pendingBytes = 0;

  async function flush(force) {
    if (pendingBytes === 0 || (!force && pendingBytes < CHUNK_BYTES)) return;
    const combined = new Uint8Array(pendingBytes);
    let offset = 0;
    for (const part of pending) {
      combined.set(part, offset);
      offset += part.length;
    }
    pending = [];
    pendingBytes = 0;
    const data = bytesToBase64(combined);
    if (!wroteAny) {
      await Filesystem.writeFile({ path: name, data, directory: Directory.Documents });
      wroteAny = true;
    } else {
      await Filesystem.appendFile({ path: name, data, directory: Directory.Documents });
    }
  }

  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      if (value && value.length) {
        pending.push(value);
        pendingBytes += value.length;
        received += value.length;
        await flush(false);
        if (onProgress) onProgress(received, totalBytes);
      }
    }
    await flush(true);
    // A zero-byte capture is pathological but shouldn't leave no file at all.
    if (!wroteAny) {
      await Filesystem.writeFile({ path: name, data: "", directory: Directory.Documents });
    }
  } catch (err) {
    try {
      reader.cancel();
    } catch {
      // already closed/cancelled — ignore
    }
    if (wroteAny) {
      // Best-effort cleanup of the partial file; don't let a cleanup
      // failure mask the original (cancel/network) error.
      await Filesystem.deleteFile({ path: name, directory: Directory.Documents }).catch(() => {});
    }
    throw err;
  }

  const { uri } = await Filesystem.getUri({ path: name, directory: Directory.Documents });
  return uri;
}
