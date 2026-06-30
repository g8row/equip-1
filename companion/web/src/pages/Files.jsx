import React, { useEffect, useState } from "react";
import { useServer } from "../context/ServerContext";
import { deleteFile, getFileDownloadUrl, getThumbnailUrl } from "../api";
import { formatBytes, formatDate } from "../lib/format";
import { canShareNative, shareFile } from "../lib/share";
import Card from "../components/ui/Card";
import Button from "../components/ui/Button";

// Android WebView has a known mixed-content quirk: a plain
// <img src="http://..."> cross-protocol load from an https://localhost page
// gets silently blocked even with MIXED_CONTENT_ALWAYS_ALLOW set, while
// fetch() to the identical URL succeeds. Fetching as a blob and handing the
// <img> a blob: object URL sidesteps it entirely (verified against the live
// device via Chrome DevTools — direct <img src> failed, fetch().blob() +
// createObjectURL() loaded fine).
function Thumbnail({ apiBase, name }) {
  const [blobUrl, setBlobUrl] = useState(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    let objectUrl = null;
    setFailed(false);
    setBlobUrl(null);

    fetch(getThumbnailUrl(apiBase, name))
      .then((res) => (res.ok ? res.blob() : Promise.reject(new Error("not ok"))))
      .then((blob) => {
        if (cancelled) return;
        objectUrl = URL.createObjectURL(blob);
        setBlobUrl(objectUrl);
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });

    return () => {
      cancelled = true;
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
  }, [apiBase, name]);

  if (failed || !blobUrl) return null;
  return <img className="file-thumb" src={blobUrl} alt="" />;
}

export default function Files() {
  const { apiBase, files, status, refresh, setError } = useServer();
  const [busy, setBusy] = useState("");
  const [sharing, setSharing] = useState("");

  const storage = status?.storage ?? {};
  const usedPercent = Number(storage.used_percent ?? 0);

  async function onDelete(name) {
    if (!window.confirm(`Delete ${name}? This cannot be undone.`)) return;
    setBusy(name);
    try {
      await deleteFile(apiBase, name);
      await refresh();
    } catch (err) {
      setError(err.message || "Delete failed");
    } finally {
      setBusy("");
    }
  }

  async function onShare(file) {
    setSharing(file.name);
    try {
      await shareFile(getFileDownloadUrl(apiBase, file.name), file.name);
    } catch (err) {
      // User-cancelled share sheets reject too — don't surface those as errors.
      if (!/cancel/i.test(err.message || "")) {
        setError(err.message || "Share failed");
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

      <Card title="Disk usage">
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
      </Card>

      <Card
        title="Recordings"
        action={
          <Button size="sm" variant="ghost" onClick={() => refresh()}>
            Refresh
          </Button>
        }
      >
        {files.length === 0 ? (
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
                  <a
                    className="btn btn--sm"
                    href={getFileDownloadUrl(apiBase, file.name)}
                    target="_blank"
                    rel="noreferrer"
                    download={file.name}
                  >
                    Get
                  </a>
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
