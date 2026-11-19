"use client";

import { useEffect, useId, useRef, type ReactNode } from "react";

/** 站内通用弹窗：复用图片弹窗的样式，提供焦点恢复与 Esc 关闭。 */
export function AppDialog({
  title,
  busy = false,
  onClose,
  children,
  footer,
}: {
  title: string;
  busy?: boolean;
  onClose: () => void;
  children: ReactNode;
  footer: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  useEffect(() => {
    const dialog = ref.current!;
    const previous = document.activeElement;
    dialog.showModal();
    return () => {
      dialog.close();
      if (previous instanceof HTMLElement && previous.isConnected) {
        previous.focus();
      }
    };
  }, []);

  return (
    <dialog
      ref={ref}
      className="image-dialog admin-dialog"
      aria-labelledby={titleId}
      onCancel={(event) => {
        if (event.target !== event.currentTarget) return;
        event.preventDefault();
        if (!busy) onClose();
      }}
    >
      <header className="image-dialog-header">
        <h2 id={titleId}>{title}</h2>
        <button
          type="button"
          className="image-button"
          aria-label="关闭弹窗"
          disabled={busy}
          onClick={onClose}
        >
          ×
        </button>
      </header>
      <div className="image-dialog-body">{children}</div>
      <footer className="image-dialog-footer">{footer}</footer>
    </dialog>
  );
}

export function ConfirmDialog({
  title,
  message,
  confirmLabel = "确认",
  danger = false,
  busy = false,
  error,
  onConfirm,
  onClose,
}: {
  title: string;
  message: ReactNode;
  confirmLabel?: string;
  danger?: boolean;
  busy?: boolean;
  error?: string | null;
  onConfirm: () => void;
  onClose: () => void;
}) {
  return (
    <AppDialog
      title={title}
      busy={busy}
      onClose={onClose}
      footer={
        <>
          <button
            type="button"
            className="image-button"
            disabled={busy}
            onClick={onClose}
          >
            取消
          </button>
          <button
            type="button"
            className="image-button primary"
            disabled={busy}
            onClick={onConfirm}
            style={danger ? { background: "var(--ph-danger)", borderColor: "var(--ph-danger)" } : undefined}
          >
            {busy ? "处理中…" : confirmLabel}
          </button>
        </>
      }
    >
      <p style={{ margin: 0, lineHeight: 1.6 }}>{message}</p>
      {error && (
        <p role="alert" className="image-notice error">
          {error}
        </p>
      )}
    </AppDialog>
  );
}
