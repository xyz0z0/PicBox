export default function AdminLoading() {
  return (
    <main
      aria-busy="true"
      style={{
        flex: 1,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
        background: "var(--ph-bg)",
        color: "var(--ph-muted)",
        fontSize: 14,
      }}
    >
      正在加载管理后台…
    </main>
  );
}
