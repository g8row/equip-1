import React from "react";

/**
 * Glyph-style status dot.
 * state: "idle" | "live" | "ok" | "warn"
 */
export default function StatusDot({ state = "idle", label, className = "" }) {
  const dot = <span className={`dot ${state}`} />;
  if (!label) return dot;
  return (
    <span className={`status-line ${className}`.trim()}>
      {dot}
      <span>{label}</span>
    </span>
  );
}
