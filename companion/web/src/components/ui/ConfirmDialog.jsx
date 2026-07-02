import React, { useEffect } from "react";
import { createPortal } from "react-dom";
import Button from "./Button";

/**
 * In-app confirmation modal — a styled, branded replacement for window.confirm.
 * On Capacitor/Android the native window.confirm shows an unbranded system
 * dialog (app icon + generic OK/Cancel), which looks broken; this renders our
 * own overlay instead and behaves identically on web and native.
 *
 * Controlled: pass `open` and the label/handler props. Esc cancels, Enter
 * confirms. See useConfirm-style usage in Settings/Files (a small promise
 * helper wraps this for drop-in `await` at call sites).
 */
export default function ConfirmDialog({
  open,
  title,
  message,
  confirmLabel = "OK",
  cancelLabel = "Cancel",
  danger = false,
  onConfirm,
  onCancel,
}) {
  useEffect(() => {
    if (!open) return undefined;
    const onKey = (e) => {
      if (e.key === "Escape") onCancel?.();
      else if (e.key === "Enter") onConfirm?.();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onConfirm, onCancel]);

  if (!open) return null;
  return createPortal(
    <div className="modal-overlay" onClick={onCancel}>
      <div
        className="modal"
        role="alertdialog"
        aria-modal="true"
        aria-label={title || "Confirm"}
        onClick={(e) => e.stopPropagation()}
      >
        {title && <h3 className="modal__title">{title}</h3>}
        <p className="modal__msg">{message}</p>
        <div className="modal__actions">
          <Button variant="ghost" onClick={onCancel}>
            {cancelLabel}
          </Button>
          <Button variant={danger ? "danger" : "primary"} onClick={onConfirm}>
            {confirmLabel}
          </Button>
        </div>
      </div>
    </div>,
    document.body
  );
}
