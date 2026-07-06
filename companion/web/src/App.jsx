import React, { useEffect } from "react";
import { Routes, Route, Navigate } from "react-router-dom";
import { ServerProvider, useServer } from "./context/ServerContext";
import AppShell from "./components/AppShell";
import Viewfinder from "./pages/Viewfinder";
import Files from "./pages/Files";
import Settings from "./pages/Settings";
import Connect from "./pages/Connect";
import { initSystemBars } from "./lib/systemBars";

// Index route: a device that has never been paired has nothing to show on
// the Viewfinder (no apiBase worth trusting) — send it straight to Connect
// instead of a dashboard for a server that's just the unconfigured default.
function HomeRoute() {
  const { firstRun } = useServer();
  return firstRun ? <Navigate to="/connect" replace /> : <Viewfinder />;
}

export default function App() {
  useEffect(() => {
    initSystemBars();
  }, []);

  return (
    <ServerProvider>
      <Routes>
        <Route element={<AppShell />}>
          <Route index element={<HomeRoute />} />
          <Route path="files" element={<Files />} />
          <Route path="settings" element={<Settings />} />
          <Route path="connect" element={<Connect />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </ServerProvider>
  );
}
