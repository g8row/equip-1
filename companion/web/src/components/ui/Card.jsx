import React from "react";

export default function Card({ title, action, children, className = "", ...rest }) {
  return (
    <section className={`card ${className}`.trim()} {...rest}>
      {(title || action) && (
        <div className="card__head">
          {title ? <h2 className="display" style={{ fontSize: "1rem" }}>{title}</h2> : <span />}
          {action}
        </div>
      )}
      {children}
    </section>
  );
}
