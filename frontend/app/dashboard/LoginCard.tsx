"use client";

import { useRef, useState } from "react";
import { useRouter } from "next/navigation";

type Mode = "login" | "register";
const MODES: Mode[] = ["login", "register"];

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

export default function LoginCard({
  mailEnabled,
}: {
  // Optional so the original Next page can keep mounting this component with no
  // props.
  //   false: SMTP/BASEURL are not configured in this deployment, so the reset
  //          flow can only answer 503 MAIL_UNAVAILABLE -> point at the admin.
  //   true / undefined: keep the original "忘记密码？" link (undefined = the
  //          config read failed, so the capability is unknown).
  mailEnabled?: boolean;
}) {
  const router = useRouter();
  const [mode, setMode] = useState<Mode>("login");
  const [showPassword, setShowPassword] = useState(false);
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [activationCode, setActivationCode] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const tabRefs = useRef<Array<HTMLButtonElement | null>>([]);

  function switchMode(next: Mode) {
    // Tabs are disabled while a request is in flight and this guard is the
    // second line of defence (keyboard handlers, programmatic clicks): the
    // submitted credentials may already have set a login cookie, so the card
    // must not swap to the other mode mid-request.
    if (loading) return;
    setMode(next);
    setError(null);
    setActivationCode("");
    setShowPassword(false);
  }

  function handleTabKeyDown(
    event: React.KeyboardEvent<HTMLButtonElement>,
    index: number,
  ) {
    let nextIndex: number | null = null;
    if (event.key === "ArrowRight") {
      nextIndex = (index + 1) % MODES.length;
    } else if (event.key === "ArrowLeft") {
      nextIndex = (index - 1 + MODES.length) % MODES.length;
    } else if (event.key === "Home") {
      nextIndex = 0;
    } else if (event.key === "End") {
      nextIndex = MODES.length - 1;
    }

    if (nextIndex !== null) {
      event.preventDefault();
      switchMode(MODES[nextIndex]);
      tabRefs.current[nextIndex]?.focus();
    }
  }

  async function onSubmit() {
    setError(null);
    if (!email.trim()) {
      setError("请输入邮箱");
      return;
    }
    if (!password) {
      setError("请输入密码");
      return;
    }

    setLoading(true);
    try {
      const endpoint =
        mode === "login" ? "/api/auth/login" : "/api/auth/register";
      const payload: {
        email: string;
        password: string;
        activationCode?: string;
      } = {
        email: email.trim().toLowerCase(),
        password,
      };
      if (mode === "register" && activationCode.trim()) {
        payload.activationCode = activationCode.trim();
      }

      const res = await fetch(endpoint, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(payload),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setError(data?.error?.message || data?.message || "请求失败，请重试");
        return;
      }
      router.refresh();
    } catch {
      // Offline / DNS / aborted body: show a plain retryable message instead of
      // leaking the fetch error text, and keep the form usable.
      setError("网络异常，请检查网络连接后重试。");
    } finally {
      setLoading(false);
    }
  }

  return (
    <div className="auth-root">
      <div className="auth-shell">
        <aside className="auth-aside">
          <a className="auth-brand" href="/">
            PicBox
          </a>
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

        <div className="auth-card">
          <a className="auth-mobile-brand" href="/">
            PicBox
          </a>
          <div className="auth-card-head">
            <h1>{mode === "login" ? "欢迎回来" : "创建账号"}</h1>
            <p>
              {mode === "login"
                ? "使用邮箱和密码登录。"
                : "首个账号无需激活码，之后请向管理员获取。"}
            </p>
          </div>

          <div className="auth-tabs" role="tablist" aria-label="登录与注册">
            {MODES.map((item, index) => (
              <button
                key={item}
                ref={(element) => {
                  tabRefs.current[index] = element;
                }}
                type="button"
                role="tab"
                id={`auth-tab-${item}`}
                aria-controls="auth-panel"
                aria-selected={mode === item}
                tabIndex={mode === item ? 0 : -1}
                disabled={loading}
                className={`auth-tab${mode === item ? " is-active" : ""}`}
                onClick={() => switchMode(item)}
                onKeyDown={(event) => handleTabKeyDown(event, index)}
              >
                {item === "login" ? "登录" : "注册"}
              </button>
            ))}
          </div>

          <div
            id="auth-panel"
            role="tabpanel"
            aria-labelledby={`auth-tab-${mode}`}
            tabIndex={-1}
          >
            <form
              className="auth-form"
              onSubmit={(event) => {
                event.preventDefault();
                void onSubmit();
              }}
            >
              <label className="auth-label">
                邮箱
                <input
                  className="auth-input"
                  type="email"
                  value={email}
                  onChange={(event) => setEmail(event.target.value)}
                  placeholder="you@example.com"
                  autoComplete="email"
                  autoFocus
                  style={inputStyle}
                />
              </label>

              <div className="auth-label">
                <div
                  className="auth-password-head"
                  style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8 }}
                >
                  <label htmlFor="auth-password">密码</label>
                  {mode === "login" && mailEnabled !== false && (
                    <a className="auth-switch-link" href="/forgot-password">
                      忘记密码？
                    </a>
                  )}
                </div>
                <span className="auth-password-field">
                  <input
                    id="auth-password"
                    className="auth-input auth-password-input"
                    type={showPassword ? "text" : "password"}
                    value={password}
                    onChange={(event) => setPassword(event.target.value)}
                    placeholder={mode === "register" ? "至少 6 位" : "请输入密码"}
                    autoComplete={
                      mode === "login" ? "current-password" : "new-password"
                    }
                    aria-describedby={
                      mode === "login" && mailEnabled === false
                        ? "auth-password-note"
                        : undefined
                    }
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
                {mode === "login" && mailEnabled === false && (
                  <span className="auth-note" id="auth-password-note">
                    忘记密码？邮件服务未启用，请联系管理员重置密码。
                  </span>
                )}
              </div>

              {mode === "register" && (
                <label className="auth-label">
                  激活码
                  <input
                    className="auth-input"
                    value={activationCode}
                    onChange={(event) => setActivationCode(event.target.value)}
                    placeholder="首个账号可留空"
                    autoComplete="off"
                    spellCheck={false}
                    style={inputStyle}
                  />
                  <span className="auth-note">
                    首个账号无需激活码，之后请向管理员获取。
                  </span>
                </label>
              )}

              {error && (
                <div className="auth-error" role="alert">
                  {error}
                </div>
              )}

              <button type="submit" className="auth-submit" disabled={loading}>
                {loading
                  ? "处理中..."
                  : mode === "login"
                  ? "登录"
                  : "注册账号"}
              </button>

              {mode === "login" && (
                <p className="auth-switch">
                  还没有账号？
                  <button
                    type="button"
                    className="auth-switch-link"
                    disabled={loading}
                    onClick={() => switchMode("register")}
                  >
                    立即注册
                  </button>
                </p>
              )}
            </form>
          </div>
        </div>
      </div>
    </div>
  );
}
