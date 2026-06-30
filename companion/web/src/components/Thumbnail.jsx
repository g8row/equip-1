import React, { useEffect, useState } from "react";
import { getThumbnailUrl } from "../api";

// Android WebView has a known mixed-content quirk: a plain
// <img src="http://..."> cross-protocol load from an https://localhost page
// gets silently blocked even with MIXED_CONTENT_ALWAYS_ALLOW set, while
// fetch() to the identical URL succeeds. Fetching as a blob and handing the
// <img> a blob: object URL sidesteps it entirely (verified against the live
// device via Chrome DevTools — direct <img src> failed, fetch().blob() +
// createObjectURL() loaded fine).
export default function Thumbnail({ apiBase, name, className = "file-thumb" }) {
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
  return <img className={className} src={blobUrl} alt="" />;
}
