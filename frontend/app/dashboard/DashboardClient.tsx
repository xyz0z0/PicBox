"use client";

import { useEffect, useRef, useState, useCallback, useMemo } from "react";
import { useRouter } from "next/navigation";
import { UserRole } from "@/app/generated/prisma/enums";
import UploadDialog from "./UploadDialog";
import { EditTagsDialog } from "./ImageDialogs";
import ExportDialog, { EXPORT_SELECTED_MAX } from "./ExportDialog";
import { MAX_IMAGE_TAGS } from "@/lib/imageTag";
import { DEFAULT_UPLOAD_CONCURRENCY } from "@/lib/uploadConcurrency";
import { formatUploadMaxLabel } from "@/lib/uploadLimit";
import { writeClipboardText as copyText } from "@/lib/clipboardText";
import { formatBytes } from "@/lib/quota";
import { AppDialog, ConfirmDialog } from "@/app/components/Dialogs";
import {
  colors,
  buttonStyle as btn,
  inputStyle as input,
  cardStyle as card,
} from "@/app/components/ui";
import ImageLightbox from "./ImageLightbox";
import ChangePasswordScreen from "./ChangePasswordScreen";
import ChangePasswordDialog from "./ChangePasswordDialog";
import EmailChangeDialog from "./EmailChangeDialog";

type User = {
  id: number;
  email: string | null;
  username: string;
  role: (typeof UserRole)[keyof typeof UserRole];
  mustChangePassword: boolean;
};
type ImageItem = {
  id: string;
  originalName: string;
  tag: string | null;
  tags: string[];
  createdAt: string;
  isVisible: boolean;
  deletedAt?: string | null;
  url: string;
  pageUrl: string;
  thumbUrl: string;
  width: number | null;
  height: number | null;
  size: number;
  originalMimeType: string;
};
type ApiKeyItem = {
  id: string;
  name: string;
  prefix: string;
  createdAt: string;
  lastUsedAt: string | null;
};

const BYTES_PER_MB = 1024 * 1024;
const DEFAULT_UPLOAD_MAX_BYTES = 5 * BYTES_PER_MB;
const MIN_UPLOAD_MAX_BYTES = 1 * BYTES_PER_MB;

function getClientUploadMaxBytes() {
  const rawValue = process.env.NEXT_PUBLIC_UPLOAD_MAX_BYTES;
  if (!rawValue) {
    return DEFAULT_UPLOAD_MAX_BYTES;
  }
  const parsedValue = Number.parseInt(rawValue, 10);
  if (Number.isNaN(parsedValue)) {
    return DEFAULT_UPLOAD_MAX_BYTES;
  }
  return Math.max(parsedValue, MIN_UPLOAD_MAX_BYTES);
}

// ─── 工具函数 ─────────────────────────────────────────────────────────────────

function formatDate(iso: string) {
  return new Date(iso).toLocaleString("zh-CN", {
    year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit",
  });
}

// 有日期分组标题时卡片只显示时刻，避免重复；无分组（按名称/大小排序）时显示完整日期。
function formatTime(iso: string) {
  return new Date(iso).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" });
}

// ─── 主组件 ──────────────────────────────────────────────────────────────────
// 薄包装不调用任何 hook：改密前后渲染的是不同组件类型，React 各自挂载/卸载，
// hook 数量互不匹配也不会触发 "Rendered more hooks than during the previous render"。
// uploadMaxBytes 可选：Go 部署由 DashboardPage 从 /api/config 传入运行时上限；
// Next 部署不传，保持原有 NEXT_PUBLIC_UPLOAD_MAX_BYTES 逻辑。
// mailEnabled 可选：Go 部署传 /api/config 的邮件能力布尔值，只在为 false 时把
// 「修改邮箱」标成需要管理员操作（该流程没有 SMTP 只能 503）；undefined（Next /
// 配置读取失败）保持原行为。注册仍走激活码，与本标志无关。
export default function DashboardClient({
  user,
  uploadMaxBytes,
  mailEnabled,
  refreshAfterPasswordChange = true,
}: {
  user: User;
  uploadMaxBytes?: number;
  mailEnabled?: boolean;
  refreshAfterPasswordChange?: boolean;
}) {
  if (user.mustChangePassword) return <ChangePasswordScreen />;
  return (
    <DashboardWorkspace
      user={user}
      uploadMaxBytes={uploadMaxBytes}
      mailEnabled={mailEnabled}
      refreshAfterPasswordChange={refreshAfterPasswordChange}
    />
  );
}

