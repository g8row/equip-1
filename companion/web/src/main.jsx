import React from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
// Self-hosted fonts (bundled by Vite) — the device AP has no internet, and the
// old Google Fonts <link> in index.html was render-blocking: on a no-internet
// network Chromium waits out a long connect timeout on fonts.googleapis.com,
// stalling the whole app (missing fonts AND slow-to-connect preview). Bundling
// them removes every internet dependency.
import "@fontsource-variable/doto"; // display (variable 100–900)
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/500.css";
import "@fontsource/ibm-plex-mono/600.css";
import App from "./App";
import "./styles.css";

createRoot(document.getElementById("root")).render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>
);
