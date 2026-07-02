// Native MJPEG support: parse a multipart ffmpeg "-f mpjpeg" byte stream into
// per-frame JPEG blob URLs.
//
// Why this exists: the Android WebView silently blocks a cross-protocol
// <img src="http://..."> load (mixed content) even with
// MIXED_CONTENT_ALWAYS_ALLOW — but fetch() to the same URL works. So we read
// the stream ourselves, split it into JPEG frames, and hand each to <img> as a
// blob: URL (same-origin, not blocked).
//
// The framing (verified against ffmpeg's mpjpeg muxer, which the companion API
// uses) is, per frame:
//     --<boundary>\r\n
//     Content-type: image/jpeg\r\n
//     Content-length: <N>\r\n
//     \r\n
//     <N bytes of JPEG>\r\n
// The response Content-Type varies (ffmpeg's own HTTP listener reports
// application/octet-stream; the companion API reports multipart/x-mixed-replace),
// so we parse the in-body "Content-length: N" framing directly rather than
// trusting the response header or a declared boundary.

const CRLFCRLF = [0x0d, 0x0a, 0x0d, 0x0a];

function indexOfSeq(buf, seq, from = 0) {
  outer: for (let i = from; i <= buf.length - seq.length; i++) {
    for (let j = 0; j < seq.length; j++) {
      if (buf[i + j] !== seq[j]) continue outer;
    }
    return i;
  }
  return -1;
}

function concat(a, b) {
  if (a.length === 0) return b;
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}

// createFrameParser returns a stateful parser: push(chunk) accepts a Uint8Array
// and returns an array of complete JPEG frames (Uint8Array) found so far. Pure
// (no DOM), so it is unit-testable off-device. Throws if the stream doesn't look
// like mpjpeg framing (guards against unbounded buffering).
export function createFrameParser() {
  const decoder = new TextDecoder();
  let buf = new Uint8Array(0);
  let need = 0; // bytes remaining for the current frame (0 = parsing headers)

  return function push(chunk) {
    if (chunk && chunk.length) buf = concat(buf, chunk);
    const frames = [];
    for (;;) {
      if (need > 0) {
        if (buf.length < need) break;
        frames.push(buf.slice(0, need));
        buf = buf.slice(need);
        need = 0;
        continue;
      }
      const hdrEnd = indexOfSeq(buf, CRLFCRLF);
      if (hdrEnd < 0) {
        if (buf.length > 1 << 20) throw new Error("MJPEG framing not found");
        break;
      }
      const header = decoder.decode(buf.slice(0, hdrEnd));
      const m = /content-length:\s*(\d+)/i.exec(header);
      buf = buf.slice(hdrEnd + 4);
      if (m) need = parseInt(m[1], 10);
      // else: a boundary-only block with no length yet — keep scanning.
    }
    return frames;
  };
}

// startMjpegStream reads `url` and invokes onFrame(blobUrl) for each JPEG frame.
// The caller owns each blobUrl and is responsible for revoking it (typically the
// previous one, once the next has been applied). onError(err) fires on a fatal
// stream error; onEnd() fires when the server closes the stream. Returns a
// stop() that aborts the fetch and suppresses further callbacks.
export function startMjpegStream(url, { onFrame, onError, onEnd } = {}) {
  const controller = new AbortController();
  let stopped = false;

  (async () => {
    try {
      const res = await fetch(url, { signal: controller.signal, cache: "no-store" });
      if (!res.ok || !res.body) throw new Error(`stream HTTP ${res.status}`);
      const reader = res.body.getReader();
      const push = createFrameParser();

      while (!stopped) {
        const { done, value } = await reader.read();
        if (done) break;
        for (const frame of push(value)) {
          if (stopped) break;
          if (onFrame) {
            onFrame(URL.createObjectURL(new Blob([frame], { type: "image/jpeg" })));
          }
        }
      }
      if (!stopped && onEnd) onEnd();
    } catch (err) {
      if (!stopped && err.name !== "AbortError" && onError) onError(err);
    }
  })();

  return function stop() {
    stopped = true;
    controller.abort();
  };
}
