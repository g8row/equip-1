import React, { useEffect, useRef, useState } from "react";
import Button from "./ui/Button";
import { formatBytes } from "../lib/format";
import { downloadToDevice } from "../lib/download";

// How long the "Saved" confirmation stays up before the row reverts to a
// plain "Get" button — long enough to notice, short enough not to litter
// the list after exporting several files back to back.
const DONE_DISPLAY_MS = 4000;

/**
 * T4.11 native "Get" action: streams a capture to Directory.Documents via
 * lib/download.js instead of the plain `<a download>` (Files.jsx keeps that
 * for the web build — the Capacitor WebView has no DownloadListener and
 * swallows the link for anything past a trivial size; real tapes export as
 * multi-GB files).
 */
export default function DownloadButton({ url, name, sizeBytes }) {
  const [state, setState] = useState({ status: "idle" });
  const controllerRef = useRef(null);
  const doneTimerRef = useRef(null);

  useEffect(
    () => () => {
      // Unmounting (e.g. navigating away mid-download) cancels the
      // transfer rather than leaving a detached fetch writing to disk.
      controllerRef.current?.abort();
      if (doneTimerRef.current) clearTimeout(doneTimerRef.current);
    },
    []
  );

  async function start() {
    if (state.status === "downloading") return;
    const controller = new AbortController();
    controllerRef.current = controller;
    setState({ status: "downloading", received: 0, total: sizeBytes || null });

    try {
      await downloadToDevice(url, name, {
        signal: controller.signal,
        onProgress: (received, total) =>
          setState((prev) =>
            prev.status === "downloading"
              ? { ...prev, received, total: total ?? prev.total }
              : prev
          ),
      });
      setState({ status: "done" });
      doneTimerRef.current = setTimeout(() => setState({ status: "idle" }), DONE_DISPLAY_MS);
    } catch (err) {
      if (controller.signal.aborted) {
        // User-initiated cancel — back to idle with no error noise.
        setState({ status: "idle" });
      } else {
        setState({ status: "error", message: err.message || "Download failed" });
      }
    } finally {
      controllerRef.current = null;
    }
  }

  function cancel() {
    controllerRef.current?.abort();
  }

  if (state.status === "downloading") {
    const pct =
      state.total != null ? Math.min(100, Math.round((state.received / state.total) * 100)) : null;
    return (
      <span className="download-progress" role="group" aria-label={`Downloading ${name}`}>
        <span className="bar download-progress__bar">
          <span
            className="bar__fill"
            style={{ width: pct != null ? `${pct}%` : "100%" }}
          />
        </span>
        <span className="label" aria-live="polite">
          {pct != null ? `${pct}%` : formatBytes(state.received)}
        </span>
        <Button size="sm" variant="ghost" onClick={cancel}>
          Cancel
        </Button>
      </span>
    );
  }

  if (state.status === "done") {
    return (
      <Button size="sm" variant="ghost" disabled>
        Saved
      </Button>
    );
  }

  return (
    <span className="download-progress">
      <Button size="sm" variant="ghost" onClick={start}>
        Get
      </Button>
      {state.status === "error" ? (
        <span className="label" role="alert" title={state.message}>
          failed
        </span>
      ) : null}
    </span>
  );
}
