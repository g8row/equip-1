import React from "react";

/** Four-bar WiFi signal strength indicator. strength is 0-100. */
export default function SignalBars({ strength }) {
  const bars = strength == null ? 0 : Math.ceil((strength / 100) * 4);
  return (
    <span
      style={{
        display: "inline-flex",
        alignItems: "flex-end",
        gap: 2,
        height: 14,
        verticalAlign: "middle",
      }}
      aria-label={`Signal: ${bars} of 4 bars`}
    >
      {[1, 2, 3, 4].map((b) => (
        <span
          key={b}
          style={{
            display: "inline-block",
            width: 3,
            height: 3 + b * 2.5,
            borderRadius: 1,
            background: b <= bars ? "var(--ok)" : "var(--line-strong)",
          }}
        />
      ))}
    </span>
  );
}
