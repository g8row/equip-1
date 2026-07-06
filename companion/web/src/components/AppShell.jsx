import React, { useEffect } from "react";
import { Link, NavLink, Outlet } from "react-router-dom";
import { useServer } from "../context/ServerContext";
import { setRecordingBar } from "../lib/systemBars";
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
  { to: "/connect", label: "Connect", glyph: "link" },
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
  // Once the connection drops, `isRecording` reflects the last-known state,
  // not reality — the device could have auto-stopped (storage, error) while
  // unreachable. Don't keep asserting "recording" system-wide off stale data.
  const isStale = reachable === false;
  const showRecording = isRecording && !isStale;

  // Tint the native status bar red while recording — a system-level REC cue.
  // Cleared as soon as the connection is lost so it can't get stuck red.
  useEffect(() => {
    setRecordingBar(showRecording);
  }, [showRecording]);

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
            <StatusDot state={showRecording ? "live" : "idle"} />
            <span className="label">{isRecording && isStale ? "rec?" : showRecording ? "rec" : "idle"}</span>
          </span>
          <Link
            to="/connect"
            className="field"
            title={apiBase}
            style={{ textDecoration: "none" }}
          >
            {/* The visible text is usually the host/IP, not a state word —
                give the dot its own accessible name so the connection state
                isn't sighted-only. */}
            <StatusDot state={serverState} srLabel={serverLabel} />
            <span className="data dim">{host || serverLabel}</span>
          </Link>
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
