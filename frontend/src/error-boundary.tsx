import { Component, Fragment, type ErrorInfo, type ReactNode } from "react";

/**
 * Port of the original app/error.tsx. Next.js wraps the page tree in an error
 * boundary and keeps the layout (footer, theme) mounted; the static bundle has
 * no such boundary, so an uncaught render error used to blank #root entirely.
 *
 * This boundary is mounted inside AppShell for every route, so a failing page
 * shows the original error copy with its two actions while the shell stays.
 * It catches render/lifecycle errors only: fetch failures are already handled
 * explicitly by each page instead of being thrown.
 */
export class RootErrorBoundary extends Component<
  { children: ReactNode },
  { error: Error | null; attempt: number }
> {
  state = { error: null as Error | null, attempt: 0 };

  static getDerivedStateFromError(error: unknown) {
    return { error: error instanceof Error ? error : new Error(String(error)) };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // The page already explains the failure to the user; keep the component
    // stack in the console so a production incident stays diagnosable.
    console.error("[PicBox] 页面渲染失败", error, info.componentStack);
  }

  private retry = (): void => {
    this.setState((state) => ({ error: null, attempt: state.attempt + 1 }));
  };

  render() {
    if (this.state.error) {
      return <ErrorPage error={this.state.error} onRetry={this.retry} />;
    }
    // The attempt key remounts the page on 重试 instead of resuming the tree
    // that threw. A page that fails permanently mounts, throws again, and is
    // caught here again — never an empty document.
    return <Fragment key={this.state.attempt}>{this.props.children}</Fragment>;
  }
}

// Same markup and copy as the original app/error.tsx, kept in this file for the
// same reason it lives in one file there.
function ErrorPage({ error, onRetry }: { error: Error; onRetry: () => void }) {
  return (
    <main
      style={{
        flex: 1,
        display: "flex",
        flexDirection: "column",
        alignItems: "center",
        justifyContent: "center",
        gap: 12,
        padding: 24,
        background: "var(--ph-bg)",
        color: "var(--ph-text)",
        textAlign: "center",
      }}
    >
      <h1 style={{ margin: 0, fontSize: 22, fontWeight: 700 }}>页面出错了</h1>
      <p style={{ margin: 0, color: "var(--ph-muted)" }}>
        {error.message || "发生了未预期的错误，请重试。"}
      </p>
      <div style={{ display: "flex", gap: 10, marginTop: 8 }}>
        <button
          type="button"
          onClick={onRetry}
          style={{
            padding: "9px 18px",
            borderRadius: 2,
            border: "none",
            background: "var(--ph-primary)",
            color: "#fff",
            fontSize: 14,
            cursor: "pointer",
          }}
        >
          重试
        </button>
        <a
          href="/"
          style={{
            padding: "9px 18px",
            borderRadius: 2,
            border: "1px solid var(--ph-border-strong)",
            color: "var(--ph-text-soft-strong)",
            fontSize: 14,
            textDecoration: "none",
          }}
        >
          返回首页
        </a>
      </div>
    </main>
  );
}
