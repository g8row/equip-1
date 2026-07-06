import React, { useState } from "react";
import { useServer } from "../context/ServerContext";
import { deleteFile, getFileDownloadUrl } from "../api";
import { formatBytes, formatDate } from "../lib/format";
import { canShareNative, shareFile } from "../lib/share";
import { isNative } from "../lib/native";
import Card from "../components/ui/Card";
import Button from "../components/ui/Button";
import Thumbnail from "../components/Thumbnail";
import DownloadButton from "../components/DownloadButton";

export default function Files() {
  // `refresh()` in ServerContext now only polls status (files were pulled out
  // of the hot loop — see T4.9); this page fetches its own list explicitly.
  const { apiBase, files, status, reachable, refreshFiles } = useServer();
  const [busy, setBusy] = useState("");
  const [sharing, setSharing] = useState("");
  // Local, transient action-error state — previously shared the context's
  // connectivity `error`, which the poll's next successful tick wiped out
  // within ~1.5s regardless of whether the user had seen it yet.
  const [actionError, setActionError] = useState("");

  // status/files default to their last-known values (or empty) while
  // unreachable — without this, "0 B used" / "No recordings found yet."
  // look exactly like a legitimately empty device instead of one we simply
  // can't currently reach.
  const offline = reachable === false;
  const storage = status?.storage ?? {};
  const usedPercent = Number(storage.used_percent ?? 0);

  async function onDelete(name) {
    if (!window.confirm(`Delete ${name}? This cannot be undone.`)) return;
    setActionError("");
    setBusy(name);
    try {
      await deleteFile(apiBase, name);
      await refreshFiles();
    } catch (err) {
      setActionError(err.message || "Delete failed");
    } finally {
      setBusy("");
    }
  }

  async function onShare(file) {
    setActionError("");
    setSharing(file.name);
    try {
      await shareFile(getFileDownloadUrl(apiBase, file.name), file.name);
    } catch (err) {
      // User-cancelled share sheets reject too — don't surface those as errors.
      if (!/cancel/i.test(err.message || "")) {
        setActionError(err.message || "Share failed");
      }
    } finally {
      setSharing("");
    }
  }

  return (
    <div className="stack">
      <div className="page-head">
        <span className="label">storage</span>
        <h1 className="display">Files</h1>
      </div>

      {actionError ? (
        <div className="notice" role="alert">
          {actionError}
        </div>
      ) : null}

      <Card title="Disk usage">
        {offline ? (
          <p className="muted-box">Can&apos;t read storage — device unreachable.</p>
        ) : (
          <>
            <div className="bar" aria-label={`${usedPercent}% used`}>
              <div className="bar__fill" style={{ width: `${Math.min(100, usedPercent)}%` }} />
            </div>
            <div style={{ marginTop: "var(--sp-3)" }}>
              <div className="kv">
                <span className="kv__k">used</span>
                <span className="kv__v">
                  {formatBytes(storage.used_bytes ?? 0)} ({usedPercent}%)
                </span>
              </div>
              <div className="kv">
                <span className="kv__k">free</span>
                <span className="kv__v">{formatBytes(storage.free_bytes ?? 0)}</span>
              </div>
              <div className="kv">
                <span className="kv__k">total</span>
                <span className="kv__v">{formatBytes(storage.total_bytes ?? 0)}</span>
              </div>
            </div>
          </>
        )}
      </Card>

      <Card
        title="Recordings"
        action={
          <Button size="sm" variant="ghost" onClick={() => refreshFiles()}>
            Refresh
          </Button>
        }
      >
        {offline ? (
          <p className="muted-box">Can&apos;t load recordings — device unreachable.</p>
        ) : files.length === 0 ? (
          <p className="dim" style={{ fontSize: "0.82rem" }}>
            No recordings found yet.
          </p>
        ) : (
          <ul className="files-list">
            {files.map((file) => (
              <li key={`${file.name}-${file.modified_unix}`}>
                <Thumbnail apiBase={apiBase} name={file.name} />
                <span className="file-meta">
                  <span className="name">{file.name}</span>
                  <span className="label">
                    {formatBytes(file.size_bytes)} &middot;{" "}
                    {formatDate(file.modified_unix)}
                  </span>
                </span>
                <span className="file-actions">
                  {canShareNative(file.size_bytes) && (
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={sharing === file.name}
                      onClick={() => onShare(file)}
                    >
                      {sharing === file.name ? "…" : "Share"}
                    </Button>
                  )}
                  {isNative() ? (
                    // Native: the WebView has no DownloadListener, so a plain
                    // `<a download>` is typically swallowed silently for
                    // anything past a trivial size (T4.11) — stream to disk
                    // via @capacitor/filesystem with progress + cancel
                    // instead. Web keeps the browser's own download manager.
                    <DownloadButton
                      url={getFileDownloadUrl(apiBase, file.name)}
                      name={file.name}
                      sizeBytes={file.size_bytes}
                    />
                  ) : (
                    <a
                      className="btn btn--sm"
                      href={getFileDownloadUrl(apiBase, file.name)}
                      target="_blank"
                      rel="noreferrer"
                      download={file.name}
                    >
                      Get
                    </a>
                  )}
                  <Button
                    size="sm"
                    variant="danger"
                    disabled={busy === file.name}
                    onClick={() => onDelete(file.name)}
                  >
                    {busy === file.name ? "…" : "Del"}
                  </Button>
                </span>
              </li>
            ))}
          </ul>
        )}
      </Card>
    </div>
  );
}
