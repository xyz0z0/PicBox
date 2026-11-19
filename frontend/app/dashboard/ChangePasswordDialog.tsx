"use client";

import { useId, useState } from "react";
import { AppDialog } from "@/app/components/Dialogs";
import { inputStyle } from "@/app/components/ui";

const MIN_PASSWORD_LENGTH = 6;

export default function ChangePasswordDialog({
  onClose,
  onSuccess,
}: {
  onClose: () => void;
  onSuccess: () => void;
}) {
  const formId = useId();
  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);
    if (!oldPassword) {
      setError("请输入原密码");
      return;
    }
    if (newPassword.length < MIN_PASSWORD_LENGTH) {
      setError(`新密码至少 ${MIN_PASSWORD_LENGTH} 位`);
      return;
    }
    if (newPassword === oldPassword) {
      setError("新密码不能与原密码相同");
      return;
    }
    if (newPassword !== confirmPassword) {
      setError("两次输入的新密码不一致");
      return;
    }

    setBusy(true);
    try {
      const response = await fetch("/api/auth/change-password", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ oldPassword, newPassword }),
      });
      const data = await response.json().catch(() => ({}));
      if (!response.ok) {
        setError(data?.error?.message || "修改密码失败，请检查原密码后重试");
        return;
      }
      setOldPassword("");
      setNewPassword("");
      setConfirmPassword("");
      onSuccess();
    } catch {
      setError("网络异常，修改密码失败，请重试");
    } finally {
      setBusy(false);
    }
  }

  return (
    <AppDialog
      title="修改密码"
      busy={busy}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="image-button" disabled={busy} onClick={onClose}>
            取消
          </button>
          <button type="submit" form={formId} className="image-button primary" disabled={busy}>
            {busy ? "修改中…" : "保存密码"}
          </button>
        </>
      }
    >
      <form id={formId} onSubmit={(event) => void submit(event)} style={{ display: "grid", gap: 14 }}>
        <label className="image-field">
          原密码
          <input
            type="password"
            autoComplete="current-password"
            autoFocus
            value={oldPassword}
            disabled={busy}
            onChange={(event) => setOldPassword(event.target.value)}
            style={inputStyle}
          />
        </label>
        <label className="image-field">
          新密码
          <input
            type="password"
            autoComplete="new-password"
            value={newPassword}
            disabled={busy}
            onChange={(event) => setNewPassword(event.target.value)}
            style={inputStyle}
          />
        </label>
        <label className="image-field">
          确认新密码
          <input
            type="password"
            autoComplete="new-password"
            value={confirmPassword}
            disabled={busy}
            onChange={(event) => setConfirmPassword(event.target.value)}
            style={inputStyle}
          />
        </label>
        {error && <p role="alert" className="image-notice error">{error}</p>}
      </form>
    </AppDialog>
  );
}
