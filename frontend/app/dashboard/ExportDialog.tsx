"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { ImageModal } from "./ImageDialogs";

// 与后端共享契约保持一致的导出限制。
export const EXPORT_SELECTED_MAX = 100;
export const EXPORT_ALL_MAX_IMAGES = 10000;
export const EXPORT_ALL_MAX_BYTES = 2 * 1024 * 1024 * 1024;

export type ExportMode = "all" | "selected";

type ExportSummary = { count: number | null; bytes: number | null };

function formatBytes(bytes: number) {
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  const exponent = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / 1024 ** exponent;
  return `${value >= 10 || exponent === 0 ? Math.round(value) : value.toFixed(1)} ${units[exponent]}`;
}

function parseHeaderNumber(raw: string | null) {
  if (raw === null || raw.trim() === "") return null;
  const value = Number.parseInt(raw, 10);
  return Number.isInteger(value) && value >= 0 ? value : null;
}

function decodeHeaderValue(raw: string) {
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

function statusMessage(status: number) {
  if (status === 400) return "导出请求无效，请调整导出范围后重试";
  if (status === 401) return "登录状态已失效，请重新登录后再导出";
  if (status === 403) return "没有权限导出所选图片";
  if (status === 404) return "导出内容不存在或已被清理";
  if (status === 413) return "导出内容超出大小限制，请减少导出数量";
  if (status === 429) return "刚刚导出过或已有导出正在进行，请稍后再试";
  if (status >= 500) return "导出服务暂时不可用，请稍后重试";
  return `导出校验失败（HTTP ${status}）`;
}

// HEAD 响应没有正文；优先读取约定的 X-Export-* 响应头，再对仍暴露 JSON
// 正文的运行环境做一次兜底解析，最后退回状态码文案。
async function readHeadError(response: Response) {
  const headerMessage = response.headers.get("X-Export-Message");
  if (headerMessage) {
    const decoded = decodeHeaderValue(headerMessage).trim();
    if (decoded) return decoded;
  }
  const headerCode = response.headers.get("X-Export-Error");
  const data = (await response.json().catch(() => null)) as
    | { error?: { message?: unknown; code?: unknown }; message?: unknown }
    | null;
  const bodyMessage = data?.error?.message ?? data?.message;
  if (typeof bodyMessage === "string" && bodyMessage.trim()) return bodyMessage.trim();
  if (headerCode) return `导出校验失败（${headerCode}）`;
  return statusMessage(response.status);
}

export default function ExportDialog({ mode, ids, onClose }: {
  mode: ExportMode;
  ids?: string[];
  onClose: () => void;
}) {
  const selectedIds = useMemo(() => (mode === "selected" ? [...(ids ?? [])] : []), [mode, ids]);
  const [phase, setPhase] = useState<"checking" | "ready" | "error">("checking");
  const [error, setError] = useState("");
  const [summary, setSummary] = useState<ExportSummary>({ count: null, bytes: null });
  const [cooldownHours, setCooldownHours] = useState<number | null>(null);
  const [started, setStarted] = useState(false);
  const [retryToken, setRetryToken] = useState(0);

  const requestUrl = useMemo(() => {
    const params = new URLSearchParams();
    if (mode === "all") {
      params.set("scope", "all");
    } else {
      selectedIds.forEach((id) => params.append("ids", id));
    }
    const query = params.toString();
    return `/api/images/export${query ? `?${query}` : ""}`;
  }, [mode, selectedIds]);

  // 客户端先拦截明显无效的请求，避免无谓的 HEAD。
  const validationError = useMemo(() => {
    if (mode === "all") return "";
    if (selectedIds.length === 0) return "请先选择要导出的图片";
    if (selectedIds.length > EXPORT_SELECTED_MAX) {
      return `一次最多导出 ${EXPORT_SELECTED_MAX} 张图片，当前已选择 ${selectedIds.length} 张，请减少选择`;
    }
    return "";
  }, [mode, selectedIds]);

  useEffect(() => {
    if (validationError) {
      setPhase("error");
      setError(validationError);
      setSummary({ count: null, bytes: null });
      setCooldownHours(null);
      return;
    }
    const controller = new AbortController();
    let active = true;
    setPhase("checking");
    setError("");
    setSummary({ count: null, bytes: null });
    setCooldownHours(null);
    fetch(requestUrl, {
      method: "HEAD",
      credentials: "same-origin",
      cache: "no-store",
      signal: controller.signal,
    })
      .then(async (response) => {
        if (!active) return;
        if (!response.ok) {
          const message = await readHeadError(response);
          if (!active) return;
          setError(message);
          setPhase("error");
          return;
        }
        setSummary({
          count: parseHeaderNumber(response.headers.get("X-Export-Count")),
          bytes: parseHeaderNumber(response.headers.get("X-Export-Bytes")),
        });
        const hours = parseHeaderNumber(response.headers.get("X-Export-Cooldown-Hours"));
        setCooldownHours(hours !== null && hours > 0 ? hours : null);
        setPhase("ready");
      })
      .catch((fetchError: unknown) => {
        if (!active || (fetchError instanceof Error && fetchError.name === "AbortError")) return;
        setError("无法连接导出服务，请检查网络后重试");
        setPhase("error");
      });
    return () => {
      active = false;
      controller.abort();
    };
  }, [requestUrl, validationError, retryToken]);

  const startDownload = useCallback(() => {
    if (phase !== "ready" || started) return;
    setStarted(true);
    // 交给浏览器原生链接下载：ZIP 由服务端流式写入磁盘，不会整体读入 JS 内存。
    // 我们无法也不应确认服务端是否完成打包，页面只负责发起下载。
    // 空 download 属性强制下载，同时保留服务端 Content-Disposition 的文件名。
    const link = document.createElement("a");
    link.href = requestUrl;
    link.download = "";
    link.rel = "noopener";
    document.body.appendChild(link);
    link.click();
    link.remove();
  }, [phase, started, requestUrl]);

  const allSummary = [
    summary.count !== null ? `约 ${summary.count} 张` : null,
    summary.bytes !== null ? `约 ${formatBytes(summary.bytes)}` : null,
  ].filter((entry): entry is string => Boolean(entry));
  const summaryText = allSummary.length > 0 ? `校验通过：${allSummary.join("，")}` : "校验通过，可以开始导出";
  const limitText = mode === "all"
    ? `全部导出每次最多 ${EXPORT_ALL_MAX_IMAGES} 张 / ${formatBytes(EXPORT_ALL_MAX_BYTES)}，超出时请分批导出。`
    : `一次最多勾选导出 ${EXPORT_SELECTED_MAX} 张；需要更多请使用“导出全部”。`;
  const scopeText = mode === "all"
    ? "导出你的全部个人图片，不受当前搜索、标签、可见性筛选或分页影响，未加载的图片也会包含在内。"
    : `导出当前勾选的 ${selectedIds.length} 张图片。`;
  const title = mode === "all" ? "导出全部个人图片" : `导出选中的 ${selectedIds.length} 张图片`;
  const canRetry = phase === "error" && !validationError && !started;

  return (
    <ImageModal title={title} busy={false} onClose={onClose}>
      <div className="image-dialog-body export-dialog-body">
        <p className="image-help export-intro">{scopeText}</p>
        <div className="export-notes">
          <p className="image-help">导出的是你上传时的原始文件，不会被压缩或转码；压缩包内附带的说明会记录原始文件名、大小、标签与上传时间。</p>
          <p className="image-help">隐藏（不可见）图片也会导出；回收站中的图片和管理员下架的图片不会导出。</p>
          <p className="image-help">{limitText}</p>
          {cooldownHours !== null && (
            <p className="image-help">每次导出成功后，同一账号需等待 {cooldownHours} 小时才能再次导出。</p>
          )}
        </div>

        {phase === "checking" && (
          <p className="image-notice progress" role="status">正在校验导出范围…</p>
        )}
        {phase === "ready" && (
          <p className="image-notice success" role="status">{summaryText}</p>
        )}
        {phase === "error" && (
          <p className="image-notice error" role="alert">{error}</p>
        )}
        {started && (
          <p className="image-notice progress" role="status">
            已交给浏览器下载，请在浏览器下载列表中查看进度；本页面无法确认下载是否完成。
          </p>
        )}
      </div>
      <footer className="image-dialog-footer">
        <button type="button" className="image-button" onClick={onClose}>关闭</button>
        {canRetry && (
          <button
            type="button"
            className="image-button"
            onClick={() => setRetryToken((current) => current + 1)}
          >
            重试校验
          </button>
        )}
        <button
          type="button"
          className="image-button primary"
          disabled={phase !== "ready" || started}
          onClick={startDownload}
        >
          {started ? "已开始下载" : phase === "checking" ? "校验中…" : "开始导出"}
        </button>
      </footer>
    </ImageModal>
  );
}
