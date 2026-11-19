import { useEffect, useState } from "react";
import AdminClient from "../../app/admin/AdminClient";
import AdminLoading from "../../app/admin/loading";
import { fetchCurrentUser, type ApiUser } from "../api";

/**
 * Static port of the original Next.js admin page.
 *
 * requireAdminUser() becomes GET /api/auth/me. The original behavior is kept:
 * an unauthenticated visitor is redirected to /dashboard (a real navigation to
 * the Go-served shell), a signed-in non-admin sees the "无权限访问" notice, and an
 * admin gets the unchanged AdminClient with all five tabs.
 */
type State =
  | { kind: "loading" }
  | { kind: "forbidden" }
  | { kind: "admin"; user: ApiUser }
  | { kind: "error"; message: string };

export function AdminPage() {
  const [state, setState] = useState<State>({ kind: "loading" });

  useEffect(() => {
    let active = true;
    void fetchCurrentUser().then((result) => {
      if (!active) return;
      if (result.state === "authenticated") {
        if (result.user.role === "ADMIN") {
          setState({ kind: "admin", user: result.user });
        } else {
          setState({ kind: "forbidden" });
        }
        return;
      }
      if (result.state === "anonymous" || result.state === "disabled") {
        // Same as redirect("/dashboard") in the Next page: a real navigation so
        // the dashboard re-reads the session from the Go binary.
        window.location.replace("/dashboard");
        return;
      }
      setState({ kind: "error", message: result.message });
    });
    return () => {
      active = false;
    };
  }, []);

  if (state.kind === "loading") return <AdminLoading />;
  if (state.kind === "forbidden") {
    return (
      <main style={{ padding: 24 }}>
        <h1 style={{ fontSize: 24, marginBottom: 12 }}>无权限访问</h1>
        <p style={{ color: "var(--ph-muted)" }}>当前账号不是管理员。</p>
      </main>
    );
  }
  if (state.kind === "error") {
    return (
      <main style={{ padding: 24 }}>
        <h1 style={{ fontSize: 24, marginBottom: 12 }}>无法加载管理后台</h1>
        <p style={{ color: "var(--ph-muted)" }}>{state.message}</p>
      </main>
    );
  }
  return <AdminClient user={state.user} />;
}
