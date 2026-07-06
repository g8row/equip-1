import React from "react";

/**
 * Glyph-style status dot.
 * state: "idle" | "live" | "ok" | "warn"
 *
 * `label` renders visible text next to the dot. `srLabel` is for the other
 * case — a dot used on its own (or next to text that doesn't actually state
 * what the color means, e.g. an address) — it names the state for screen
 * readers without adding visible clutter. A dot with neither is presentation
 * only and is hidden from assistive tech rather than announced as nothing.
 */
export default function StatusDot({ state = "idle", label, srLabel, className = "" }) {
  if (label) {
    return (
      <span className={`status-line ${className}`.trim()}>
        <span className={`dot ${state}`} aria-hidden="true" />
        <span>{label}</span>
      </span>
    );
  }
  if (srLabel) {
    return (
      <span className={className}>
        <span className={`dot ${state}`} aria-hidden="true" />
        <span className="sr-only">{srLabel}</span>
      </span>
    );
  }
  return <span className={`dot ${state}`} aria-hidden="true" />;
}
