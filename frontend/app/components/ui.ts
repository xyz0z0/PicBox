import type { CSSProperties } from "react";

/**
 * 全站共享的样式令牌。
 *
 * 颜色一律指向 globals.css 的 CSS 变量，避免工作台、管理端、首页各自维护一套硬编码色值。
 * 这里只放需要跨组件复用的基础样式；单个页面特有的布局仍写在各自文件中。
 */
export const colors = {
  primary: "var(--ph-primary)",
  primaryHover: "var(--ph-primary-strong)",
  danger: "var(--ph-danger)",
  dangerHover: "var(--ph-danger-strong)",
  border: "var(--ph-border)",
  bg: "var(--ph-bg)",
  card: "var(--ph-surface)",
  text: "var(--ph-text)",
  muted: "var(--ph-muted)",
  success: "var(--ph-success)",
} as const;

export type ButtonVariant = "primary" | "danger" | "outline" | "ghost";

export function buttonStyle(variant: ButtonVariant = "primary"): CSSProperties {
  const base: CSSProperties = {
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    gap: 6,
    padding: "7px 14px",
    borderRadius: 2,
    fontSize: 13,
    fontWeight: 600,
    cursor: "pointer",
    border: "none",
    transition: "background 0.15s, color 0.15s, border-color 0.15s",
    whiteSpace: "nowrap",
  };
  if (variant === "danger") {
    return { ...base, background: colors.danger, color: "#fff" };
  }
  if (variant === "outline") {
    return {
      ...base,
      background: "var(--ph-surface)",
      color: colors.primary,
      border: `1px solid ${colors.border}`,
    };
  }
  if (variant === "ghost") {
    return {
      ...base,
      background: "transparent",
      color: colors.muted,
      border: `1px solid ${colors.border}`,
    };
  }
  return { ...base, background: colors.primary, color: "var(--ph-primary-fg)" };
}

export const inputStyle: CSSProperties = {
  padding: "8px 12px",
  borderRadius: 2,
  border: `1px solid ${colors.border}`,
  fontSize: 14,
  width: "100%",
  boxSizing: "border-box",
  background: "var(--ph-surface)",
  color: "var(--ph-text)",
};

/** 管理端筛选/表单里与按钮同行的紧凑输入框。 */
export const compactInputStyle: CSSProperties = {
  padding: "8px 10px",
  border: `1px solid ${colors.border}`,
  borderRadius: 2,
  fontSize: 13,
  boxSizing: "border-box",
  background: "var(--ph-surface)",
  color: "var(--ph-text)",
};

export const cardStyle: CSSProperties = {
  background: colors.card,
  borderRadius: 2,
  border: `1px solid ${colors.border}`,
  padding: 20,
};

export const panelStyle: CSSProperties = {
  background: colors.card,
  border: `1px solid ${colors.border}`,
  borderRadius: 2,
  padding: 16,
  overflowX: "auto",
};

export function tableStyle(minWidth: number): CSSProperties {
  return {
    width: "100%",
    borderCollapse: "collapse",
    minWidth,
  };
}

export const thStyle: CSSProperties = {
  textAlign: "left",
  fontSize: 12,
  fontWeight: 600,
  color: colors.muted,
  background: "var(--ph-surface-muted)",
  borderBottom: `1px solid ${colors.border}`,
  padding: "10px 12px",
  whiteSpace: "nowrap",
};

export const tdStyle: CSSProperties = {
  fontSize: 13,
  color: colors.text,
  borderBottom: `1px solid ${colors.border}`,
  padding: "12px",
  verticalAlign: "middle",
};
