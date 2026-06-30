import React from "react";

export function Tabs({ children, className = "", ...rest }) {
  return (
    <div className={`tabs ${className}`.trim()} role="tablist" {...rest}>
      {children}
    </div>
  );
}

export default function Tab({ active = false, badge, children, ...rest }) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={active}
      className={`tab ${active ? "active" : ""}`.trim()}
      {...rest}
    >
      <span>{children}</span>
      {badge ? <span className="badge">{badge}</span> : null}
    </button>
  );
}