function DashboardWorkspace({
  user,
  uploadMaxBytes,
  mailEnabled,
  refreshAfterPasswordChange,
}: {
  user: User;
  uploadMaxBytes?: number;
  mailEnabled?: boolean;
  refreshAfterPasswordChange: boolean;
}) {
  const router = useRouter();
  const [tab, setTab] = useState<"images" | "apikeys">("images");
  const [accountDialog, setAccountDialog] = useState<"settings" | "password" | "email" | null>(null);
  const [passwordChanged, setPasswordChanged] = useState(false);

  function switchTab(next: "images" | "apikeys") {
    setTab(next);
    // 两个 tab 内容高度差异大，避免浏览器把已滚动位置钳制到新页面底部造成跳动。
    window.scrollTo({ top: 0 });
  }

  async function logout() {
    await fetch("/api/auth/logout", { method: "POST" });
    router.refresh();
  }

  function finishPasswordChange() {
    setAccountDialog(null);
    setPasswordChanged(true);
    // Next 的 router.refresh() 保留客户端 state；Go 静态包的 refresh 是整页
    // reload，会把刚设置的提示清掉。改密不改变用户资料，静态页传入 false
    // 跳过这次 reload；登录/退出等其它 refresh 仍保持原语义。
    if (refreshAfterPasswordChange) router.refresh();
  }

  return (
    <div style={{ background: colors.bg }}>
      <header className="app-header">
        <div className="app-header-inner">
          <a className="app-brand" href="/" style={{ flexShrink: 0 }}>
            PicBox
          </a>
          <nav className="app-nav">
            {user.role === UserRole.ADMIN && (
              <a className="app-nav-link" href="/admin" style={{ flexShrink: 0 }}>
                后台管理
              </a>
            )}
            <span className="app-user" style={{ minWidth: 0 }}>
              <svg className="app-user-icon" viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <circle cx="12" cy="8" r="3.5" />
                <path d="M4.5 20c.5-3.5 3.3-5.5 7.5-5.5s7 2 7.5 5.5" />
              </svg>
              <span className="app-user-name">{user.email ?? user.username}</span>
            </span>
            <button
              type="button"
              style={{ ...btn("ghost"), fontSize: 12, padding: "6px 8px", flexShrink: 0 }}
              onClick={() => setAccountDialog("settings")}
            >
              账号设置
            </button>
            <button type="button" style={{ ...btn("ghost"), fontSize: 12, padding: "6px 8px", flexShrink: 0 }} onClick={logout}>
              退出登录
            </button>
          </nav>
        </div>
      </header>

      {passwordChanged && (
        <div className="app-main" style={{ paddingBottom: 0 }}>
          <div className="image-notice is-dismissible success" role="status">
            密码已修改，其他设备需重新登录。
            <button
              type="button"
              className="image-notice-close"
              aria-label="关闭提示"
              onClick={() => setPasswordChanged(false)}
            >
              ×
            </button>
          </div>
        </div>
      )}

      {accountDialog === "settings" && (
        <AppDialog
          title="账号设置"
          onClose={() => setAccountDialog(null)}
          footer={
            <button type="button" className="image-button" onClick={() => setAccountDialog(null)}>
              关闭
            </button>
          }
        >
          <div style={{ display: "grid", gap: 14 }}>
            <div style={{ display: "grid", gap: 4, minWidth: 0 }}>
              <span className="image-help">当前邮箱</span>
              <strong style={{ overflowWrap: "anywhere" }}>{user.email ?? "未设置"}</strong>
            </div>
            <div style={{ display: "flex", flexWrap: "wrap", gap: 8 }}>
              <button type="button" className="image-button primary" onClick={() => setAccountDialog("password")}>
                修改密码
              </button>
              <button
                type="button"
                className="image-button"
                disabled={mailEnabled === false}
                title={mailEnabled === false ? "邮件服务未启用：请联系管理员修改邮箱" : undefined}
                onClick={() => setAccountDialog("email")}
              >
                修改邮箱
              </button>
            </div>
            {mailEnabled === false && (
              <p className="image-help" style={{ margin: 0 }}>
                邮件服务未启用：修改邮箱需要邮件确认，请联系管理员在后台修改。
              </p>
            )}
          </div>
        </AppDialog>
      )}

      {accountDialog === "password" && (
        <ChangePasswordDialog
          onClose={() => setAccountDialog(null)}
          onSuccess={finishPasswordChange}
        />
      )}

      {accountDialog === "email" && mailEnabled !== false && (
        <EmailChangeDialog
          currentEmail={user.email ?? ""}
          onClose={() => setAccountDialog(null)}
        />
      )}

      <main className="app-main">
        <div className="app-tabs" role="tablist">
          {(["images", "apikeys"] as const).map((t) => (
            <button
              key={t}
              type="button"
              role="tab"
              aria-selected={tab === t}
              className={`app-tab${tab === t ? " is-active" : ""}`}
              onClick={() => switchTab(t)}
            >
              {t === "images" ? "我的图片" : "API Key"}
            </button>
          ))}
        </div>

        {tab === "images" ? <ImagesTab uploadMaxBytes={uploadMaxBytes} /> : <ApiKeysTab />}
      </main>
    </div>
  );
}

// ─── 图片标签页 ───────────────────────────────────────────────────────────────
type VisibilityFilter = "all" | "visible" | "hidden";
type ImagesResponse = {
  items: ImageItem[];
  nextCursor: string | null;
  total: number;
};

type BatchAction = "visibility" | "delete" | "restore" | "purge";

const BATCH_ACTION_LABEL: Record<BatchAction, string> = {
  visibility: "更新",
  delete: "移入回收站",
  restore: "恢复",
  purge: "彻底删除",
};

function imageDateLabel(iso: string) {
  const date = new Date(iso);
  const today = new Date();
  const startToday = new Date(today.getFullYear(), today.getMonth(), today.getDate());
  const startDate = new Date(date.getFullYear(), date.getMonth(), date.getDate());
  const dayDifference = Math.round((startToday.getTime() - startDate.getTime()) / 86400000);
  if (dayDifference === 0) return "今天";
  if (dayDifference === 1) return "昨天";
  return date.toLocaleDateString("zh-CN", { year: "numeric", month: "long", day: "numeric" });
}

function EmptyArt() {
  return (
    <svg className="image-empty-art" viewBox="0 0 120 88" aria-hidden="true">
      <rect x="8" y="12" width="104" height="64" rx="10" fill="var(--ph-primary-soft)" />
      <rect x="8" y="12" width="104" height="64" rx="10" fill="none" stroke="var(--ph-border-strong)" strokeWidth="2" />
      <circle cx="40" cy="35" r="8" fill="var(--ph-surface)" />
      <path d="M14 68 42 44l20 17 14-12 30 25H14Z" fill="var(--ph-primary)" opacity="0.55" />
      <path d="M14 72h92" stroke="var(--ph-surface)" strokeWidth="3" strokeLinecap="round" />
    </svg>
  );
}

// 首次加载的卡片骨架：与图片网格同尺寸，避免“一行文字 → 网格”的跳变。
function ImageGridSkeleton() {
  return (
    <div className="compact-image-grid" aria-hidden="true">
      {Array.from({ length: 10 }, (_, index) => (
        <div className="compact-image-card image-skeleton" key={index}>
          <div className="image-skeleton-preview" />
          <div className="compact-image-info">
            <div className="image-skeleton-line" style={{ width: "72%" }} />
            <div className="image-skeleton-line" style={{ width: "45%" }} />
          </div>
        </div>
      ))}
    </div>
  );
}

