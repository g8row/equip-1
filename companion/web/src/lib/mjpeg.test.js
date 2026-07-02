import { describe, it, expect } from "vitest";
import { createFrameParser } from "./mjpeg";

const enc = new TextEncoder();

function concat(arrays) {
  const total = arrays.reduce((n, a) => n + a.length, 0);
  const out = new Uint8Array(total);
  let off = 0;
  for (const a of arrays) {
    out.set(a, off);
    off += a.length;
  }
  return out;
}

// Wrap raw frames in the ffmpeg mpjpeg framing this parser targets:
//   --ffmpeg\r\nContent-type: image/jpeg\r\nContent-length: N\r\n\r\n<frame>\r\n
// (lowercase "Content-length" matches ffmpeg's actual output).
function buildMultipart(frames) {
  const parts = [];
  for (const f of frames) {
    parts.push(enc.encode(`--ffmpeg\r\nContent-type: image/jpeg\r\nContent-length: ${f.length}\r\n\r\n`));
    parts.push(f);
    parts.push(enc.encode("\r\n"));
  }
  return concat(parts);
}

// Distinct, known-length frames so we can assert byte-exact extraction.
const frames = [
  Uint8Array.from({ length: 12 }, (_, i) => i),
  Uint8Array.from({ length: 300 }, (_, i) => (i * 7) % 256),
  Uint8Array.from({ length: 1 }, () => 0xff),
  Uint8Array.from({ length: 64 }, (_, i) => 255 - (i % 256)),
];

describe("createFrameParser", () => {
  it("extracts all frames from a single chunk, byte-exact", () => {
    const parse = createFrameParser();
    const got = parse(buildMultipart(frames));
    expect(got.length).toBe(frames.length);
    got.forEach((f, i) => expect(Array.from(f)).toEqual(Array.from(frames[i])));
  });

  it("is chunking-invariant: identical output fed one byte at a time", () => {
    const buf = buildMultipart(frames);
    const parse = createFrameParser();
    const got = [];
    for (let i = 0; i < buf.length; i++) got.push(...parse(buf.subarray(i, i + 1)));
    expect(got.length).toBe(frames.length);
    got.forEach((f, i) => expect(Array.from(f)).toEqual(Array.from(frames[i])));
  });

  it("holds a partial trailing frame until the rest arrives", () => {
    const buf = buildMultipart(frames.slice(0, 1));
    const parse = createFrameParser();
    const cut = buf.length - 3; // split mid-frame
    expect(parse(buf.subarray(0, cut))).toHaveLength(0);
    const rest = parse(buf.subarray(cut));
    expect(rest).toHaveLength(1);
    expect(Array.from(rest[0])).toEqual(Array.from(frames[0]));
  });

  it("throws if the stream never yields valid framing (guards unbounded buffering)", () => {
    const parse = createFrameParser();
    const junk = new Uint8Array(2 * 1024 * 1024); // > 1 MiB with no header terminator
    expect(() => parse(junk)).toThrow();
  });
});
