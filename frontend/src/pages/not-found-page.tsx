/**
 * Same markup as the original Next.js not-found page. That app gets it from the App Router
 * 404 boundary; the static bundle renders it for unknown paths and for image
 * detail pages whose id is missing or not visible.
 */
export function NotFoundPage() {
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
      <h1 style={{ margin: 0, fontSize: 40, fontWeight: 700, color: "var(--ph-primary)" }}>404</h1>
      <p style={{ margin: 0, color: "var(--ph-muted)" }}>页面不存在，或图片已被删除 / 不可见。</p>
      <a
        href="/"
        style={{
          marginTop: 8,
          padding: "9px 18px",
          borderRadius: 2,
          background: "var(--ph-primary)",
          color: "#fff",
          fontSize: 14,
          textDecoration: "none",
        }}
      >
        返回首页
      </a>
    </main>
  );
}
