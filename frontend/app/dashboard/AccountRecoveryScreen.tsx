"use client";

import { useEffect, useRef, useState } from "react";
import { inputStyle } from "@/app/components/ui";
import { fetchRuntimeConfig } from "@/src/api";

export type AccountRecoveryMode = "forgot" | "reset" | "verify";

const FORGOT_SUCCESS = "如果该邮箱对应可用账号，你将收到密码重置邮件。请检查收件箱和垃圾邮件。";
const RESET_SUCCESS = "密码已重置，请重新登录。";
const VERIFY_SUCCESS = "登录邮箱已更新，请重新登录。";

export default function AccountRecoveryScreen({ mode }: { mode: AccountRecoveryMode }) {
  const fragmentModeRef = useRef<AccountRecoveryMode | null>(null);
  const [token, setToken] = useState("");
  const [tokenReady, setTokenReady] = useState(mode === "forgot");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [errorLink, setErrorLink] = useState<"forgot" | "dashboard" | "account-settings" | null>(null);
  const [success, setSuccess] = useState(false);
  const [busy, setBusy] = useState(false);

  // 只有 "forgot" 需要先确认本部署能不能发信：未配置 SMTP/BASEURL 时提交该
  // 表单只会得到 503 MAIL_UNAVAILABLE，页面必须直接指向管理员，而不是把用户
  // 送进一个必然失败的流程。reset/verify 是从已发出的邮件链接进来的，保持原样。
  const [mailState, setMailState] = useState<"loading" | "enabled" | "disabled">(
    mode === "forgot" ? "loading" : "enabled",
  );

  useEffect(() => {
    if (mode !== "forgot") return;
    let active = true;
    void (async () => {
      const result = await fetchRuntimeConfig();
      if (!active) return;
      // 配置读取本身失败时不推断“邮件不可用”：保持原表单，由提交结果说明。
      setMailState(result.state === "ok" && !result.mailEnabled ? "disabled" : "enabled");
    })();
    return () => {
      active = false;
    };
  }, [mode]);

  useEffect(() => {
    if (mode === "forgot") return;
    if (fragmentModeRef.current === mode) return;
    fragmentModeRef.current = mode;
    const fragment = new URLSearchParams(window.location.hash.replace(/^#/, ""));
    const value = fragment.get("token")?.trim() ?? "";
    setToken(value);
    if (window.location.hash) {
      window.history.replaceState(
        window.history.state,
        "",
        `${window.location.pathname}${window.location.search}`
      );
    }
    setTokenReady(true);
  }, [mode]);

  const title = mode === "forgot"
    ? "找回密码"
    : mode === "reset"
      ? "重置密码"
      : "确认新邮箱";
  const description = mode === "forgot"
    ? "输入注册时使用的邮箱地址。"
    : mode === "reset"
      ? "请设置一个新密码，至少 6 位。"
      : "确认后，新邮箱将成为你的登录邮箱。";

  function showError(message: string, link: "forgot" | "dashboard" | "account-settings" | null = null) {
    setError(message);
    setErrorLink(link);
  }

  async function submit(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setError(null);
    setErrorLink(null);

    if (mode === "forgot") {
      const normalizedEmail = email.trim().toLowerCase();
      if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(normalizedEmail)) {
        showError("请输入有效的邮箱地址");
        return;
      }
      await request("/api/auth/forgot-password", { email: normalizedEmail });
      return;
    }

    if (!token) {
      showError(mode === "reset"
        ? "重置链接无效或已过期，请重新申请。"
        : "确认链接无效或已过期，请登录后重新发起邮箱修改。", mode === "reset" ? "forgot" : "dashboard");
      return;
    }

    if (mode === "reset") {
      if (password.length < 6) {
        showError("新密码至少 6 位");
        return;
      }
      if (password !== confirmPassword) {
        showError("两次输入的新密码不一致");
        return;
      }
      await request("/api/auth/reset-password", { token, password });
      return;
    }

    // 邮箱变更必须由用户明确点击确认按钮，不在页面加载时自动提交。
    await request("/api/auth/change-email/confirm", { token });
  }

  async function request(endpoint: string, payload: Record<string, string>) {
    setBusy(true);
    try {
      const response = await fetch(endpoint, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(payload),
      });
      const data = await response.json().catch(() => ({}));
      if (!response.ok) {
        const code = data?.error?.code;
        if (mode === "forgot") {
          showError(response.status === 429
            ? "请求过于频繁，请稍后重试。"
            : response.status === 503
              ? "暂时无法发送邮件，请稍后重试或联系管理员。"
              : "暂时无法发送邮件，请稍后重试。");
          return;
        }
        if (mode === "reset") {
          if (code === "INVALID_TOKEN") {
            showError("重置链接无效或已过期，请重新申请。", "forgot");
          } else if (code === "INVALID_PASSWORD") {
            showError("新密码至少 6 位");
          } else if (code === "SAME_PASSWORD") {
            showError("新密码不能与原密码相同，请设置一个新密码。");
          } else if (response.status === 429) {
            showError("请求过于频繁，请稍后重试。");
          } else {
            showError("无法重置密码，请稍后重试。");
          }
          return;
        }
        if (response.status === 401) {
          showError("请先登录后重新打开邮件中的确认链接。", "dashboard");
        } else if (code === "INVALID_TOKEN") {
          showError("确认链接无效或已过期，请登录后重新发起邮箱修改。", "account-settings");
        } else if (response.status === 409 && code === "EMAIL_EXISTS") {
          showError("该新邮箱已被其他账号使用，请回账号设置更换邮箱后重新申请。", "account-settings");
        } else if (response.status === 429) {
          showError("请求过于频繁，请稍后重试。");
        } else {
          showError("无法确认新邮箱，请稍后重试。");
        }
        return;
      }
      setSuccess(true);
      setPassword("");
      setConfirmPassword("");
    } catch {
      showError(mode === "forgot"
        ? "网络异常，暂时无法发送邮件，请稍后重试。"
        : mode === "reset"
          ? "网络异常，密码未重置，请稍后重试。"
          : "网络异常，邮箱尚未更新，请稍后重试。");
    } finally {
      setBusy(false);
    }
  }

  const missingToken = mode !== "forgot" && tokenReady && !token;
  const errorLinkHref = errorLink === "forgot" ? "/forgot-password" : "/dashboard";
  const errorLinkLabel = errorLink === "forgot"
    ? "重新申请重置邮件"
    : errorLink === "account-settings"
      ? "返回账号设置"
      : "先去登录";

  return (
    <main className="auth-root">
      <div className="auth-shell">
        <aside className="auth-aside">
          <a className="auth-brand" href="/">PicBox</a>
          <div>
            <h2>把喜欢的图片，好好收在一起</h2>
            <p>图片保持原样，用标签整理，误删也能找回。</p>
            <ul className="auth-points">
              <li>原图保存，不被压缩</li>
              <li>标签整理，快速筛选</li>
              <li>链接分享，打包下载</li>
            </ul>
          </div>
          <p style={{ fontSize: 12, color: "rgb(255 255 255 / 62%)" }}>
            自托管的私人图床
          </p>
        </aside>

        <section className="auth-card">
          <a className="auth-mobile-brand" href="/">PicBox</a>
          <div className="auth-card-head">
            <h1>{title}</h1>
            <p>{description}</p>
          </div>

          {success ? (
            <div className="auth-form">
              <p role="status" className="image-notice success">
                {mode === "forgot" ? FORGOT_SUCCESS : mode === "reset" ? RESET_SUCCESS : VERIFY_SUCCESS}
              </p>
              <a className="auth-submit" href="/dashboard" style={{ display: "block", textAlign: "center", textDecoration: "none" }}>
                返回登录
              </a>
            </div>
          ) : mode === "forgot" && mailState === "loading" ? (
            <p role="status" className="auth-note">正在检查邮件服务…</p>
          ) : mode === "forgot" && mailState === "disabled" ? (
            <p role="status" className="image-notice error">
              当前部署未启用邮件服务，无法发送重置邮件。请联系管理员重置密码。
            </p>
          ) : !tokenReady ? (
            <p role="status" className="auth-note">正在打开确认页面…</p>
          ) : missingToken ? (
            <div className="auth-form">
              <p className="auth-note">
                {mode === "reset"
                  ? "此重置链接无效或不完整。"
                  : "此确认链接无效或不完整。请登录后重新发起邮箱修改。"}
              </p>
              <a className="auth-submit" href={mode === "reset" ? "/forgot-password" : "/dashboard"} style={{ display: "block", textAlign: "center", textDecoration: "none" }}>
                {mode === "reset" ? "重新申请重置邮件" : "返回登录"}
              </a>
            </div>
          ) : mode === "verify" ? (
            <form className="auth-form" noValidate onSubmit={(event) => void submit(event)}>
              {error && <div className="auth-error" role="alert">{error}</div>}
              {errorLink && <a className="auth-switch-link" href={errorLinkHref}>{errorLinkLabel}</a>}
              <button type="submit" className="auth-submit" disabled={busy}>
                {busy ? "确认中…" : "确认修改邮箱"}
              </button>
            </form>
          ) : (
            <form className="auth-form" noValidate onSubmit={(event) => void submit(event)}>
              {mode === "forgot" ? (
                <label className="auth-label">
                  邮箱
                  <input
                    className="auth-input"
                    type="email"
                    autoComplete="email"
                    autoFocus
                    value={email}
                    disabled={busy}
                    onChange={(event) => setEmail(event.target.value)}
                    style={inputStyle}
                  />
                </label>
              ) : (
                <>
                  <label className="auth-label">
                    新密码
                    <input
                      className="auth-input"
                      type="password"
                      autoComplete="new-password"
                      value={password}
                      disabled={busy}
                      onChange={(event) => setPassword(event.target.value)}
                      style={inputStyle}
                    />
                  </label>
                  <label className="auth-label">
                    确认新密码
                    <input
                      className="auth-input"
                      type="password"
                      autoComplete="new-password"
                      value={confirmPassword}
                      disabled={busy}
                      onChange={(event) => setConfirmPassword(event.target.value)}
                      style={inputStyle}
                    />
                  </label>
                </>
              )}
              {error && <div className="auth-error" role="alert">{error}</div>}
              {errorLink && <a className="auth-switch-link" href={errorLinkHref}>{errorLinkLabel}</a>}
              <button type="submit" className="auth-submit" disabled={busy}>
                {busy
                  ? mode === "forgot" ? "发送中…" : "重置中…"
                  : mode === "forgot" ? "发送重置邮件" : "重置密码"}
              </button>
            </form>
          )}

          {!success && !missingToken && (
            <p className="auth-switch" style={{ marginTop: 14 }}>
              <a className="auth-switch-link" href="/dashboard">返回登录</a>
            </p>
          )}
        </section>
      </div>
    </main>
  );
}
