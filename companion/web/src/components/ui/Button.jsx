import React from "react";

/**
 * Minimal pill button.
 * variant: "default" | "primary" | "accent" | "ghost" | "danger"
 */
export default function Button({
  variant = "default",
  size,
  block = false,
  className = "",
  type = "button",
  ...rest
}) {
  const classes = [
    "btn",
    variant !== "default" ? `btn--${variant}` : "",
    size === "sm" ? "btn--sm" : "",
    block ? "btn--block" : "",
    className,
  ]
    .filter(Boolean)
    .join(" ");
  return <button type={type} className={classes} {...rest} />;
}
