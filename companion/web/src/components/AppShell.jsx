import React from "react";
import { NavLink, Outlet } from "react-router-dom";
import { useServer } from "../context/ServerContext";
import StatusDot from "./ui/StatusDot";

const NAV = [
  { to: "/", label: "View", end: true },
  { to: "/files", label: "Files" },
  { to: "/settings", label: "Setup" },
  { to: "/connect", label: "Link" },
];

function DotGrid() {
  return (
    <span className="nav__dotgrid" aria-hidden="true">
      <span />
      <span />
      <span />
      <span />
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
            <DotGrid />
            {item.label}
          </NavLink>
        ))}
      </nav>
    </div>
  );
}
