"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { formatBytes } from "@/lib/quota";
import { writeClipboardText } from "@/lib/clipboardText";
import { AppDialog, ConfirmDialog } from "@/app/components/Dialogs";
import {
  colors,
  buttonStyle as btn,
  panelStyle,
  tableStyle,
  thStyle,
  tdStyle,
  compactInputStyle,
} from "@/app/components/ui";
import ActivationCodesTab from "./ActivationCodesTab";

type AdminUser = {
  id: number;
  email: string | null;
  username: string;
  role: "USER" | "ADMIN";
  disabledAt: string | null;
  createdAt: string;
  storageUsedBytes?: number;
  quotaBytes?: number;
  quotaOverrideBytes?: number | null;
  _count?: {
    images: number;
    apiKeys: number;
  };
};

type AdminImage = {
  id: string;
  originalName: string;
  createdAt: string;
  isVisible: boolean;
  uploader: {
    id: number;
    email: string | null;
    username: string;
  };
  url: string;
  pageUrl: string;
  thumbUrl: string;
};

type AdminPageUser = {
  id: number;
  email: string | null;
  username: string;
  role: "USER" | "ADMIN";
};

type AdminAuditLog = {
  id: number;
  action:
    | "ADMIN_LOGIN"
    | "ADMIN_USER_CREATE"
    | "ADMIN_USER_DISABLE"
    | "ADMIN_USER_ENABLE"
    | "ADMIN_USER_PASSWORD_RESET"
    | "ADMIN_USER_EXPORT_COOLDOWN_RESET"
    | "ADMIN_USER_QUOTA_UPDATE"
    | "ADMIN_USER_EMAIL_UPDATE"
    | "ADMIN_ACTIVATION_CODE_CREATE"
    | "ADMIN_ACTIVATION_CODE_REVOKE"
    | "ACTIVATION_CODE_USED"
    | "ADMIN_IMAGE_HIDE"
    | "ADMIN_IMAGE_SHOW"
    | "ADMIN_IMAGE_DELETE";
  detail: string | null;
  createdAt: string;
  actor: {
    id: number;
    email: string | null;
    username: string;
  };
  targetUser: {
    id: number;
    email: string | null;
    username: string;
  } | null;
};

const ADMIN_TABS = [
  { key: "overview", label: "总览" },
  { key: "users", label: "用户管理" },
  { key: "codes", label: "激活码" },
  { key: "images", label: "图片管理" },
  { key: "audit", label: "审计日志" },
] as const;

type AdminTab = (typeof ADMIN_TABS)[number]["key"];

const AUDIT_ACTION_OPTIONS: Array<{ value: AdminAuditLog["action"]; label: string }> = [
  { value: "ADMIN_LOGIN", label: "管理员登录" },
  { value: "ADMIN_USER_CREATE", label: "创建用户" },
  { value: "ADMIN_USER_DISABLE", label: "禁用用户" },
  { value: "ADMIN_USER_ENABLE", label: "启用用户" },
  { value: "ADMIN_USER_PASSWORD_RESET", label: "重置用户密码" },
  { value: "ADMIN_USER_EXPORT_COOLDOWN_RESET", label: "解除导出限制" },
  { value: "ADMIN_USER_QUOTA_UPDATE", label: "调整存储配额" },
  { value: "ADMIN_USER_EMAIL_UPDATE", label: "修改登录邮箱" },
  { value: "ADMIN_ACTIVATION_CODE_CREATE", label: "生成激活码" },
  { value: "ADMIN_ACTIVATION_CODE_REVOKE", label: "撤销激活码" },
  { value: "ACTIVATION_CODE_USED", label: "使用激活码" },
  { value: "ADMIN_IMAGE_HIDE", label: "隐藏图片" },
  { value: "ADMIN_IMAGE_SHOW", label: "显示图片" },
  { value: "ADMIN_IMAGE_DELETE", label: "删除图片" },
];

const ADMIN_USERS_PAGE_SIZE = 20;
const BYTES_PER_MB = 1024 * 1024;
const BYTES_PER_GB = 1024 * BYTES_PER_MB;

function displayUser(user: { email: string | null; username: string }) {
  return user.email ?? user.username;
}

function isGeneratedUsername(username: string) {
  return /^user_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(
    username
  );
}

