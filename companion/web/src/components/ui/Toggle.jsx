import React from "react";

export default function Toggle({ checked = false, onChange, disabled = false, label }) {
  const button = (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      className="toggle"
      disabled={disabled}
      onClick={() => onChange && onChange(!checked)}
    />
  );
  if (!label) return button;
  return (
    <div className="toggle-row">
      <span className="label">{label}</span>
      {button}
    </div>
  );
}