function ImagesTab({ uploadMaxBytes: configuredUploadMaxBytes }: { uploadMaxBytes?: number }) {
  const requestIdRef = useRef(0);
  const [items, setItems] = useState<ImageItem[]>([]);
  const [queryInput, setQueryInput] = useState("");
  const [query, setQuery] = useState("");
  const [visibility, setVisibility] = useState<VisibilityFilter>("all");
  const [sort, setSort] = useState<"newest" | "oldest" | "name" | "size">("newest");
  const [tagQuery, setTagQuery] = useState("");
  const [tagFilter, setTagFilter] = useState<string[]>([]);
  const [tagMatch, setTagMatch] = useState<"any" | "all">("any");
  const [uploadFiles, setUploadFiles] = useState<File[] | null>(null);
  const [tagEditing, setTagEditing] = useState<{ ids: string[]; tags?: string[] } | null>(null);
  const [exportRequest, setExportRequest] = useState<{ mode: "all" | "selected"; ids: string[] } | null>(null);
  const [tags, setTags] = useState<string[]>([]);
  const [trashed, setTrashed] = useState(false);
  const [total, setTotal] = useState(0);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [usage, setUsage] = useState<{ usedBytes: number; quotaBytes: number } | null>(null);
  const [batchConfirm, setBatchConfirm] = useState<{ ids: string[]; action: "delete" | "purge" } | null>(null);
  const [confirmBusy, setConfirmBusy] = useState(false);
  const [confirmError, setConfirmError] = useState<string | null>(null);
  const [lightboxId, setLightboxId] = useState<string | null>(null);
  const [dragging, setDragging] = useState(false);
  const [batching, setBatching] = useState(false);
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [copyToast, setCopyToast] = useState<{ type: "success" | "error"; text: string } | null>(null);
  const [notice, setNotice] = useState<{ type: "success" | "error"; text: string } | null>(null);
  const [tagWarning, setTagWarning] = useState<string | null>(null);
  const [viewMode, setViewMode] = useState<"wall" | "sheet">(() => {
    try {
      return window.localStorage.getItem("picbox-gallery-mode") === "sheet" ? "sheet" : "wall";
    } catch {
      return "wall";
    }
  });
  // 运行时上限优先（Go /api/config）；未提供时沿用 Next 的构建期变量。
  const uploadMaxBytes = configuredUploadMaxBytes ?? getClientUploadMaxBytes();
  const uploadMaxLabel = formatUploadMaxLabel(uploadMaxBytes);

  const changeViewMode = (mode: "wall" | "sheet") => {
    setViewMode(mode);
    try {
      window.localStorage.setItem("picbox-gallery-mode", mode);
    } catch {
      // 忽略：隐私模式或禁用存储时仅保留当前会话状态。
    }
  };

  useEffect(() => {
    const timer = window.setTimeout(() => setQuery(queryInput.trim()), 300);
    return () => window.clearTimeout(timer);
  }, [queryInput]);

  const fetchImages = useCallback(async (cursor?: string): Promise<ImagesResponse> => {
    const params = new URLSearchParams({ mine: "true", limit: "40" });
    // 回收站与可见性筛选互斥：回收站列出的是已软删除的图片。
    if (trashed) {
      params.set("trashed", "true");
    } else {
      params.set("visibility", visibility);
    }
    if (query) params.set("query", query);
    tagFilter.forEach((tag) => params.append("tag", tag));
    if (tagFilter.length > 0) params.set("tagMatch", tagMatch);
    if (cursor) params.set("cursor", cursor);
    params.set("sort", sort);
    const res = await fetch(`/api/images?${params}`);
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data?.error?.message || data?.message || "加载图片失败");
    return data as ImagesResponse;
  }, [query, visibility, trashed, tagFilter, tagMatch, sort]);

  // 标签候选是次要数据：HTTP/网络/JSON 失败都在内部消化，保留上一次候选，
  // 用独立的可重试提示呈现，绝不把已成功的图片列表刷新或标签保存报成失败。
  const reloadTags = useCallback(async () => {
    try {
      const res = await fetch("/api/images/tags");
      if (!res.ok) throw new Error("加载标签失败");
      const data = (await res.json().catch(() => null)) as { tags?: unknown } | null;
      if (!data || !Array.isArray(data.tags) || !data.tags.every((tag) => typeof tag === "string")) throw new Error("加载标签失败");
      setTags(data.tags as string[]);
      setTagWarning(null);
    } catch {
      setTagWarning("标签候选加载失败，可重试；不影响图片列表与保存结果");
    }
  }, []);

  useEffect(() => { void reloadTags(); }, [reloadTags]);

  // 用量是辅助信息：失败时保留上一次结果，不影响图片列表与上传。
  const reloadUsage = useCallback(async () => {
    try {
      const res = await fetch("/api/images/usage");
      const data = await res.json().catch(() => ({}));
      if (!res.ok) return;
      setUsage({
        usedBytes: Number(data.usedBytes) || 0,
        quotaBytes: Number(data.quotaBytes) || 0,
      });
    } catch {
      // 忽略：下次刷新会再试。
    }
  }, []);

  useEffect(() => { void reloadUsage(); }, [reloadUsage]);

  // 复制反馈只做浮层提示，自动消失，不占文档流去挤压列表控件。
  useEffect(() => {
    if (!copyToast) return;
    const timer = window.setTimeout(() => setCopyToast(null), 2200);
    return () => window.clearTimeout(timer);
  }, [copyToast]);

  // 行内 notice（批量操作、加载错误等）也自动消失，避免过期信息一直挂在列表上方。
  // 成功 4s、错误 8s；保留关闭按钮让用户可立即收起。
  useEffect(() => {
    if (!notice) return;
    const timer = window.setTimeout(() => setNotice(null), notice.type === "error" ? 8000 : 4000);
    return () => window.clearTimeout(timer);
  }, [notice]);

  useEffect(() => {
    const requestId = ++requestIdRef.current;
    setSelected(new Set());
    setLoading(true);
    setLoadError(null);
    setNotice(null);
    fetchImages()
      .then((data) => {
        if (requestId !== requestIdRef.current) return;
        setItems(data.items);
        setNextCursor(data.nextCursor);
        setTotal(data.total);
      })
      .catch((error: Error) => {
        // 保留上一批结果，不清空网格；失败单独用 loadError 呈现，避免和“没有图片”同时出现。
        if (requestId === requestIdRef.current) setLoadError(error.message);
      })
      .finally(() => {
        if (requestId === requestIdRef.current) setLoading(false);
      });
  }, [fetchImages]);

  const tagCandidates = [...new Set([...tags, ...tagFilter])]
    .sort()
    .filter((tag) => tag.toLowerCase().includes(tagQuery.trim().toLowerCase()));

  const loadMoreRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    if (!nextCursor || loading || loadingMore) return;
    const node = loadMoreRef.current;
    if (!node || typeof IntersectionObserver === "undefined") return;
    const observer = new IntersectionObserver(
      (entries) => { if (entries[0]?.isIntersecting) void loadMore(); },
      { rootMargin: "200px" }
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, [nextCursor, loading, loadingMore, fetchImages]);

  const groups = useMemo<Array<[string | null, ImageItem[]]>>(() => {
    // 名称/大小排序与日期分组冲突，统一放进一个不带标题的组。
    if (sort === "name" || sort === "size") return [[null, items]];
    const grouped = new Map<string, ImageItem[]>();
    for (const image of items) {
      const label = imageDateLabel(image.createdAt);
      grouped.set(label, [...(grouped.get(label) || []), image]);
    }
    // 分组顺序跟随 API 的全局时间顺序：newest 是「今天 → 过去」，
    // oldest 自然是「过去 → 今天」，不需要再反转。
    return [...grouped.entries()];
  }, [items, sort]);

  async function reload() {
    const requestId = ++requestIdRef.current;
    try {
      const data = await fetchImages();
      if (requestId !== requestIdRef.current) return;
      setItems(data.items);
      setNextCursor(data.nextCursor);
      setTotal(data.total);
      setLoadError(null);
      await reloadTags();
      await reloadUsage();
    } catch (error) {
      if (requestId === requestIdRef.current) {
        setNotice({
          type: "error",
          text: error instanceof Error ? error.message : "刷新图片列表失败",
        });
      }
    } finally {
      if (requestId === requestIdRef.current) setLoading(false);
    }
  }

  async function loadMore() {
    if (!nextCursor || loadingMore) return;
    const requestId = requestIdRef.current;
    setLoadingMore(true);
    try {
      const data = await fetchImages(nextCursor);
      if (requestId !== requestIdRef.current) return;
      setItems((current) => [...current, ...data.items]);
      setNextCursor(data.nextCursor);
      setTotal(data.total);
    } catch (error) {
      if (requestId === requestIdRef.current) setNotice({ type: "error", text: error instanceof Error ? error.message : "加载更多失败" });
    } finally {
      setLoadingMore(false);
    }
  }

  function toggleSelection(id: string) {
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id); else next.add(id);
      return next;
    });
  }

  async function executeBatch(
    ids: string[],
    action: BatchAction,
    visible?: boolean
  ): Promise<{ ok: boolean; message?: string }> {
    setBatching(true);
    setNotice(null);
    try {
      const res = await fetch("/api/images/batch", {
        method: "PATCH",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ ids, action, ...(action === "visibility" ? { visible } : {}) }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(data?.error?.message || "批量操作失败");

      const updatedCount = Number(data?.updatedCount) || 0;
      const failedCount = Number(data?.failedCount) || 0;
      const cleanupPending = Number(data?.cleanupPending) || 0;

      // 彻底删除用真实删掉的 id 集合（Go 的 updatedIds，含 cleanupPending 条目）；
      // 原 Next 后端没有该字段，此时沿用既有「请求的 id 全部成功」行为。
      if (action === "purge") {
        const updatedIds = Array.isArray(data?.updatedIds)
          ? (data.updatedIds as unknown[]).filter((id): id is string => typeof id === "string")
          : null;
        const removedIds = updatedIds ?? ids;
        const removedSet = new Set(removedIds);
        setItems((current) => current.filter((image) => !removedSet.has(image.id)));
        setTotal((current) => Math.max(0, current - (updatedIds ? removedIds.length : updatedCount)));
        // 失败项必须保持选中：否则用户看不到、也无法只重试它们。
        setSelected((current) => new Set([...current].filter((id) => !removedSet.has(id))));
        const parts = [`已彻底删除 ${updatedCount} 张`];
        if (failedCount > 0) parts.push(`${failedCount} 张删除失败，仍保留在列表中`);
        // DB 行已删除，这些条目不会进入保留期清理；文件字节只在下次服务启动时重试清理。
        if (cleanupPending > 0) parts.push(`${cleanupPending} 张的文件清理尚未完成，请联系管理员检查日志并重启服务重试`);
        await reloadTags();
        void reloadUsage();
        setNotice({ type: failedCount > 0 ? "error" : "success", text: parts.join("；") });
        return { ok: true };
      }

      const idSet = new Set(ids);
      // 删除、恢复会让图片离开当前列表（分别是我的图片 / 回收站）。
      const leavesCurrentList = action === "delete" || action === "restore";
      if (leavesCurrentList) {
        setItems((current) => current.filter((image) => !idSet.has(image.id)));
        setTotal((current) => Math.max(0, current - updatedCount));
      } else if ((visibility === "visible" && !visible) || (visibility === "hidden" && visible)) {
        setItems((current) => current.filter((image) => !idSet.has(image.id)));
        setTotal((current) => Math.max(0, current - updatedCount));
      } else {
        setItems((current) => current.map((image) => idSet.has(image.id) ? { ...image, isVisible: Boolean(visible) } : image));
      }
      await reloadTags();
      void reloadUsage();
      setSelected(new Set());
      setNotice({ type: "success", text: `已${BATCH_ACTION_LABEL[action]} ${updatedCount} 张图片` });
      return { ok: true };
    } catch (error) {
      const message = error instanceof Error ? error.message : "批量操作失败";
      if (action === "purge") {
        // 请求可能已在服务端部分生效（网络中断），或选择里含已被删除的 id。
        // 先按服务端现状刷新并清空选择，避免用旧 id 重试导致整批 403。
        try {
          await reload();
        } catch {
          // reload 自己会把失败写到 loadError；这里保留原始操作错误。
        }
        setSelected(new Set());
      }
      setNotice({ type: "error", text: message });
      return { ok: false, message };
    } finally {
      setBatching(false);
    }
  }

  async function runBatch(ids: string[], action: BatchAction, visible?: boolean) {
    if (ids.length === 0) return;
    if (action === "delete" || action === "purge") {
      setConfirmError(null);
      setBatchConfirm({ ids, action });
      return;
    }
    await executeBatch(ids, action, visible);
  }

  async function confirmBatch() {
    if (!batchConfirm) return;
    setConfirmBusy(true);
    setConfirmError(null);
    const result = await executeBatch(batchConfirm.ids, batchConfirm.action);
    setConfirmBusy(false);
    if (result.ok || batchConfirm.action === "purge") {
      // purge 失败时 executeBatch 已按服务端现状刷新列表并清空选择；旧快照里的 id 可能已被删除，
      // 保留确认框会让用户再次提交旧 id 并整批 403。关闭确认框，错误留在页面 notice 上，
      // 用户按刷新后的列表重新选择。
      setBatchConfirm(null);
      setConfirmError(null);
    } else {
      setConfirmError(result.message || "批量操作失败");
    }
  }

  // 与详情页 CopyButtons 保持一致：![原始文件名](图片地址)。
  function markdownImage(image: ImageItem): string {
    return `![${image.originalName}](${image.url})`;
  }

  async function copy(text: string, success: string, id: string) {
    if (!(await copyText(text))) {
      setCopyToast({ type: "error", text: "复制失败，请手动选中内容复制" });
      return;
    }
    setCopiedId(id);
    window.setTimeout(() => setCopiedId(null), 1500);
    setCopyToast({ type: "success", text: success });
  }

  function copyMarkdown(image: ImageItem) {
    return copy(markdownImage(image), "Markdown 已复制", image.id);
  }

  function copyUrl(image: ImageItem) {
    return copy(image.url, "链接已复制", image.id);
  }

  const allLoadedSelected = items.length > 0 && items.every((image) => selected.has(image.id));
  const singleSelected = selected.size === 1 ? items.find((image) => selected.has(image.id)) : undefined;
  const hasActiveFilter =
    query.length > 0 || tagFilter.length > 0 || (!trashed && visibility !== "all");
  // 「导出全部」不受筛选影响，筛选后为空时仍可能有可导出的图片；
  // 只有在没有筛选且确实一张都没有时才禁用。
  const canExportAll = !loading && (total > 0 || hasActiveFilter);

  function clearFilters() {
    setQueryInput("");
    setQuery("");
    setTagFilter([]);
    setVisibility("all");
  }

  function clearSearchFilter() {
    // 同时清理防抖值与输入框，避免待执行定时器把搜索词写回来。
    setQueryInput("");
    setQuery("");
  }

  function removeTagFilter(tag: string) {
    setTagFilter((current) => current.filter((entry) => entry !== tag));
  }

  return (
    <div
      className={`image-manager${dragging ? " is-dragging" : ""}${selected.size > 0 ? " has-selection" : ""}`}
      aria-busy={loading}
      onDragOver={(e) => { if (!trashed && e.dataTransfer.types.includes("Files")) { e.preventDefault(); setDragging(true); } }}
      onDragLeave={(e) => { if (!e.currentTarget.contains(e.relatedTarget as Node)) setDragging(false); }}
      onDrop={(e) => { e.preventDefault(); setDragging(false); if (!trashed && e.dataTransfer.files.length) setUploadFiles(Array.from(e.dataTransfer.files)); }}
    >
      {dragging && <div className="image-drop-overlay">松开加入待上传列表（支持多张）</div>}
      {uploadFiles !== null && <UploadDialog initialFiles={uploadFiles} suggestions={tags} maxBytes={uploadMaxBytes} usage={usage} concurrency={DEFAULT_UPLOAD_CONCURRENCY}
        onClose={() => setUploadFiles(null)} onUploaded={reload} />}
      {tagEditing && <EditTagsDialog ids={tagEditing.ids} initialTags={tagEditing.tags} suggestions={tags}
        onClose={() => setTagEditing(null)} onSaved={async () => { await reload(); setSelected(new Set()); setNotice({ type: "success", text: "图片标签已更新" }); }} />}
      {exportRequest && <ExportDialog mode={exportRequest.mode} ids={exportRequest.ids} onClose={() => setExportRequest(null)} />}
      {batchConfirm && (
        <ConfirmDialog
          title={batchConfirm.action === "delete" ? "移入回收站" : "彻底删除"}
          message={
            batchConfirm.action === "delete"
              ? `确定删除选中的 ${batchConfirm.ids.length} 张图片吗？图片会移入回收站，可在彻底删除前恢复。`
              : `确定彻底删除选中的 ${batchConfirm.ids.length} 张图片吗？此操作不可恢复，删除后无法找回。`
          }
          confirmLabel={batchConfirm.action === "delete" ? "移入回收站" : "彻底删除"}
          danger
          busy={confirmBusy}
          error={confirmError}
          onConfirm={() => void confirmBatch()}
          onClose={() => {
            if (!confirmBusy) {
              setBatchConfirm(null);
              setConfirmError(null);
            }
          }}
        />
      )}
      {lightboxId && (
        <ImageLightbox
          items={items}
          initialId={lightboxId}
          trashed={trashed}
          onClose={() => setLightboxId(null)}
        />
      )}

      <div className="image-page-heading">
        <div>
          <div className="image-page-eyebrow">{trashed ? "ARCHIVE · 回收站" : "COLLECTION · 全部馆藏"}</div>
          <h1>{trashed ? "回收站" : "我的图片"}</h1>
          <p>{trashed ? "恢复误删的图片，或彻底删除不再需要的文件。" : "在装裱画廊墙与接触印相表之间切换，整理、分享或导出馆藏。"}</p>
        </div>
        {!trashed && <button type="button" className="image-upload-primary" onClick={() => setUploadFiles([])}>+ 上传图片</button>}
      </div>

      <div className="image-overview" aria-live="polite">
        <span className="image-total">已加载 {items.length} / 共 <strong>{total}</strong> 张{trashed ? "待处理图片" : "图片"}</span>
        {usage && usage.quotaBytes > 0 && (
          <span
            className={`image-quota${usage.usedBytes >= usage.quotaBytes ? " is-full" : ""}`}
            title={`${usage.usedBytes} / ${usage.quotaBytes} bytes（含回收站与管理员隐藏/下架的图片）`}
          >
            已用 {formatBytes(usage.usedBytes)} / {formatBytes(usage.quotaBytes)}
          </span>
        )}
        {loading && items.length > 0 && <span className="image-updating" role="status">更新中…</span>}
      </div>

      <section className="image-filter-panel" aria-label="搜索与筛选">
        <div className="image-toolbar">
          <div className="image-search-wrap">
            <span aria-hidden="true">⌕</span>
            <input value={queryInput} onChange={(e) => setQueryInput(e.target.value)} placeholder="搜索文件名" aria-label="搜索文件名" />
            {queryInput && <button type="button" onClick={clearSearchFilter} aria-label="清空搜索">×</button>}
          </div>
          {trashed ? null : (
            <select value={visibility} onChange={(e) => setVisibility(e.target.value as VisibilityFilter)} aria-label="可见状态筛选">
              <option value="all">全部状态</option>
              <option value="visible">仅可见</option>
              <option value="hidden">仅不可见</option>
            </select>
          )}
          <select aria-label="排序方式" value={sort} onChange={(event) => setSort(event.target.value as typeof sort)}>
            <option value="newest">最新优先</option>
            <option value="oldest">最旧优先</option>
            <option value="name">按文件名</option>
            <option value="size">按文件大小</option>
          </select>
          <details className="image-tag-filter">
            <summary>标签筛选{tagFilter.length > 0 ? `（${tagFilter.length}）` : ""}</summary>
            <div className="image-tag-filter-options">
              <input className="image-tag-search" value={tagQuery} onChange={(event) => setTagQuery(event.target.value)} placeholder="搜索标签" aria-label="搜索标签" />
              {tagCandidates.map((tag) => <label key={tag}>
                <input type="checkbox" checked={tagFilter.includes(tag)} disabled={!tagFilter.includes(tag) && tagFilter.length >= MAX_IMAGE_TAGS}
                  onChange={(event) => setTagFilter((current) => event.target.checked ? [...current, tag] : current.filter((entry) => entry !== tag))} />
                <span>{tag}</span>
              </label>)}
              {tagCandidates.length === 0 && <span>{tagQuery.trim() ? "没有匹配的标签" : "暂无标签"}</span>}
              {tagFilter.length >= MAX_IMAGE_TAGS && <span className="image-help">最多同时筛选 {MAX_IMAGE_TAGS} 个标签</span>}
              {tagFilter.length > 0 && <button type="button" className="image-button" onClick={() => setTagFilter([])}>清空标签筛选</button>}
            </div>
          </details>
          {tagFilter.length > 0 && <select aria-label="标签匹配方式" value={tagMatch} onChange={(event) => setTagMatch(event.target.value as "any" | "all")}>
            <option value="any">包含任意标签</option>
            <option value="all">包含全部标签</option>
          </select>}
        </div>

        {(queryInput.trim() || (!trashed && visibility !== "all") || tagFilter.length > 0) && (
          <div className="image-active-filters" aria-label="当前筛选条件">
            {queryInput.trim() && <button type="button" className="image-filter-chip" onClick={clearSearchFilter} aria-label={`移除文件名筛选：${queryInput.trim()}`}>
              文件名：{queryInput.trim()} <span aria-hidden="true">×</span>
            </button>}
            {!trashed && visibility !== "all" && <button type="button" className="image-filter-chip" onClick={() => setVisibility("all")} aria-label={`移除可见状态筛选：${visibility === "visible" ? "仅可见" : "仅不可见"}`}>
              状态：{visibility === "visible" ? "仅可见" : "仅不可见"} <span aria-hidden="true">×</span>
            </button>}
            {tagFilter.map((tag) => <button type="button" className="image-filter-chip" key={tag} onClick={() => removeTagFilter(tag)} aria-label={`移除标签筛选：${tag}`}>
              标签（{tagMatch === "all" ? "全部匹配" : "任意匹配"}）：{tag} <span aria-hidden="true">×</span>
            </button>)}
            <button type="button" className="image-filter-clear" onClick={clearFilters}>清除全部筛选</button>
          </div>
        )}
      </section>

      <div className="image-manager-controls">
        <div className="image-toolbar-secondary">
          <button
            type="button"
            className={trashed ? "image-secondary-button is-active" : "image-secondary-button"}
            onClick={() => setTrashed((current) => !current)}
            aria-pressed={trashed}
          >
            {trashed ? "← 返回我的图片" : "🗑 回收站"}
          </button>
          {!trashed && (
            <button
              type="button"
              className="image-secondary-button"
              disabled={!canExportAll}
              title={canExportAll ? undefined : "还没有可导出的图片"}
              onClick={() => setExportRequest({ mode: "all", ids: [] })}
            >
              ⬇ 导出全部
            </button>
          )}
          <div className="mode-switch" role="group" aria-label="视图模式">
            <button
              type="button"
              className={viewMode === "wall" ? "is-active" : undefined}
              aria-pressed={viewMode === "wall"}
              onClick={() => changeViewMode("wall")}
            >
              画廊墙
            </button>
            <button
              type="button"
              className={viewMode === "sheet" ? "is-active" : undefined}
              aria-pressed={viewMode === "sheet"}
              onClick={() => changeViewMode("sheet")}
            >
              接触印相表
            </button>
          </div>
        </div>

        <div className="image-toolbar-sub">
          <label className="image-select-all">
            <input
              type="checkbox"
              checked={allLoadedSelected}
              disabled={items.length === 0}
              onChange={() => setSelected(allLoadedSelected ? new Set() : new Set(items.map((image) => image.id)))}
            />
            全选已加载（{items.length}）
          </label>
          <span>
            {trashed
              ? "回收站中的图片会自动清理，请及时恢复需要保留的图片"
              : `勾选图片后使用底部工具栏操作；支持一次选择或拖入多张 PNG、JPG、GIF、WEBP，单张最大 ${uploadMaxLabel}`}
          </span>
        </div>
      </div>

      {notice && (
        <div className={`image-notice is-dismissible ${notice.type}`} role={notice.type === "error" ? "alert" : "status"}>
          <span>{notice.text}</span>
          <button type="button" className="image-notice-close" aria-label="关闭提示" onClick={() => setNotice(null)}>×</button>
        </div>
      )}
      {tagWarning && <div className="image-notice warn" role="status">
        <span>{tagWarning}</span>
        <button type="button" className="image-button" onClick={() => void reloadTags()}>重试加载标签</button>
      </div>}

      {selected.size > 0 && (
        <div className="batch-toolbar">
          <strong>已选择 {selected.size} 张</strong>
          {trashed ? (
            <>
              <button style={btn("outline")} disabled={batching} onClick={() => runBatch([...selected], "restore")}>恢复</button>
              <button style={btn("danger")} disabled={batching} onClick={() => runBatch([...selected], "purge")}>彻底删除</button>
            </>
          ) : (
            <>
              {singleSelected && (
                <>
                  <button style={btn("primary")} disabled={batching} onClick={() => void copyMarkdown(singleSelected)}>复制 Markdown</button>
                  <button style={btn("outline")} disabled={batching} onClick={() => void copyUrl(singleSelected)}>复制链接</button>
                </>
              )}
              <button style={btn("outline")} disabled={batching} onClick={() => runBatch([...selected], "visibility", true)}>设为可见</button>
              <button style={btn("ghost")} disabled={batching} onClick={() => runBatch([...selected], "visibility", false)}>设为不可见</button>
              <button style={btn("outline")} disabled={batching} onClick={() => setTagEditing({ ids: [...selected] })}>编辑标签</button>
              <button style={btn("outline")} disabled={batching} onClick={() => setExportRequest({ mode: "selected", ids: [...selected] })}>⬇ 导出选中</button>
              <button style={btn("danger")} disabled={batching} onClick={() => runBatch([...selected], "delete")}>删除</button>
              {selected.size > EXPORT_SELECTED_MAX && <span className="export-limit-hint">导出选中一次最多 {EXPORT_SELECTED_MAX} 张</span>}
            </>
          )}
          <button style={btn("ghost")} disabled={batching} onClick={() => setSelected(new Set())}>取消选择</button>
        </div>
      )}

      {loading && items.length === 0 && (
        <div role="status" aria-label="正在加载图片列表">
          <ImageGridSkeleton />
        </div>
      )}
      {loadError && (
        <div className="image-empty" role="alert">
          <EmptyArt />
          <p>图片列表加载失败：{loadError}</p>
          <button type="button" className="image-button" onClick={() => void reload()}>重试</button>
        </div>
      )}
      {!loading && !loadError && items.length === 0 && (
        <div className="image-empty">
          <EmptyArt />
          {trashed ? (
            <p>回收站是空的</p>
          ) : hasActiveFilter ? (
            <>
              <p>没有符合条件的图片</p>
              <button type="button" className="image-button" onClick={clearFilters}>清除筛选</button>
            </>
          ) : (
            <>
              <p>还没有上传图片</p>
              <button type="button" className="image-button primary" onClick={() => setUploadFiles([])}>上传第一张图片</button>
            </>
          )}
        </div>
      )}

      {groups.map(([label, images]) => (
        <section className="image-date-group" key={label ?? "all"}>
          {label && <h2>{label}<span>{images.length} 张</span></h2>}
          {viewMode === "wall" ? (
            <div className="compact-image-grid">
              {images.map((image) => (
                <article className={`compact-image-card${selected.has(image.id) ? " selected" : ""}`} key={image.id}>
                  <label className="image-selector" onClick={(e) => e.stopPropagation()}>
                    <input type="checkbox" checked={selected.has(image.id)} onChange={() => toggleSelection(image.id)} aria-label={`选择 ${image.originalName}`} />
                  </label>
                  <div className="compact-image-media">
                    <button
                      type="button"
                      className="compact-image-preview"
                      aria-label={`预览 ${image.originalName}`}
                      onClick={() => setLightboxId(image.id)}
                    >
                      <img src={image.thumbUrl} alt={image.originalName} loading="lazy" decoding="async" />
                    </button>
                    {!trashed && (
                      <button
                        type="button"
                        className="compact-image-quick-copy"
                        aria-label="复制 Markdown"
                        onClick={() => void copyMarkdown(image)}
                      >
                        {copiedId === image.id ? "✓" : "⧉"}
                      </button>
                    )}
                  </div>
                  <div className="compact-image-info">
                    <div className="compact-image-name" title={image.originalName}>{image.originalName}</div>
                    <div className="compact-image-tags">{image.tags.map((tag) => <span className="compact-image-tag" title={tag} key={tag}>{tag}</span>)}</div>
                    <div className="compact-image-spec">
                      {image.width && image.height ? `${image.width}×${image.height} · ` : ""}{formatBytes(image.size)}
                    </div>
                    <div className="compact-image-meta">
                      <time dateTime={image.createdAt} title={formatDate(image.createdAt)}>
                        {label ? formatTime(image.createdAt) : formatDate(image.createdAt)}
                      </time>
                      {trashed ? (
                        <span className="hidden">已删除</span>
                      ) : (
                        <span className={image.isVisible ? "visible" : "hidden"}>{image.isVisible ? "可见" : "不可见"}</span>
                      )}
                    </div>
                  </div>
                </article>
              ))}
            </div>
          ) : (
            <div className="contact-sheet-wrap">
              <table className="contact-sheet">
                <thead>
                  <tr>
                    <th className="contact-check-col" aria-label="选择" />
                    <th>印样</th>
                    <th>文件名</th>
                    <th>分辨率</th>
                    <th>大小</th>
                    <th>时间</th>
                    <th>状态</th>
                    {!trashed && <th>操作</th>}
                  </tr>
                </thead>
                <tbody>
                  {images.map((image) => (
                    <tr
                      key={image.id}
                      className={selected.has(image.id) ? "selected" : undefined}
                      onClick={() => setLightboxId(image.id)}
                    >
                      <td className="contact-check-col" onClick={(e) => e.stopPropagation()}>
                        <input
                          type="checkbox"
                          checked={selected.has(image.id)}
                          onChange={() => toggleSelection(image.id)}
                          aria-label={`选择 ${image.originalName}`}
                        />
                      </td>
                      <td>
                        <button
                          type="button"
                          className="contact-thumb"
                          aria-label={`预览 ${image.originalName}`}
                          onClick={(e) => {
                            e.stopPropagation();
                            setLightboxId(image.id);
                          }}
                        >
                          <img src={image.thumbUrl} alt={image.originalName} loading="lazy" decoding="async" />
                        </button>
                      </td>
                      <td className="contact-name-cell">
                        <div className="contact-name-text" title={image.originalName}>{image.originalName}</div>
                        {image.tags.length > 0 && (
                          <div className="contact-tags">
                            {image.tags.map((tag) => (
                              <span className="compact-image-tag" title={tag} key={tag}>{tag}</span>
                            ))}
                          </div>
                        )}
                      </td>
                      <td className="contact-mono">{image.width && image.height ? `${image.width} × ${image.height}` : "—"}</td>
                      <td className="contact-mono">{formatBytes(image.size)}</td>
                      <td className="contact-mono">
                        <time dateTime={image.createdAt} title={formatDate(image.createdAt)}>
                          {label ? formatTime(image.createdAt) : formatDate(image.createdAt)}
                        </time>
                      </td>
                      <td>
                        {trashed ? (
                          <span className="contact-status is-hidden">已删除</span>
                        ) : (
                          <span className={`contact-status ${image.isVisible ? "is-visible" : "is-hidden"}`}>
                            {image.isVisible ? "可见" : "不可见"}
                          </span>
                        )}
                      </td>
                      {!trashed && (
                        <td onClick={(e) => e.stopPropagation()}>
                          <button
                            type="button"
                            className="contact-copy-btn"
                            aria-label="复制 Markdown"
                            onClick={() => void copyMarkdown(image)}
                          >
                            {copiedId === image.id ? "✓ 已复制" : "⧉ 复制"}
                          </button>
                        </td>
                      )}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      ))}

      {nextCursor && (
        <div ref={loadMoreRef} style={{ textAlign: "center", marginTop: 24 }}>
          <button style={btn("ghost")} onClick={loadMore} disabled={loadingMore}>{loadingMore ? "加载中..." : "加载更多"}</button>
        </div>
      )}

      {copyToast && (
        <div className={`image-copy-toast ${copyToast.type}`} role="status">
          {copyToast.type === "success" ? "✓ " : "! "}{copyToast.text}
        </div>
      )}
    </div>
  );
}

// ─── API Key 标签页 ───────────────────────────────────────────────────────────
function ApiKeysTab() {
  const [keys, setKeys] = useState<ApiKeyItem[]>([]);
  const [loading, setLoading] = useState(true);
  const [newName, setNewName] = useState("");
  const [creating, setCreating] = useState(false);
  const [newRawKey, setNewRawKey] = useState<string | null>(null);
  const [copiedNew, setCopiedNew] = useState(false);
  const [revokeTarget, setRevokeTarget] = useState<ApiKeyItem | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);
  const [revokeError, setRevokeError] = useState<string | null>(null);
  const [confirmHideKey, setConfirmHideKey] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);

  async function loadKeys() {
    setLoading(true);
    setLoadError(null);
    try {
      const res = await fetch("/api/apikeys");
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setLoadError(data?.error?.message || "加载 API Key 失败");
        return;
      }
      setKeys(data.keys || []);
    } catch {
      setLoadError("网络异常，加载 API Key 失败");
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => { loadKeys(); }, []);

  async function createKey() {
    if (!newName.trim()) return;
    setCreateError(null);
    setCreating(true);
    try {
      const res = await fetch("/api/apikeys", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ name: newName.trim() }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setCreateError(data?.error?.message || "创建失败");
        return;
      }
      setNewRawKey(data.key.rawKey);
      setCopiedNew(false);
      setNewName("");
      await loadKeys();
    } finally {
      setCreating(false);
    }
  }

  async function revokeKey(key: ApiKeyItem) {
    setRevokeBusy(true);
    setRevokeError(null);
    try {
      const res = await fetch(`/api/apikeys/${key.id}`, { method: "DELETE" });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        setRevokeError(data?.error?.message || "撤销 API Key 失败，请重试");
        return;
      }
      setKeys((prev) => prev.filter((k) => k.id !== key.id));
      setRevokeTarget(null);
    } catch {
      setRevokeError("网络异常，撤销 API Key 失败");
    } finally {
      setRevokeBusy(false);
    }
  }

  async function copyNewKey() {
    if (!newRawKey) return;
    const copied = await copyText(newRawKey);
    if (!copied) {
      setActionError("复制失败，请手动选中 Key 复制");
      return;
    }
    setActionError(null);
    setCopiedNew(true);
  }

  return (
    <div>
      {actionError && <div className="image-notice error" role="alert">{actionError}</div>}
      {/* 生成新 Key 表单 */}
      <div style={{ ...card, marginBottom: 20 }}>
        <h3 style={{ margin: "0 0 14px", fontSize: 15, fontWeight: 600 }}>生成新 API Key</h3>
        <div style={{ display: "flex", gap: 10, flexWrap: "wrap" }}>
          <input
            style={{ ...input, maxWidth: 320 }}
            placeholder="Key 名称（如：PicGo、自用脚本）"
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            onKeyDown={(e) => e.key === "Enter" && createKey()}
          />
          <button style={btn("primary")} onClick={createKey} disabled={creating || !newName.trim()}>
            {creating ? "生成中..." : "生成 Key"}
          </button>
        </div>
        {createError && <p style={{ margin: "8px 0 0", fontSize: 13, color: colors.danger }}>{createError}</p>}

        {newRawKey && (
          <div style={{
            marginTop: 16,
            padding: 16,
            borderRadius: 2,
            background: "var(--ph-warning-soft)",
            border: "1px solid var(--ph-warning-border)",
          }}>
            <div style={{ fontSize: 13, fontWeight: 600, color: "var(--ph-warning-text)", marginBottom: 8 }}>
              ⚠️ 请立即保存此 Key，关闭后将无法再次查看！
            </div>
            <div style={{ display: "flex", gap: 10, alignItems: "center", flexWrap: "wrap" }}>
              <code style={{
                flex: 1,
                fontSize: 12,
                background: "var(--ph-code-bg)",
                padding: "8px 12px",
                borderRadius: 2,
                wordBreak: "break-all",
                color: "var(--ph-warning-text)",
              }}>
                {newRawKey}
              </code>
              <button style={btn("outline")} onClick={copyNewKey}>
                {copiedNew ? "✅ 已复制" : "复制"}
              </button>
              <button style={{ ...btn("ghost"), fontSize: 18, padding: "2px 8px" }} onClick={() => setConfirmHideKey(true)} aria-label="关闭并隐藏此 Key">
                ×
              </button>
            </div>
            <div style={{ fontSize: 12, color: colors.muted, marginTop: 8 }}>
              使用方式：<code style={{ background: "var(--ph-code-bg)", padding: "2px 6px", borderRadius: 2 }}>
                curl -X POST https://your-domain/api/images -H &quot;Authorization: Bearer {"{API_KEY}"}&quot; -F &quot;file=@image.jpg&quot;
              </code>
            </div>
          </div>
        )}
      </div>

      {/* Key 列表 */}
      <div style={card}>
        <h3 style={{ margin: "0 0 14px", fontSize: 15, fontWeight: 600 }}>
          已有 API Key {!loading && `(${keys.length})`}
        </h3>
        {loading && <p style={{ color: colors.muted }}>加载中...</p>}
        {!loading && loadError && (
          <div role="alert">
            <p style={{ color: colors.danger, margin: "0 0 8px" }}>{loadError}</p>
            <button style={btn("outline")} onClick={() => void loadKeys()}>重试</button>
          </div>
        )}
        {!loading && !loadError && keys.length === 0 && (
          <p style={{ color: colors.muted, margin: 0 }}>还没有 API Key，立即生成一个吧。</p>
        )}
        {!loadError && keys.length > 0 && (
          <div style={{ overflowX: "auto" }}>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
              <thead>
                <tr style={{ borderBottom: `1px solid ${colors.border}` }}>
                  {["名称", "前缀", "创建时间", "最后使用", "操作"].map((h) => (
                    <th key={h} style={{
                      textAlign: "left",
                      padding: "8px 12px",
                      color: colors.muted,
                      fontWeight: 500,
                      whiteSpace: "nowrap",
                    }}>{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <tr key={k.id} style={{ borderBottom: `1px solid ${colors.border}` }}>
                    <td style={{ padding: "10px 12px", fontWeight: 500 }}>{k.name}</td>
                    <td style={{ padding: "10px 12px" }}>
                      <code style={{ background: "var(--ph-code-bg)", padding: "2px 8px", borderRadius: 2, fontSize: 12 }}>
                        {k.prefix}…
                      </code>
                    </td>
                    <td style={{ padding: "10px 12px", color: colors.muted, whiteSpace: "nowrap" }}>
                      {formatDate(k.createdAt)}
                    </td>
                    <td style={{ padding: "10px 12px", color: colors.muted, whiteSpace: "nowrap" }}>
                      {k.lastUsedAt ? formatDate(k.lastUsedAt) : "从未"}
                    </td>
                    <td style={{ padding: "10px 12px" }}>
                      <button
                        style={btn("danger")}
                        onClick={() => {
                          setRevokeTarget(k);
                          setRevokeError(null);
                        }}
                      >
                        撤销
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {revokeTarget && (
        <ConfirmDialog
          title="撤销 API Key"
          message={
            <>
              确定撤销 <strong>{revokeTarget.name}</strong>（{revokeTarget.prefix}…）？
              撤销后无法恢复，使用该 Key 的客户端会立即失效。
            </>
          }
          confirmLabel="撤销"
          danger
          busy={revokeBusy}
          error={revokeError}
          onConfirm={() => void revokeKey(revokeTarget)}
          onClose={() => {
            if (!revokeBusy) {
              setRevokeTarget(null);
              setRevokeError(null);
            }
          }}
        />
      )}
      {confirmHideKey && (
        <ConfirmDialog
          title="关闭一次性 Key"
          message="关闭后将无法再次查看此 Key，确认已经保存？"
          confirmLabel="关闭"
          danger
          onConfirm={() => {
            setConfirmHideKey(false);
            setNewRawKey(null);
          }}
          onClose={() => setConfirmHideKey(false)}
        />
      )}
    </div>
  );
}
