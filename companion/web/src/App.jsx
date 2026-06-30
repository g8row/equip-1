import React from "react";
import { Routes, Route, Navigate } from "react-router-dom";
import { ServerProvider } from "./context/ServerContext";
import AppShell from "./components/AppShell";
import Viewfinder from "./pages/Viewfinder";
import Files from "./pages/Files";
import Settings from "./pages/Settings";
import Connect from "./pages/Connect";

export default function App() {
  return (
    <ServerProvider>
      <Routes>
        <Route element={<AppShell />}>
          <Route index element={<Viewfinder />} />
          <Route path="files" element={<Files />} />
          <Route path="settings" element={<Settings />} />
          <Route path="connect" element={<Connect />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Routes>
    </ServerProvider>
  );
}
