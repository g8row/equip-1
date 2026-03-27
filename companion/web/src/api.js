const DEFAULT_API_BASE = import.meta.env.VITE_API_BASE || "http://127.0.0.1:8000";

export function getDefaultApiBase() {
  return DEFAULT_API_BASE;
}

function parseIpv4Host(text) {
  const value = (text || "").trim();
  const match = value.match(/^(?:https?:\/\/)?(\d{1,3}(?:\.\d{1,3}){3})(?::\d+)?$/);
  if (!match) {
    return null;
  }
  const octets = match[1].split(".").map((part) => Number(part));
  if (octets.some((part) => Number.isNaN(part) || part < 0 || part > 255)) {
    return null;
  }
  return octets.join(".");
}

function hostToPrefix(host) {
  const ip = parseIpv4Host(host);
  if (!ip) {
    return null;
  }
  const parts = ip.split(".");
  return `${parts[0]}.${parts[1]}.${parts[2]}`;
}

function normalizeBase(base) {
  return (base || "").trim().replace(/\/+$/, "");
}

async function request(base, path, options = {}) {
  const safeBase = normalizeBase(base) || DEFAULT_API_BASE;
  const response = await fetch(`${safeBase}${path}`, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });

  if (!response.ok) {
    const message = await response.text();
    throw new Error(message || `Request failed: ${response.status}`);
  }

  return response.json();
}

export async function probeServer(base) {
  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), 1200);
  try {
    const safeBase = normalizeBase(base) || DEFAULT_API_BASE;
    const response = await fetch(`${safeBase}/health`, {
      signal: controller.signal,
    });
    return response.ok;
  } catch {
    return false;
  } finally {
    clearTimeout(timeoutId);
  }
}

export async function discoverServers(options = {}) {
  const {
    seeds = [],
    port = 8000,
    timeoutMs = 450,
    concurrency = 36,
  } = options;

  const seedPrefixes = seeds
    .map((seed) => hostToPrefix(seed))
    .filter(Boolean);

  const candidatePrefixes = [
    ...new Set([
      ...seedPrefixes,
      "192.168.1",
      "192.168.0",
      "10.0.0",
      "10.0.1",
    ]),
  ];

  const targets = [];
  for (const prefix of candidatePrefixes) {
    for (let host = 2; host <= 254; host += 1) {
      targets.push(`http://${prefix}.${host}:${port}`);
    }
  }

  const discovered = [];
  let cursor = 0;

  async function worker() {
    while (cursor < targets.length) {
      const index = cursor;
      cursor += 1;
      const base = targets[index];
      const controller = new AbortController();
      const timeoutId = setTimeout(() => controller.abort(), timeoutMs);
      try {
        const response = await fetch(`${base}/health`, { signal: controller.signal });
        if (!response.ok) {
          continue;
        }
        const body = await response.json().catch(() => ({}));
        if (body.ok) {
          discovered.push({
            base,
            hostname: body.hostname || null,
            service: body.service || null,
          });
        }
      } catch {
        // Ignore unreachable hosts during discovery.
      } finally {
        clearTimeout(timeoutId);
      }
    }
  }

  const workers = Array.from({ length: concurrency }, () => worker());
  await Promise.all(workers);

  return discovered;
}

export function getStatus(base) {
  return request(base, "/api/status");
}

export function getFiles(base) {
  return request(base, "/api/files");
}

export function toggleRecording(base) {
  return request(base, "/api/record/toggle", { method: "POST" });
}

export function startRecording(base) {
  return request(base, "/api/record/start", { method: "POST" });
}

export function stopRecording(base) {
  return request(base, "/api/record/stop", { method: "POST" });
}

export function getRecordingCaptureMode(base) {
  return request(base, "/api/config/recording-capture-mode");
}

export function setRecordingCaptureMode(base, mode) {
  return request(base, "/api/config/recording-capture-mode", {
    method: "POST",
    body: JSON.stringify({ mode }),
  });
}

export function getStreamUrl(base) {
  const safeBase = normalizeBase(base) || DEFAULT_API_BASE;
  return `${safeBase}/api/stream/mjpeg`;
}

/** Returns the WHEP signalling URL routed through the companion API proxy. */
export function getWhepUrl(base) {
  const safeBase = normalizeBase(base) || DEFAULT_API_BASE;
  return `${safeBase}/api/stream/whep`;
}

export function getFileDownloadUrl(base, name) {
  const safeBase = normalizeBase(base) || DEFAULT_API_BASE;
  return `${safeBase}/api/files/download/${encodeURIComponent(name)}`;
}
