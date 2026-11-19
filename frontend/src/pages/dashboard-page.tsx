import { useEffect, useState } from "react";
import DashboardClient from "../../app/dashboard/DashboardClient";
import LoginCard from "../../app/dashboard/LoginCard";
import DashboardLoading from "../../app/dashboard/loading";
import { fetchCurrentUser, fetchRuntimeConfig, type ApiUser } from "../api";

/**
 * Static port of the original Next.js dashboard page.
 *
 * requireCurrentUser() becomes GET /api/auth/me; the three branches keep the
 * original rendering: disabled notice, LoginCard, DashboardClient. DashboardClient
 * (including its dialogs, lightbox, API key tab and change-password flow) is the
 * original component, unchanged.
 *
 * The upload ceiling is runtime configuration in the Go deployment
 * (UPLOAD_MAX_BYTES), so it is read from GET /api/config and passed to the
 * original component as an optional prop. A missing or invalid config is an
 * explicit error with a retry action: silently falling back to the build-time
 * 5MiB default would either block legitimate larger files or show a wrong hint.
 *
 * The same response carries mailEnabled (SMTP + BASEURL configured). It only
 * gates the "forgot password" / "change email" entries; registration stays on
 * activation codes and never depends on mail. A failed config read on the
 * anonymous branch degrades to "unknown" (original links) instead of turning
 * the login page into an error page.
 */
type State =
  | { kind: "loading" }
  | { kind: "disabled" }
  | { kind: "login"; mailEnabled: boolean | undefined }
  | {
      kind: "workspace";
      user: ApiUser;
      uploadMaxBytes: number;
      mailEnabled: boolean;
    }
  | { kind: "error"; message: string };

export function DashboardPage() {
  const [state, setState] = useState<State>({ kind: "loading" });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let active = true;
    setState({ kind: "loading" });

    void (async () => {
      const userResult = await fetchCurrentUser();
      if (!active) return;

      if (userResult.state === "anonymous") {
        // Only needed to decide whether the "忘记密码" entry can work at all.
        const configResult = await fetchRuntimeConfig();
        if (!active) return;
        setState({
          kind: "login",
          mailEnabled: configResult.state === "ok" ? configResult.mailEnabled : undefined,
        });
        return;
      }
      if (userResult.state === "disabled") {
        setState({ kind: "disabled" });
        return;
      }
      if (userResult.state === "error") {
        setState({ kind: "error", message: userResult.message });
        return;
      }

      const configResult = await fetchRuntimeConfig();
      if (!active) return;
      if (configResult.state === "error") {
        setState({ kind: "error", message: `无法读取上传限制：${configResult.message}` });
        return;
      }
      setState({
        kind: "workspace",
        user: userResult.user,
        uploadMaxBytes: configResult.uploadMaxBytes,
        mailEnabled: configResult.mailEnabled,
      });
    })();

    return () => {
      active = false;
    };
  }, [attempt]);

  switch (state.kind) {
    case "loading":
      return <DashboardLoading />;
    case "disabled":
      return (
        <main style={{ padding: 24 }}>
          <h1 style={{ fontSize: 24, marginBottom: 12 }}>账号已禁用</h1>
          <p style={{ color: "var(--ph-muted)" }}>请联系管理员处理。</p>
        </main>
      );
    case "login":
      return <LoginCard mailEnabled={state.mailEnabled} />;
    case "error":
      return (
        <main style={{ padding: 24 }}>
          <h1 style={{ fontSize: 24, marginBottom: 12 }}>无法加载工作台</h1>
          <p style={{ color: "var(--ph-muted)" }}>{state.message}</p>
          <button
            type="button"
            onClick={() => setAttempt((current) => current + 1)}
            style={{
              marginTop: 12,
              padding: "7px 14px",
              borderRadius: 2,
              border: "1px solid var(--ph-border)",
              background: "var(--ph-surface)",
              color: "var(--ph-text)",
              fontSize: 13,
              cursor: "pointer",
            }}
          >
            重试
          </button>
        </main>
      );
    case "workspace":
      return (
        <DashboardClient
          user={state.user}
          uploadMaxBytes={state.uploadMaxBytes}
          mailEnabled={state.mailEnabled}
          // Next 的 router.refresh() 会保留客户端 state；静态包的 refresh 是整页
          // reload，会把改密成功提示清掉。改密后无需重新读取用户资料，跳过这次
          // reload 即可；登录/退出等其它 refresh 仍由原组件调用。
          refreshAfterPasswordChange={false}
        />
      );
  }
}
