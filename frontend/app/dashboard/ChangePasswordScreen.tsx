"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";

// 与 LoginCard 保持一致的紧凑输入样式。
const inputStyle: React.CSSProperties = {
  width: "100%",
  boxSizing: "border-box",
  padding: "10px 14px",
  borderRadius: 2,
  border: "1px solid var(--ph-border)",
  fontSize: 14,
  background: "var(--ph-surface)",
  color: "var(--ph-text)",
};

const MIN_PASSWORD_LENGTH = 6;

/**
 * 管理员创建账号后首次登录的整页改密界面。由 DashboardClient 在
 * mustChangePassword=true 时条件渲染——没有关闭按钮、没有导航、没有 Esc，
 * 因此无法跳过；提交成功后服务端清标记，router.refresh() 回到正常工作台。
 */
export default function ChangePasswordScreen() {
  const router = useRouter();
  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function onSubmit() {
    setError(null);
    if (!oldPassword) {
      setError("请输入管理员提供的临时密码");
      return;
    }
    if (newPassword.length < MIN_PASSWORD_LENGTH) {
      setError(`新密码至少 ${MIN_PASSWORD_LENGTH} 位`);
      return;
    }
    if (newPassword === oldPassword) {
      setError("新密码不能与原密码相同，请设置一个新的密码");
      return;
    }
    if (newPassword !== confirmPassword) {
      setError("两次输入的新密码不一致");
      return;
    }

    setBusy(true);
    try {
      const res = await fetch("/api/auth/change-password", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ oldPassword, newPassword }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setError(data?.error?.message || "修改失败，请重试");
        return;
      }
      // 服务端已重签 cookie 并清除 mustChangePassword，刷新后回到正常工作台。
      router.refresh();
    } catch {
      setError("网络异常，修改失败");
    } finally {
      setBusy(false);
    }
  }

  const passwordType = showPassword ? "text" : "password";

  return (
    <div className="auth-root">
      <div className="auth-shell">
        <aside className="auth-aside">
          <a className="auth-brand" href="/">
            PicBox
          </a>
          <div>
            <h2>先设置一个自己的密码</h2>
            <p>你的账号由管理员创建，首次使用需要更换临时密码。</p>
            <ul className="auth-points">
              <li>临时密码仅用于首次登录</li>
              <li>新密码至少 6 位</li>
              <li>修改后立即生效，无需重新登录</li>
            </ul>
          </div>
          <p style={{ fontSize: 12, color: "rgb(255 255 255 / 62%)" }}>
            自托管的私人图床
          </p>
        </aside>

        <div className="auth-card">
          <div className="auth-card-head">
            <h1>修改密码</h1>
            <p>为了账号安全，请先更换管理员生成的临时密码。</p>
          </div>

          <form
            className="auth-form"
            onSubmit={(event) => {
              event.preventDefault();
              void onSubmit();
            }}
          >
            <div className="auth-label">
              <label htmlFor="change-old">临时密码</label>
              <span className="auth-password-field">
                <input
                  id="change-old"
                  className="auth-input auth-password-input"
                  type={passwordType}
                  value={oldPassword}
                  onChange={(event) => setOldPassword(event.target.value)}
                  placeholder="管理员提供的临时密码"
                  autoComplete="current-password"
                  autoFocus
                  style={inputStyle}
                />
                <button
                  type="button"
                  className="auth-password-toggle"
                  aria-label={showPassword ? "隐藏密码" : "显示密码"}
                  aria-pressed={showPassword}
                  onClick={() => setShowPassword((visible) => !visible)}
                >
                  {showPassword ? "隐藏" : "显示"}
                </button>
              </span>
            </div>

            <label className="auth-label">
              新密码
              <input
                className="auth-input"
                type={passwordType}
                value={newPassword}
                onChange={(event) => setNewPassword(event.target.value)}
                placeholder="至少 6 位"
                autoComplete="new-password"
                style={inputStyle}
              />
            </label>

            <label className="auth-label">
              确认新密码
              <input
                className="auth-input"
                type={passwordType}
                value={confirmPassword}
                onChange={(event) => setConfirmPassword(event.target.value)}
                placeholder="再次输入新密码"
                autoComplete="new-password"
                style={inputStyle}
              />
            </label>

            {error && (
              <div className="auth-error" role="alert">
                {error}
              </div>
            )}

            <button type="submit" className="auth-submit" disabled={busy}>
              {busy ? "修改中..." : "保存并进入"}
            </button>
          </form>
        </div>
      </div>
    </div>
  );
}
