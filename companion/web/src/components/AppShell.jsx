import React from "react";
import { NavLink, Outlet } from "react-router-dom";
import { useServer } from "../context/ServerContext";
import StatusDot from "./ui/StatusDot";

// 3x3 dot-matrix glyphs — keeps the Nothing-OS dot aesthetic while giving
// each tab a distinct, recognizable shape instead of an identical 2x2 grid.
const GLYPHS = {
  view: [0, 1, 0, 1, 1, 1, 0, 1, 0], // camera / target
  files: [1, 1, 1, 1, 0, 1, 1, 1, 1], // stack / folder
  setup: [1, 0, 1, 0, 1, 0, 1, 0, 1], // sliders
  link: [1, 0, 0, 0, 1, 0, 0, 0, 1], // diagonal link
};

const NAV = [
  { to: "/", label: "View", end: true, glyph: "view" },
  { to: "/files", label: "Files", glyph: "files" },
  { to: "/settings", label: "Setup", glyph: "setup" },
  { to: "/connect", label: "Link", glyph: "link" },
];

function DotGrid({ glyph }) {
  const cells = GLYPHS[glyph] ?? GLYPHS.view;
  return (
    <span className="nav__dotgrid" aria-hidden="true">
      {cells.map((on, i) => (
        <span key={i} className={on ? "" : "nav__dotgrid-off"} />
      ))}
    </span>
  );
}

export default function AppShell() {
  const { status, apiBase, reachable } = useServer();
  const isRecording = status?.recorder?.mode === "recording";

  let serverState = "warn";
  let serverLabel = "connecting";
  if (reachable === true) {
    serverState = "ok";
    serverLabel = "online";
  } else if (reachable === false) {
    serverState = "idle";
    serverLabel = "offline";
  }

  const host = (() => {
    try {
      return new URL(apiBase).host;
    } catch {
      return apiBase;
    }
  })();

  return (
    <div className="shell">
      <header className="shell__header">
        <div className="shell__brand">
          <span className="dot live" style={{ opacity: 0.9 }} />
          <span className="wordmark">EQUIP&middot;1</span>
        </div>
        <div className="shell__status">
          <span className="field">
            <StatusDot state={isRecording ? "live" : "idle"} />
            <span className="label">{isRecording ? "rec" : "idle"}</span>
          </span>
          <span className="field" title={apiBase}>
            <StatusDot state={serverState} />
            <span className="data dim">{host || serverLabel}</span>
          </span>
        </div>
      </header>

      <main className="shell__main">
        <Outlet />
      </main>

      <nav className="nav nav--bottom" aria-label="Primary">
        {NAV.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end}
            className={({ isActive }) => `nav__item ${isActive ? "active" : ""}`}
          >
            <DotGrid glyph={item.glyph} />
            {item.label}
          </NavLink>
        ))}
      </nav>
    </div>
  );
}