function formatDate(value: string) {
  return new Date(value).toLocaleString("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// 审计 detail 多为 key=value 片段，转成冒号形式便于阅读。
function formatDetail(detail: string | null): string {
  if (!detail) return "-";
  return detail.replace(/([A-Za-z_][A-Za-z0-9_]*)=/g, "$1: ");
}

export default function AdminClient({ user }: { user: AdminPageUser }) {
  const [tab, setTab] = useState<AdminTab>("overview");

  // 首次渲染与 SSR 都用默认 tab，mount 后再从 URL 同步，避免 hydration 不一致。
  useEffect(() => {
    const value = new URLSearchParams(window.location.search).get("tab");
    if (ADMIN_TABS.some((item) => item.key === value)) setTab(value as AdminTab);
  }, []);

  function switchTab(next: AdminTab) {
    setTab(next);
    const url = new URL(window.location.href);
    if (next === "overview") url.searchParams.delete("tab");
    else url.searchParams.set("tab", next);
    window.history.replaceState(null, "", url);
  }

  return (
    <div style={{ background: colors.bg }}>
      <header className="app-header">
        <div className="app-header-inner">
          <a className="app-brand" href="/admin">
            PicBox <span className="app-brand-sub">Admin</span>
          </a>
          <nav className="app-nav">
            <span className="app-user">
              <svg className="app-user-icon" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <circle cx="12" cy="8" r="3.5" />
                <path d="M4.5 20c.5-3.5 3.3-5.5 7.5-5.5s7 2 7.5 5.5" />
              </svg>
              <span className="app-user-name">{displayUser(user)}</span>
            </span>
            <a className="app-nav-link" href="/dashboard">
              返回工作台
            </a>
          </nav>
        </div>
      </header>

      <main className="app-main">
        <div className="app-tabs" role="tablist">
          {ADMIN_TABS.map((item) => (
            <button
              key={item.key}
              type="button"
              role="tab"
              aria-selected={tab === item.key}
              className={`app-tab${tab === item.key ? " is-active" : ""}`}
              onClick={() => switchTab(item.key)}
            >
              {item.label}
            </button>
          ))}
        </div>

        {tab === "overview" ? <AdminOverviewTab onNavigate={switchTab} /> : null}
        {tab === "users" ? <AdminUsersTab currentUserId={user.id} /> : null}
        {tab === "codes" ? <ActivationCodesTab /> : null}
        {tab === "images" ? <AdminImagesTab /> : null}
        {tab === "audit" ? <AdminAuditLogsTab /> : null}
      </main>
    </div>
  );
}

type AdminStats = {
  userCount: number;
  adminCount: number;
  disabledCount: number;
  imageCount: number;
  storageUsedBytes: number;
  todayUploadCount: number;
  quotaAlertCount: number;
  quotaTop: Array<{
    id: number;
    label: string;
    usedBytes: number;
    quotaBytes: number;
    ratio: number;
    exceeded: boolean;
  }>;
};

function AdminOverviewTab({ onNavigate }: { onNavigate: (tab: AdminTab) => void }) {
  const [stats, setStats] = useState<AdminStats | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await fetch("/api/admin/stats");
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(data?.error?.message || "加载总览数据失败");
      setStats(data as AdminStats);
    } catch (e) {
      setError(e instanceof Error ? e.message : "加载总览数据失败");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  if (loading && !stats) return <p style={{ color: colors.muted }}>正在加载总览...</p>;
  if (error && !stats) {
    return (
      <div>
        <p style={{ color: colors.danger }}>{error}</p>
        <button style={btn("outline")} onClick={() => void load()}>重试</button>
      </div>
    );
  }
  if (!stats) return null;

  const cards: Array<{ label: string; value: string; note?: string; alert?: boolean }> = [
    { label: "账号总数", value: String(stats.userCount), note: `其中管理员 ${stats.adminCount} 个` },
    { label: "图片总数", value: String(stats.imageCount), note: "不含回收站与已下架" },
    { label: "存储用量", value: formatBytes(stats.storageUsedBytes), note: "含回收站与已下架图片" },
    { label: "今日上传", value: String(stats.todayUploadCount) },
    { label: "禁用账号", value: String(stats.disabledCount), alert: stats.disabledCount > 0 },
    { label: "配额告急", value: String(stats.quotaAlertCount), note: "用量达到配额 80%", alert: stats.quotaAlertCount > 0 },
  ];

  return (
    <div>
      <div className="admin-overview-head">
        <div>
          <h2>站点总览</h2>
          <p>账号、图片与存储的整体情况。</p>
        </div>
        <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
          {error && <span role="alert" style={{ fontSize: 12, color: colors.danger }}>{error}</span>}
          <button style={btn("outline")} disabled={loading} onClick={() => void load()}>
            {loading ? "刷新中…" : "刷新"}
          </button>
        </div>
      </div>

      <div className="admin-stats-grid">
        {cards.map((card) => (
          <div className="admin-stat-card" key={card.label}>
            <div className="admin-stat-label">{card.label}</div>
            <div className={`admin-stat-value${card.alert ? " is-alert" : ""}`}>{card.value}</div>
            {card.note && <div className="admin-stat-note">{card.note}</div>}
          </div>
        ))}
      </div>

      <div className="admin-quota-panel">
        <h3>配额使用情况</h3>
        {stats.quotaTop.length === 0 ? (
          <p style={{ margin: 0, fontSize: 13, color: colors.muted }}>暂无达到配额 80% 的账号。</p>
        ) : (
          <>
            {stats.quotaTop.map((row) => (
              <div className="admin-quota-row" key={row.id}>
                <span className="admin-quota-name" title={row.label}>{row.label}</span>
                <span className="admin-quota-bar">
                  <i
                    className={row.exceeded ? "is-alert" : undefined}
                    style={{ width: `${Math.min(100, Math.round(row.ratio * 100))}%` }}
                  />
                </span>
                <span className="admin-quota-value">
                  {formatBytes(row.usedBytes)} / {formatBytes(row.quotaBytes)} · {Math.round(row.ratio * 100)}%
                </span>
                {row.exceeded && <span className="admin-quota-badge">已超限</span>}
              </div>
            ))}
            <button style={{ ...btn("ghost"), marginTop: 12 }} onClick={() => onNavigate("users")}>
              查看全部账号 →
            </button>
          </>
        )}
      </div>
    </div>
  );
}

function AdminUsersTab({ currentUserId }: { currentUserId: number }) {
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [page, setPage] = useState(1);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(1);
  const [defaultQuotaBytes, setDefaultQuotaBytes] = useState(0);
  const [notice, setNotice] = useState<{
    type: "success" | "warn" | "error";
    text: string;
  } | null>(null);

  const [editingQuotaId, setEditingQuotaId] = useState<number | null>(null);
  const [quotaDraft, setQuotaDraft] = useState("");
  const [quotaUnit, setQuotaUnit] = useState<"MB" | "GB">("MB");
  const [savingQuotaId, setSavingQuotaId] = useState<number | null>(null);

  // 行级「更多」菜单：同一时刻只开一个，fixed 定位避免被表格横向滚动容器裁剪。
  const [openMenu, setOpenMenu] = useState<{ id: number; top: number; left: number } | null>(null);
  const menuTriggerRef = useRef<HTMLButtonElement | null>(null);

  function toggleRowMenu(trigger: HTMLButtonElement, id: number) {
    if (openMenu?.id === id) {
      setOpenMenu(null);
      return;
    }
    const rect = trigger.getBoundingClientRect();
    const width = 172;
    const estimatedHeight = 4 * 37 + 10;
    const left = Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8));
    const below = rect.bottom + 6;
    const top = below + estimatedHeight > window.innerHeight
      ? Math.max(8, rect.top - estimatedHeight - 6)
      : below;
    setOpenMenu({ id, top, left });
  }

  const [passwordTarget, setPasswordTarget] = useState<AdminUser | null>(null);
  const [password, setPassword] = useState("");
  const [passwordConfirm, setPasswordConfirm] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [passwordBusy, setPasswordBusy] = useState(false);
  const [passwordError, setPasswordError] = useState<string | null>(null);

  const [userActionTarget, setUserActionTarget] = useState<{
    user: AdminUser;
    operation: "disable" | "enable";
  } | null>(null);
  const [userActionBusy, setUserActionBusy] = useState(false);
  const [userActionError, setUserActionError] = useState<string | null>(null);

  const [cooldownTarget, setCooldownTarget] = useState<AdminUser | null>(null);
  const [cooldownBusy, setCooldownBusy] = useState(false);
  const [cooldownError, setCooldownError] = useState<string | null>(null);

  const [emailTarget, setEmailTarget] = useState<AdminUser | null>(null);
  const [emailDraft, setEmailDraft] = useState("");
  const [emailBusy, setEmailBusy] = useState(false);
  const [emailError, setEmailError] = useState<string | null>(null);

  // 页面级「创建用户」：表单弹窗 + 一次性临时密码结果弹窗。
  const [createOpen, setCreateOpen] = useState(false);
  const [createEmail, setCreateEmail] = useState("");
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [createResult, setCreateResult] = useState<{ email: string; temporaryPassword: string } | null>(null);
  const [createCopied, setCreateCopied] = useState(false);

  const fetchUsers = useCallback(async (targetPage: number, search: string) => {
    const params = new URLSearchParams({
      page: String(targetPage),
      limit: String(ADMIN_USERS_PAGE_SIZE),
    });
    if (search) params.set("q", search);
    const res = await fetch(`/api/admin/users?${params.toString()}`);
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(data?.error?.message || "加载用户失败");
    }
    return {
      users: (data.users ?? []) as AdminUser[],
      total: Number(data.total) || 0,
      page: Number(data.page) || 1,
      totalPages: Number(data.totalPages) || 1,
      defaultQuotaBytes:
        Number(data.defaultQuotaBytes ?? data.quotaBytes) || 0,
    };
  }, []);

  const load = useCallback(
    async (targetPage: number, search: string) => {
      setLoading(true);
      setError(null);
      try {
        const data = await fetchUsers(targetPage, search);
        setUsers(data.users);
        setTotal(data.total);
        setPage(data.page);
        setTotalPages(data.totalPages);
        setDefaultQuotaBytes(data.defaultQuotaBytes);
      } catch (e: any) {
        setError(e?.message || "加载用户失败");
      } finally {
        setLoading(false);
      }
    },
    [fetchUsers]
  );

  useEffect(() => {
    void load(page, query);
  }, [load, page, query]);

  useEffect(() => {
    const next = queryInput.trim();
    const timer = window.setTimeout(() => {
      setQuery(next);
      setPage(1);
    }, 300);
    return () => window.clearTimeout(timer);
  }, [queryInput]);

  // 更多菜单：点外部 / Esc 关闭并把焦点还给触发按钮；滚动或缩放时关闭，
  // 避免 fixed 菜单停留在过期的坐标上。
  useEffect(() => {
    if (!openMenu) return;
    function onPointerDown(event: PointerEvent) {
      const target = event.target;
      if (!(target instanceof Element) || !target.closest("[data-admin-row-menu]")) {
        setOpenMenu(null);
      }
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key !== "Escape") return;
      setOpenMenu(null);
      menuTriggerRef.current?.focus();
    }
    function close() {
      setOpenMenu(null);
    }
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    window.addEventListener("scroll", close, true);
    window.addEventListener("resize", close);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("scroll", close, true);
      window.removeEventListener("resize", close);
    };
  }, [openMenu]);

  // 列表刷新后若菜单对应的账号已不在当前页，清掉残留的打开状态。
  useEffect(() => {
    if (openMenu && !users.some((item) => item.id === openMenu.id)) setOpenMenu(null);
  }, [users, openMenu]);

  async function submitUserAction() {
    if (!userActionTarget) return;
    const { user, operation } = userActionTarget;
    setUserActionBusy(true);
    setUserActionError(null);
    try {
      const res = await fetch(`/api/admin/users/${user.id}`, {
        method: "PATCH",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ operation }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setUserActionError(data?.error?.message || "操作失败");
        return;
      }
      setUsers((prev) =>
        prev.map((item) =>
          item.id === user.id
            ? { ...item, disabledAt: data.user.disabledAt }
            : item
        )
      );
      setUserActionTarget(null);
      setNotice({
        type: "success",
        text: `${operation === "disable" ? "已禁用" : "已启用"} ${displayUser(user)}`,
      });
    } catch {
      setUserActionError("网络异常，操作失败");
    } finally {
      setUserActionBusy(false);
    }
  }

  async function submitPasswordReset() {
    if (!passwordTarget) return;
    if (password.trim().length < 6) {
      setPasswordError("密码至少 6 位");
      return;
    }
    if (password !== passwordConfirm) {
      setPasswordError("两次输入的密码不一致");
      return;
    }

    setPasswordBusy(true);
    setPasswordError(null);
    try {
      const res = await fetch(`/api/admin/users/${passwordTarget.id}`, {
        method: "PATCH",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ operation: "reset_password", password }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setPasswordError(data?.error?.message || "重置密码失败");
        return;
      }
      const username = displayUser(passwordTarget);
      const revokedApiKeys = Number(data?.revokedApiKeys) || 0;
      setPasswordTarget(null);
      setPassword("");
      setPasswordConfirm("");
      setShowPassword(false);
      setNotice({
        type: "success",
        text: `已重置 ${username} 的密码，所有已登录设备已退出；${
          revokedApiKeys > 0
            ? `${revokedApiKeys} 个 API Key 已失效`
            : "该账号没有有效的 API Key"
        }。请将新密码告知该用户。`,
      });
    } catch {
      setPasswordError("网络异常，重置密码失败");
    } finally {
      setPasswordBusy(false);
    }
  }

  async function submitCooldownReset() {
    if (!cooldownTarget) return;
    setCooldownBusy(true);
    setCooldownError(null);
    try {
      const res = await fetch(`/api/admin/users/${cooldownTarget.id}`, {
        method: "PATCH",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ operation: "reset_export_cooldown" }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setCooldownError(data?.error?.message || "解除导出限制失败");
        return;
      }
      const cooldown = data?.exportCooldown;
      const username = displayUser(cooldownTarget);
      setCooldownTarget(null);
      if (cooldown?.reservationActive) {
        const waitMinutes = Math.max(
          1,
          Math.ceil(Number(cooldown.retryAfterSeconds || 0) / 60)
        );
        setNotice({
          type: "warn",
          text: `已解除 ${username} 的导出限制，但该账号仍有导出正在进行，约 ${waitMinutes} 分钟后才能再次导出。`,
        });
      } else if (!cooldown?.reset) {
        setNotice({
          type: "warn",
          text: `${username} 当前没有导出限制，无需解除。`,
        });
      } else {
        setNotice({
          type: "success",
          text: `已解除 ${username} 的导出限制，该账号可以立即导出。`,
        });
      }
    } catch {
      setCooldownError("网络异常，解除导出限制失败");
    } finally {
      setCooldownBusy(false);
    }
  }

  async function saveQuota(user: AdminUser) {
    const raw = quotaDraft.trim();
    let quotaValue: number | null;
    if (raw === "") {
      quotaValue = null;
    } else if (/^\d+(\.\d+)?$/.test(raw)) {
      const amount = Number(raw);
      quotaValue = amount === 0
        ? 0
        : Math.round(amount * (quotaUnit === "GB" ? BYTES_PER_GB : BYTES_PER_MB));
      // 除以 2^20 / 2^30 是精确运算，未改动的历史字节值可原样往返保存。
      if (amount > 0 && quotaValue < BYTES_PER_MB) {
        setNotice({ type: "error", text: "配额最小为 1 MB" });
        return;
      }
    } else {
      setNotice({
        type: "error",
        text: "请输入非负数字（单位 MB/GB）；留空表示继承全局配额，0 表示不限",
      });
      return;
    }

    setSavingQuotaId(user.id);
    setNotice(null);
    try {
      const res = await fetch(`/api/admin/users/${user.id}`, {
        method: "PATCH",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          operation: "set_quota",
          quotaBytes: quotaValue,
        }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setNotice({
          type: "error",
          text: data?.error?.message || "保存配额失败",
        });
        return;
      }

      const quota = data?.quota;
      setUsers((prev) =>
        prev.map((item) =>
          item.id === user.id
            ? {
                ...item,
                quotaBytes:
                  typeof quota?.effectiveBytes === "number"
                    ? quota.effectiveBytes
                    : item.quotaBytes,
                quotaOverrideBytes: quota?.overrideBytes ?? null,
                storageUsedBytes:
                  typeof quota?.usedBytes === "number"
                    ? quota.usedBytes
                    : item.storageUsedBytes,
              }
            : item
        )
      );
      setEditingQuotaId(null);
      setQuotaDraft("");
      setQuotaUnit("MB");
      if (quota?.exceeded) {
        setNotice({
          type: "warn",
          text: `${displayUser(user)} 已用 ${formatBytes(
            quota.usedBytes ?? 0
          )}，超过新配额，需清理回收站或调高配额后才能继续上传。`,
        });
      } else if (quota?.changed === false) {
        setNotice({ type: "warn", text: `${displayUser(user)} 的配额未变化。` });
      } else {
        setNotice({
          type: "success",
          text: `已更新 ${displayUser(user)} 的存储配额。`,
        });
      }
    } catch {
      setNotice({ type: "error", text: "网络异常，保存配额失败" });
    } finally {
      setSavingQuotaId(null);
    }
  }

  async function submitEmail() {
    if (!emailTarget) return;
    const email = emailDraft.trim().toLowerCase();
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
      setEmailError("请输入有效的邮箱地址");
      return;
    }

    setEmailBusy(true);
    setEmailError(null);
    try {
      const res = await fetch(`/api/admin/users/${emailTarget.id}`, {
        method: "PATCH",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ operation: "set_email", email }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setEmailError(data?.error?.message || "保存邮箱失败");
        return;
      }
      setUsers((prev) =>
        prev.map((item) =>
          item.id === emailTarget.id ? { ...item, email: data.email } : item
        )
      );
      setEmailTarget(null);
      setEmailDraft("");
      setNotice({
        type: "success",
        text: data.changed
          ? `已更新 ${displayUser({ ...emailTarget, email: data.email })} 的登录邮箱。`
          : "邮箱未变化。",
      });
    } catch {
      setEmailError("网络异常，保存邮箱失败");
    } finally {
      setEmailBusy(false);
    }
  }

  async function submitCreateUser() {
    const email = createEmail.trim().toLowerCase();
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
      setCreateError("请输入有效的邮箱地址");
      return;
    }

    setCreateBusy(true);
    setCreateError(null);
    try {
      const res = await fetch("/api/admin/users", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ email }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setCreateError(data?.error?.message || "创建用户失败");
        return;
      }
      // 明文临时密码只在结果弹窗里出现一次；关闭即丢弃。
      setCreateResult({ email, temporaryPassword: data.temporaryPassword });
      setCreateCopied(false);
      setCreateOpen(false);
      setCreateEmail("");
      // 新账号可能不匹配当前搜索条件，先清筛选再回到第 1 页刷新。
      setQueryInput("");
      setQuery("");
      setPage(1);
      await load(1, "");
      setNotice({ type: "success", text: `已创建账号 ${email}。` });
    } catch {
      setCreateError("网络异常，创建用户失败");
    } finally {
      setCreateBusy(false);
    }
  }

  async function copyTemporaryPassword() {
    if (!createResult) return;
    const copied = await writeClipboardText(createResult.temporaryPassword);
    setCreateCopied(copied);
  }

  if (loading && users.length === 0) {
    return <p style={{ color: colors.muted }}>正在加载用户...</p>;
  }
  if (error && users.length === 0) {
    return (
      <div>
        <p style={{ color: colors.danger }}>{error}</p>
        <button style={btn("outline")} onClick={() => void load(page, query)}>
          重试
        </button>
      </div>
    );
  }

  return (
    <div style={{ ...panelStyle }}>
      <div style={{ display: "flex", gap: 8, marginBottom: 10, flexWrap: "wrap" }}>
        <input
          value={queryInput}
          onChange={(event) => setQueryInput(event.target.value)}
          placeholder="搜索邮箱或用户名"
          aria-label="搜索邮箱或用户名"
          style={{
            flex: "1 1 240px",
            maxWidth: 320,
            padding: "7px 10px",
            border: `1px solid ${colors.border}`,
            borderRadius: 2,
            fontSize: 13,
          }}
        />
        {queryInput && (
          <button style={btn("outline")} onClick={() => setQueryInput("")}>
            清空
          </button>
        )}
        <button
          type="button"
          style={{ ...btn("primary"), marginLeft: "auto" }}
          onClick={() => {
            setCreateError(null);
            setCreateEmail("");
            setCreateOpen(true);
          }}
        >
          + 创建用户
        </button>
      </div>

      {notice && (
        <p
          role={notice.type === "error" ? "alert" : "status"}
          style={{
            margin: "0 0 10px",
            fontSize: 13,
            color:
              notice.type === "error"
                ? colors.danger
                : notice.type === "warn"
                ? "var(--ph-warning)"
                : colors.success,
          }}
        >
          {notice.text}
        </p>
      )}
      {error && users.length > 0 && (
        <p role="alert" style={{ margin: "0 0 10px", fontSize: 13, color: colors.danger }}>
          {error}{" "}
          <button style={btn("outline")} onClick={() => void load(page, query)}>
            重试
          </button>
        </p>
      )}

      <p className="admin-table-hint">左右滑动可查看全部列</p>
      <table className="app-table" style={tableStyle(1040)}>
        <thead>
          <tr>
            <th style={thStyle}>ID</th>
            <th style={thStyle}>邮箱 / 账号</th>
            <th style={thStyle}>角色</th>
            <th style={thStyle}>状态</th>
            <th style={thStyle}>图片数</th>
            <th style={thStyle}>存储用量</th>
            <th style={thStyle}>创建时间</th>
            <th style={thStyle}>操作</th>
          </tr>
        </thead>
        <tbody>
          {users.map((item) => {
            const isCurrentUser = item.id === currentUserId;
            const isDisabled = Boolean(item.disabledAt);
            const usedBytes = item.storageUsedBytes ?? 0;
            const effectiveQuota = item.quotaBytes ?? defaultQuotaBytes;
            const overQuota = effectiveQuota > 0 && usedBytes >= effectiveQuota;
            const editingQuota = editingQuotaId === item.id;
            return (
              <tr key={item.id}>
                <td style={tdStyle}>{item.id}</td>
                <td style={tdStyle}>
                  {displayUser(item)}
                  {item.email &&
                    item.username !== item.email &&
                    !isGeneratedUsername(item.username) && (
                      <div style={{ fontSize: 11, color: colors.muted }}>
                        {item.username}
                      </div>
                    )}
                </td>
                <td style={tdStyle}>
                  <span className={`admin-role-badge${item.role === "ADMIN" ? " is-admin" : ""}`}>
                    {item.role === "ADMIN" ? "管理员" : "用户"}
                  </span>
                </td>
                <td style={tdStyle}>
                  {isDisabled ? (
                    <span style={{ color: colors.danger }}>已禁用</span>
                  ) : (
                    <span style={{ color: colors.success }}>正常</span>
                  )}
                </td>
                <td style={tdStyle}>{item._count?.images || 0}</td>
                <td
                  style={{
                    ...tdStyle,
                    color: overQuota ? colors.danger : undefined,
                  }}
                  title={
                    effectiveQuota > 0
                      ? `${usedBytes} / ${effectiveQuota} bytes（含回收站与管理员隐藏/下架、尚未物理删除的图片）`
                      : `${usedBytes} bytes（不限配额）`
                  }
                >
                  {editingQuota ? (
                    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
                      <div style={{ display: "flex", alignItems: "center", gap: 4 }}>
                        <input
                          value={quotaDraft}
                          onChange={(event) => setQuotaDraft(event.target.value)}
                          placeholder={
                            defaultQuotaBytes > 0
                              ? `继承 ${formatBytes(defaultQuotaBytes)}`
                              : "继承全局（不限）"
                          }
                          aria-label={`设置 ${displayUser(item)} 的存储配额`}
                          style={{
                            width: 86,
                            padding: "4px 6px",
                            border: `1px solid ${colors.border}`,
                            borderRadius: 2,
                            fontSize: 12,
                          }}
                        />
                        <select
                          value={quotaUnit}
                          onChange={(event) => setQuotaUnit(event.target.value as "MB" | "GB")}
                          aria-label={`设置 ${displayUser(item)} 的配额单位`}
                          style={{
                            padding: "4px 4px",
                            border: `1px solid ${colors.border}`,
                            borderRadius: 2,
                            fontSize: 12,
                            background: "var(--ph-surface)",
                            color: "var(--ph-text)",
                          }}
                        >
                          <option value="MB">MB</option>
                          <option value="GB">GB</option>
                        </select>
                        <button
                          style={{ ...btn("primary"), padding: "4px 8px", fontSize: 12 }}
                          disabled={savingQuotaId === item.id}
                          onClick={() => void saveQuota(item)}
                        >
                          {savingQuotaId === item.id ? "保存中" : "保存"}
                        </button>
                        <button
                          style={{ ...btn("outline"), padding: "4px 8px", fontSize: 12 }}
                          disabled={savingQuotaId === item.id}
                          onClick={() => {
                            setEditingQuotaId(null);
                            setQuotaDraft("");
                            setQuotaUnit("MB");
                          }}
                        >
                          取消
                        </button>
                      </div>
                      <span style={{ fontSize: 11, color: colors.muted }}>
                        留空=继承全局，0=不限，最小 1 MB
                      </span>
                    </div>
                  ) : (
                    <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
                      <span>
                        {formatBytes(usedBytes)} /{" "}
                        {effectiveQuota > 0 ? formatBytes(effectiveQuota) : "不限"}
                      </span>
                      {item.quotaOverrideBytes != null && (
                        <span style={{ fontSize: 11, color: colors.muted }}>
                          {item.quotaOverrideBytes === 0 ? "账号不限" : "专属"}
                        </span>
                      )}
                      <button
                        style={{ ...btn("outline"), padding: "4px 8px", fontSize: 12 }}
                        onClick={() => {
                          setEditingQuotaId(item.id);
                          const override = item.quotaOverrideBytes;
                          if (override == null) {
                            setQuotaDraft("");
                            setQuotaUnit("MB");
                          } else {
                            const unit = override >= BYTES_PER_GB && override % BYTES_PER_GB === 0 ? "GB" : "MB";
                            setQuotaUnit(unit);
                            setQuotaDraft(String(override / (unit === "GB" ? BYTES_PER_GB : BYTES_PER_MB)));
                          }
                          setNotice(null);
                        }}
                      >
                        设置
                      </button>
                    </div>
                  )}
                </td>
                <td style={tdStyle}>{formatDate(item.createdAt)}</td>
                <td style={tdStyle}>
                  <button
                    type="button"
                    className="admin-row-menu-trigger"
                    data-admin-row-menu
                    aria-label={`${displayUser(item)} 的更多操作`}
                    aria-haspopup="menu"
                    aria-expanded={openMenu?.id === item.id}
                    ref={(node) => { if (openMenu?.id === item.id) menuTriggerRef.current = node; }}
                    onClick={(event) => toggleRowMenu(event.currentTarget, item.id)}
                  >
                    更多 <span aria-hidden="true">⋯</span>
                  </button>
                  {openMenu?.id === item.id && (
                    <div
                      className="admin-row-menu"
                      role="menu"
                      data-admin-row-menu
                      style={{ top: openMenu.top, left: openMenu.left }}
                    >
                      {!isCurrentUser && (
                        <button
                          type="button"
                          role="menuitem"
                          className={isDisabled ? undefined : "danger"}
                          onClick={() => {
                            setOpenMenu(null);
                            setUserActionError(null);
                            setUserActionTarget({
                              user: item,
                              operation: isDisabled ? "enable" : "disable",
                            });
                          }}
                        >
                          {isDisabled ? "启用" : "禁用"}
                        </button>
                      )}
                      <button
                        type="button"
                        role="menuitem"
                        onClick={() => {
                          setOpenMenu(null);
                          setPasswordTarget(item);
                          setPassword("");
                          setPasswordConfirm("");
                          setShowPassword(false);
                          setPasswordError(null);
                        }}
                      >
                        重置密码
                      </button>
                      <button
                        type="button"
                        role="menuitem"
                        onClick={() => {
                          setOpenMenu(null);
                          setEmailTarget(item);
                          setEmailDraft(item.email ?? "");
                          setEmailError(null);
                        }}
                      >
                        修改邮箱
                      </button>
                      <button
                        type="button"
                        role="menuitem"
                        onClick={() => {
                          setOpenMenu(null);
                          setCooldownTarget(item);
                          setCooldownError(null);
                        }}
                      >
                        解除导出限制
                      </button>
                    </div>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>

      {!loading && users.length === 0 && (
        <p style={{ color: colors.muted, padding: "16px 0", margin: 0 }}>
          没有匹配的账号
        </p>
      )}

      <div
        style={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          gap: 8,
          flexWrap: "wrap",
          marginTop: 12,
        }}
      >
        <span style={{ color: colors.muted, fontSize: 12 }}>
          共 {total} 个账号 · 第 {page} / {totalPages} 页
          {loading ? " · 更新中…" : ""}
        </span>
        <div style={{ display: "flex", gap: 6 }}>
          <button
            style={btn("outline")}
            disabled={loading || page <= 1}
            onClick={() => setPage((current) => Math.max(1, current - 1))}
          >
            上一页
          </button>
          <button
            style={btn("outline")}
            disabled={loading || page >= totalPages}
            onClick={() =>
              setPage((current) => Math.min(totalPages, current + 1))
            }
          >
            下一页
          </button>
        </div>
      </div>

      {userActionTarget && (
        <ConfirmDialog
          title={
            userActionTarget.operation === "disable" ? "禁用账号" : "启用账号"
          }
          message={
            userActionTarget.operation === "disable"
              ? `禁用 ${displayUser(userActionTarget.user)} 后，该账号将无法登录或使用 API Key，可随时重新启用。`
              : `启用 ${displayUser(userActionTarget.user)} 后，该账号可以重新登录并使用 API Key。`
          }
          confirmLabel={userActionTarget.operation === "disable" ? "禁用" : "启用"}
          danger={userActionTarget.operation === "disable"}
          busy={userActionBusy}
          error={userActionError}
          onConfirm={() => void submitUserAction()}
          onClose={() => {
            if (!userActionBusy) {
              setUserActionTarget(null);
              setUserActionError(null);
            }
          }}
        />
      )}

      {passwordTarget && (
        <AppDialog
          title={`重置 ${displayUser(passwordTarget)} 的密码`}
          busy={passwordBusy}
          onClose={() => {
            if (!passwordBusy) {
              setPasswordTarget(null);
              setPasswordError(null);
            }
          }}
          footer={
            <>
              <button
                type="button"
                className="image-button"
                disabled={passwordBusy}
                onClick={() => {
                  setPasswordTarget(null);
                  setPasswordError(null);
                }}
              >
                取消
              </button>
              <button
                type="button"
                className="image-button primary"
                disabled={passwordBusy}
                onClick={() => void submitPasswordReset()}
              >
                {passwordBusy ? "重置中…" : "重置密码"}
              </button>
            </>
          }
        >
          <div style={{ display: "grid", gap: 10 }}>
            <label className="image-field">
              新密码
              <input
                type={showPassword ? "text" : "password"}
                value={password}
                onChange={(event) => {
                  setPassword(event.target.value);
                  setPasswordError(null);
                }}
                autoComplete="new-password"
                placeholder="至少 6 位"
              />
            </label>
            <label className="image-field">
              确认新密码
              <input
                type={showPassword ? "text" : "password"}
                value={passwordConfirm}
                onChange={(event) => {
                  setPasswordConfirm(event.target.value);
                  setPasswordError(null);
                }}
                autoComplete="new-password"
                placeholder="再次输入"
              />
            </label>
            <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 13 }}>
              <input
                type="checkbox"
                checked={showPassword}
                onChange={(event) => setShowPassword(event.target.checked)}
              />
              显示密码
            </label>
            <p className="image-help" style={{ margin: 0 }}>
              重置后该用户所有已登录设备会立即退出，API Key 也会全部失效，需要重新登录并重新生成 Key。
            </p>
            {passwordError && (
              <p role="alert" className="image-notice error">
                {passwordError}
              </p>
            )}
          </div>
        </AppDialog>
      )}

      {cooldownTarget && (
        <ConfirmDialog
          title="解除导出限制"
          message={`确认解除 ${displayUser(cooldownTarget)} 的导出限制？解除后该账号可以立即再次导出；若该账号正有导出在进行，导出不会被打断，需等它结束。`}
          confirmLabel="解除限制"
          busy={cooldownBusy}
          error={cooldownError}
          onConfirm={() => void submitCooldownReset()}
          onClose={() => {
            if (!cooldownBusy) {
              setCooldownTarget(null);
              setCooldownError(null);
            }
          }}
        />
      )}

      {emailTarget && (
        <AppDialog
          title="修改登录邮箱"
          busy={emailBusy}
          onClose={() => {
            if (!emailBusy) {
              setEmailTarget(null);
              setEmailError(null);
            }
          }}
          footer={
            <>
              <button
                type="button"
                className="image-button"
                disabled={emailBusy}
                onClick={() => {
                  setEmailTarget(null);
                  setEmailError(null);
                }}
              >
                取消
              </button>
              <button
                type="button"
                className="image-button primary"
                disabled={emailBusy}
                onClick={() => void submitEmail()}
              >
                {emailBusy ? "保存中…" : "保存"}
              </button>
            </>
          }
        >
          <div style={{ display: "grid", gap: 10 }}>
            <label className="image-field">
              新邮箱
              <input
                type="email"
                value={emailDraft}
                onChange={(event) => {
                  setEmailDraft(event.target.value);
                  setEmailError(null);
                }}
                placeholder="you@example.com"
                autoComplete="email"
              />
            </label>
            <p className="image-help" style={{ margin: 0 }}>
              更新后请使用新邮箱登录。
            </p>
            {emailError && (
              <p role="alert" className="image-notice error">
                {emailError}
              </p>
            )}
          </div>
        </AppDialog>
      )}

      {createOpen && (
        <AppDialog
          title="创建用户"
          busy={createBusy}
          onClose={() => {
            if (!createBusy) {
              setCreateOpen(false);
              setCreateError(null);
            }
          }}
          footer={
            <>
              <button
                type="button"
                className="image-button"
                disabled={createBusy}
                onClick={() => {
                  setCreateOpen(false);
                  setCreateError(null);
                }}
              >
                取消
              </button>
              <button
                type="button"
                className="image-button primary"
                disabled={createBusy}
                onClick={() => void submitCreateUser()}
              >
                {createBusy ? "创建中…" : "创建"}
              </button>
            </>
          }
        >
          <div style={{ display: "grid", gap: 10 }}>
            <label className="image-field">
              邮箱
              <input
                type="email"
                value={createEmail}
                onChange={(event) => {
                  setCreateEmail(event.target.value);
                  setCreateError(null);
                }}
                placeholder="you@example.com"
                autoComplete="off"
              />
            </label>
            <p className="image-help" style={{ margin: 0 }}>
              角色固定为普通用户。系统会生成一个临时密码，创建成功后只显示一次；
              该账号首次登录时必须修改密码。
            </p>
            {createError && (
              <p role="alert" className="image-notice error">
                {createError}
              </p>
            )}
          </div>
        </AppDialog>
      )}

      {createResult && (
        <AppDialog
          title="账号已创建"
          onClose={() => setCreateResult(null)}
          footer={
            <button
              type="button"
              className="image-button primary"
              onClick={() => setCreateResult(null)}
            >
              我已保存，关闭
            </button>
          }
        >
          <div style={{ display: "grid", gap: 12 }}>
            <p style={{ margin: 0, fontSize: 13, color: colors.danger, fontWeight: 600 }}>
              ⚠️ 完整密码只显示这一次，关闭后无法再查看，请先复制保存。
            </p>
            <div style={{ display: "grid", gap: 4 }}>
              <span className="image-help" style={{ margin: 0 }}>邮箱</span>
              <code style={{ fontSize: 13, wordBreak: "break-all" }}>{createResult.email}</code>
            </div>
            <div style={{ display: "grid", gap: 4 }}>
              <span className="image-help" style={{ margin: 0 }}>临时密码</span>
              <code
                data-testid="temporary-password"
                style={{ fontSize: 15, fontWeight: 700, letterSpacing: 1, wordBreak: "break-all", userSelect: "all" }}
              >
                {createResult.temporaryPassword}
              </code>
            </div>
            <button
              type="button"
              className="image-button"
              style={{ justifySelf: "start" }}
              onClick={() => void copyTemporaryPassword()}
            >
              {createCopied ? "✓ 已复制" : "复制密码"}
            </button>
            <p className="image-help" style={{ margin: 0 }}>
              请把邮箱与临时密码交给对方。对方首次登录后会被要求设置自己的新密码。
            </p>
          </div>
        </AppDialog>
      )}
    </div>
  );
}
function AdminImagesTab() {
  const [items, setItems] = useState<AdminImage[]>([]);
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [visibility, setVisibility] = useState<"all" | "visible" | "hidden">("all");
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [togglingVisibilityId, setTogglingVisibilityId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<AdminImage | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);

  useEffect(() => {
    const timer = window.setTimeout(() => setQuery(queryInput.trim()), 300);
    return () => window.clearTimeout(timer);
  }, [queryInput]);

  const fetchImages = useCallback(async (cursor?: string) => {
    const params = new URLSearchParams({ limit: "30" });
    if (query) params.set("q", query);
    if (visibility !== "all") params.set("visibility", visibility);
    if (cursor) {
      params.set("cursor", cursor);
    }
    const res = await fetch(`/api/admin/images?${params.toString()}`);
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(data?.error?.message || "加载图片失败");
    }
    return data as { items: AdminImage[]; nextCursor: string | null };
  }, [query, visibility]);

  const loadInitial = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await fetchImages();
      setItems(data.items);
      setNextCursor(data.nextCursor);
    } catch (e: any) {
      setError(e?.message || "加载图片失败");
    } finally {
      setLoading(false);
    }
  }, [fetchImages]);

  useEffect(() => {
    void loadInitial();
  }, [loadInitial]);

  async function loadMore() {
    if (!nextCursor) {
      return;
    }
    setLoadingMore(true);
    setError(null);
    try {
      const data = await fetchImages(nextCursor);
      setItems((prev) => [...prev, ...data.items]);
      setNextCursor(data.nextCursor);
    } catch (e: any) {
      setError(e?.message || "加载更多图片失败");
    } finally {
      setLoadingMore(false);
    }
  }

  async function submitDelete() {
    if (!deleteTarget) return;
    setDeleteBusy(true);
    setDeleteError(null);
    try {
      const res = await fetch(`/api/admin/images/${deleteTarget.id}`, {
        method: "DELETE",
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setDeleteError(data?.error?.message || "删除失败");
        return;
      }
      setItems((prev) => prev.filter((item) => item.id !== deleteTarget.id));
      setDeleteTarget(null);
    } catch {
      setDeleteError("网络异常，删除失败");
    } finally {
      setDeleteBusy(false);
    }
  }

  async function toggleVisibility(item: AdminImage) {
    setTogglingVisibilityId(item.id);
    setActionError(null);
    try {
      const operation = item.isVisible ? "hide" : "show";
      const res = await fetch(`/api/admin/images/${item.id}`, {
        method: "PATCH",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ operation }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setActionError(data?.error?.message || "更新可见状态失败");
        return;
      }
      setItems((prev) =>
        prev.map((image) =>
          image.id === item.id
            ? { ...image, isVisible: Boolean(data?.image?.isVisible) }
            : image
        )
      );
    } catch {
      setActionError("网络异常，更新可见状态失败");
    } finally {
      setTogglingVisibilityId(null);
    }
  }

  if (loading) {
    return <p style={{ color: colors.muted }}>正在加载图片...</p>;
  }
  if (error && items.length === 0) {
    return (
      <div>
        <p style={{ color: colors.danger }}>{error}</p>
        <button style={btn("outline")} onClick={() => void loadInitial()}>
          重试
        </button>
      </div>
    );
  }

  return (
    <div style={{ ...panelStyle }}>
      {actionError && (
        <p role="alert" style={{ margin: "0 0 10px", fontSize: 13, color: colors.danger }}>
          {actionError}
        </p>
      )}
      {error && (
        <p role="alert" style={{ margin: "0 0 10px", fontSize: 13, color: colors.danger }}>
          {error}{" "}
          <button style={btn("outline")} onClick={() => void loadMore()}>
            重试
          </button>
        </p>
      )}
      <div className="admin-filter-bar">
        <input
          value={queryInput}
          onChange={(event) => setQueryInput(event.target.value)}
          placeholder="搜索文件名或上传者"
          aria-label="搜索图片"
          style={compactInputStyle}
        />
        <select value={visibility} onChange={(event) => setVisibility(event.target.value as typeof visibility)} aria-label="可见状态筛选" style={compactInputStyle}>
          <option value="all">全部状态</option>
          <option value="visible">仅可见</option>
          <option value="hidden">仅不可见</option>
        </select>
      </div>
      <p className="admin-table-hint">左右滑动可查看全部列</p>
      <table className="app-table" style={tableStyle(1040)}>
        <thead>
          <tr>
            <th style={thStyle}>图片</th>
            <th style={thStyle}>文件名</th>
            <th style={thStyle}>上传者</th>
            <th style={thStyle}>上传时间</th>
            <th style={thStyle}>可见性</th>
            <th style={thStyle}>操作</th>
          </tr>
        </thead>
        <tbody>
          {items.map((item) => (
            <tr key={item.id}>
              <td style={tdStyle}>
                <img
                  src={item.thumbUrl}
                  alt={item.originalName}
                  loading="lazy"
                  decoding="async"
                  style={{ width: 80, height: 80, objectFit: "cover", borderRadius: 2 }}
                />
              </td>
              <td style={tdStyle}>{item.originalName}</td>
              <td style={tdStyle}>
                {displayUser(item.uploader)} (#{item.uploader.id})
              </td>
              <td style={tdStyle}>{formatDate(item.createdAt)}</td>
              <td style={tdStyle}>
                <span style={{ color: item.isVisible ? colors.success : colors.muted }}>
                  {item.isVisible ? "可见" : "不可见"}
                </span>
              </td>
              <td style={tdStyle}>
                <div style={{ display: "flex", gap: 8 }}>
                  <a href={item.pageUrl || item.url} target="_blank" style={{ ...btn("outline"), textDecoration: "none" }}>
                    查看
                  </a>
                  <button
                    style={btn("outline")}
                    onClick={() => toggleVisibility(item)}
                    disabled={togglingVisibilityId === item.id}
                  >
                    {togglingVisibilityId === item.id
                      ? "更新中..."
                      : item.isVisible
                      ? "设为不可见"
                      : "设为可见"}
                  </button>
                  <button
                    style={btn("danger")}
                    onClick={() => {
                      setDeleteTarget(item);
                      setDeleteError(null);
                    }}
                  >
                    删除
                  </button>
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {!loading && items.length === 0 && (
        <p style={{ color: colors.muted, padding: "16px 0", margin: 0 }}>
          暂无图片
        </p>
      )}
      <div style={{ marginTop: 16 }}>
        {nextCursor ? (
          <button
            style={btn("outline")}
            onClick={loadMore}
            disabled={loadingMore}
          >
            {loadingMore ? "加载中..." : "加载更多"}
          </button>
        ) : (
          <span style={{ color: colors.muted, fontSize: 12 }}>没有更多了</span>
        )}
      </div>
      {deleteTarget && (
        <ConfirmDialog
          title="删除图片"
          message={
            <>
              确认删除 <strong>{deleteTarget.originalName}</strong>（上传者{" "}
              {displayUser(deleteTarget.uploader)}）？图片会被管理员下架，上传者无法恢复。
            </>
          }
          confirmLabel="删除"
          danger
          busy={deleteBusy}
          error={deleteError}
          onConfirm={() => void submitDelete()}
          onClose={() => {
            if (!deleteBusy) {
              setDeleteTarget(null);
              setDeleteError(null);
            }
          }}
        />
      )}
    </div>
  );
}

function AdminAuditLogsTab() {
  const [logs, setLogs] = useState<AdminAuditLog[]>([]);
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [action, setAction] = useState<AdminAuditLog["action"] | "all">("all");
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const timer = window.setTimeout(() => setQuery(queryInput.trim()), 300);
    return () => window.clearTimeout(timer);
  }, [queryInput]);

  const fetchLogs = useCallback(async (cursor?: string) => {
    const params = new URLSearchParams({ limit: "30" });
    if (query) params.set("q", query);
    if (action !== "all") params.set("action", action);
    if (cursor) {
      params.set("cursor", cursor);
    }
    const res = await fetch(`/api/admin/audit-logs?${params.toString()}`);
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(data?.error?.message || "加载审计日志失败");
    }
    return data as { logs: AdminAuditLog[]; nextCursor: string | null };
  }, [query, action]);

  const loadInitial = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const data = await fetchLogs();
      setLogs(data.logs);
      setNextCursor(data.nextCursor);
    } catch (e: any) {
      setError(e?.message || "加载审计日志失败");
    } finally {
      setLoading(false);
    }
  }, [fetchLogs]);

  useEffect(() => {
    void loadInitial();
  }, [loadInitial]);

  async function loadMore() {
    if (!nextCursor) {
      return;
    }
    setLoadingMore(true);
    setError(null);
    try {
      const data = await fetchLogs(nextCursor);
      setLogs((prev) => [...prev, ...data.logs]);
      setNextCursor(data.nextCursor);
    } catch (e: any) {
      setError(e?.message || "加载更多审计日志失败");
    } finally {
      setLoadingMore(false);
    }
  }

  function getActionLabel(action: AdminAuditLog["action"]) {
    if (action === "ADMIN_LOGIN") {
      return "管理员登录";
    }
    if (action === "ADMIN_USER_CREATE") {
      return "创建用户";
    }
    if (action === "ADMIN_USER_DISABLE") {
      return "禁用用户";
    }
    if (action === "ADMIN_USER_ENABLE") {
      return "启用用户";
    }
    if (action === "ADMIN_USER_PASSWORD_RESET") {
      return "重置用户密码";
    }
    if (action === "ADMIN_USER_EXPORT_COOLDOWN_RESET") {
      return "解除导出限制";
    }
    if (action === "ADMIN_USER_QUOTA_UPDATE") {
      return "调整存储配额";
    }
    if (action === "ADMIN_USER_EMAIL_UPDATE") {
      return "修改登录邮箱";
    }
    if (action === "ADMIN_ACTIVATION_CODE_CREATE") {
      return "生成激活码";
    }
    if (action === "ADMIN_ACTIVATION_CODE_REVOKE") {
      return "撤销激活码";
    }
    if (action === "ACTIVATION_CODE_USED") {
      return "使用激活码";
    }
    if (action === "ADMIN_IMAGE_HIDE") {
      return "隐藏图片";
    }
    if (action === "ADMIN_IMAGE_SHOW") {
      return "显示图片";
    }
    if (action === "ADMIN_IMAGE_DELETE") {
      return "删除图片";
    }
    return "未知操作";
  }

  if (loading) {
    return <p style={{ color: colors.muted }}>正在加载审计日志...</p>;
  }
  if (error && logs.length === 0) {
    return (
      <div>
        <p style={{ color: colors.danger }}>{error}</p>
        <button style={btn("outline")} onClick={() => void loadInitial()}>
          重试
        </button>
      </div>
    );
  }

  return (
    <div style={panelStyle}>
      {error && (
        <p role="alert" style={{ margin: "0 0 10px", fontSize: 13, color: colors.danger }}>
          {error}{" "}
          <button style={btn("outline")} onClick={() => void loadMore()}>
            重试
          </button>
        </p>
      )}
      <div className="admin-filter-bar">
        <input
          value={queryInput}
          onChange={(event) => setQueryInput(event.target.value)}
          placeholder="搜索操作者、目标或详情"
          aria-label="搜索审计日志"
          style={compactInputStyle}
        />
        <select value={action} onChange={(event) => setAction(event.target.value as typeof action)} aria-label="操作类型筛选" style={compactInputStyle}>
          <option value="all">全部操作</option>
          {AUDIT_ACTION_OPTIONS.map((option) => (
            <option value={option.value} key={option.value}>{option.label}</option>
          ))}
        </select>
      </div>
      <p className="admin-table-hint">左右滑动可查看全部列</p>
      <table className="app-table" style={tableStyle(1040)}>
        <thead>
          <tr>
            <th style={thStyle}>时间</th>
            <th style={thStyle}>操作</th>
            <th style={thStyle}>操作者</th>
            <th style={thStyle}>目标用户</th>
            <th style={thStyle}>详情</th>
          </tr>
        </thead>
        <tbody>
          {logs.map((log) => (
            <tr key={log.id}>
              <td style={tdStyle}>{formatDate(log.createdAt)}</td>
              <td style={tdStyle}>{getActionLabel(log.action)}</td>
              <td style={tdStyle}>
                {displayUser(log.actor)} (#{log.actor.id})
              </td>
              <td style={tdStyle}>
                {log.targetUser
                  ? `${displayUser(log.targetUser)} (#${log.targetUser.id})`
                  : "-"}
              </td>
              <td style={tdStyle}>
                <span className="admin-log-detail" title={log.detail ?? undefined}>{formatDetail(log.detail)}</span>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {!loading && logs.length === 0 && (
        <p style={{ color: colors.muted, padding: "16px 0", margin: 0 }}>
          暂无审计日志
        </p>
      )}
      <div style={{ marginTop: 16 }}>
        {nextCursor ? (
          <button
            style={btn("outline")}
            onClick={loadMore}
            disabled={loadingMore}
          >
            {loadingMore ? "加载中..." : "加载更多"}
          </button>
        ) : (
          <span style={{ color: colors.muted, fontSize: 12 }}>没有更多了</span>
        )}
      </div>
    </div>
  );
}
