"use client";

import { useId, useState } from "react";
import { AppDialog } from "@/app/components/Dialogs";
import { inputStyle } from "@/app/components/ui";

export default function EmailChangeDialog({
  currentEmail,
  onClose,
}: {
  currentEmail: string;
  onClose: () => void;
}) {
  const formId = useId();
  const [password, setPassword] = useState("");
  const [email, setEmail] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [sent, setSent] = useState(false);
  const [busy, setBusy] = useState(false);

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);
    if (!password) {
      setError("请输入当前密码");
      return;
    }
    const normalizedEmail = email.trim().toLowerCase();
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(normalizedEmail)) {
      setError("请输入有效的新邮箱地址");
      return;
    }

    setBusy(true);
    try {
      const response = await fetch("/api/auth/change-email", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ password, email: normalizedEmail }),
      });
      const data = await response.json().catch(() => ({}));
      if (!response.ok) {
        const code = data?.error?.code;
        const message = response.status === 409 || code === "EMAIL_EXISTS"
          ? "该邮箱已被其他账号使用，请换一个邮箱。"
          : response.status === 429
            ? "操作过于频繁，请稍后重试。"
            : response.status === 503
              ? "暂时无法发送确认邮件，请稍后重试或联系管理员。"
              : response.status === 400
                ? code === "INVALID_CREDENTIALS"
                  ? "当前密码不正确，请重试。"
                  : code === "SAME_EMAIL"
                    ? "新邮箱与当前邮箱相同，请填写不同的邮箱地址。"
                    : "请检查当前密码和新邮箱后重试。"
                : "修改邮箱失败，请重试。";
        setError(message);
        return;
      }
      setPassword("");
      setSent(true);
    } catch {
      setError("网络异常，确认邮件未能发送，请稍后重试。");
    } finally {
      setBusy(false);
    }
  }

  return (
    <AppDialog
      title="修改登录邮箱"
      busy={busy}
      onClose={onClose}
      footer={
        sent ? (
          <button type="button" className="image-button primary" onClick={onClose}>
            关闭
          </button>
        ) : (
          <>
            <button type="button" className="image-button" disabled={busy} onClick={onClose}>
              取消
            </button>
            <button type="submit" form={formId} className="image-button primary" disabled={busy}>
              {busy ? "发送中…" : "发送确认邮件"}
            </button>
          </>
        )
      }
    >
      {sent ? (
        <p role="status" className="image-notice success">
          确认邮件已发送，请到新邮箱打开链接完成修改。
        </p>
      ) : (
        <form id={formId} noValidate onSubmit={(event) => void submit(event)} style={{ display: "grid", gap: 14 }}>
          <div style={{ display: "grid", gap: 4, minWidth: 0 }}>
            <span className="image-help">当前邮箱</span>
            <strong style={{ overflowWrap: "anywhere" }}>{currentEmail || "未设置"}</strong>
          </div>
          <label className="image-field">
            当前密码
            <input
              type="password"
              autoComplete="current-password"
              autoFocus
              value={password}
              disabled={busy}
              onChange={(event) => setPassword(event.target.value)}
              style={inputStyle}
            />
          </label>
          <label className="image-field">
            新邮箱
            <input
              type="email"
              autoComplete="email"
              value={email}
              disabled={busy}
              onChange={(event) => setEmail(event.target.value)}
              style={inputStyle}
            />
          </label>
          {error && <p role="alert" className="image-notice error">{error}</p>}
        </form>
      )}
    </AppDialog>
  );
}
