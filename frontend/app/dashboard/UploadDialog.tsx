"use client";

import { useEffect, useRef, useState, type ClipboardEvent } from "react";
import { IMAGE_TAG_ERROR } from "@/lib/imageTag";
import { applyClipboardImages, canAcceptClipboardImages, normalizeClipboardImageFile, shouldHandlePaste } from "@/lib/clipboardImage";
import { ImageModal, ImagePreviewDialog, TagInput, draftTags, emptyTagDraft } from "./ImageDialogs";
import { formatBytes } from "@/lib/quota";
import { formatUploadMaxLabel } from "@/lib/uploadLimit";
import { DEFAULT_UPLOAD_CONCURRENCY } from "@/lib/uploadConcurrency";

type UploadItem = {
  id: string;
  file?: File;
  url?: string;
  preview?: string;
  label: string;
  status: "pending" | "uploading" | "success" | "error" | "invalid";
  error?: string;
};

// 单张图片的请求超时。卡住的请求必须能失败，否则该条目会永远停在“上传中…”。
const UPLOAD_TIMEOUT_MS = 120_000;

export default function UploadDialog({ initialFiles, suggestions, maxBytes, usage, concurrency = DEFAULT_UPLOAD_CONCURRENCY, onClose, onUploaded }: {
  initialFiles: File[]; suggestions: string[]; maxBytes: number; usage?: { usedBytes: number; quotaBytes: number } | null; concurrency?: number; onClose: () => void; onUploaded: () => Promise<void>;
}) {
  const [items, setItems] = useState<UploadItem[]>([]);
  const [tags, setTags] = useState(emptyTagDraft);
  const [urls, setUrls] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [refreshFailed, setRefreshFailed] = useState(false);
  const [started, setStarted] = useState(false);
  const [previewItem, setPreviewItem] = useState<{ src: string; label: string } | null>(null);
  const [clipboardMessage, setClipboardMessage] = useState("");
  const [progress, setProgress] = useState<{ done: number; total: number } | null>(null);
  const busyRef = useRef(false);
  const closedRef = useRef(false);
  const previewRef = useRef(false);
  const readingRef = useRef(false);
  const previews = useRef(new Set<string>());
  const controllers = useRef(new Map<string, AbortController>());
  const abortReasons = useRef(new Map<string, "timeout" | "cancelled">());
  const inputRef = useRef<HTMLInputElement>(null);
  const maxLabel = formatUploadMaxLabel(maxBytes);

  function fileItems(files: File[]): UploadItem[] {
    return files.map((file) => {
      const invalid = file.size > maxBytes ? `图片过大，单张最大 ${maxLabel}`
        : file.type && !["image/png", "image/jpeg", "image/gif", "image/webp"].includes(file.type) ? "仅支持 PNG、JPG、GIF、WEBP" : undefined;
      const preview = invalid ? undefined : URL.createObjectURL(file);
      if (preview) previews.current.add(preview);
      return { id: crypto.randomUUID(), file, label: file.name, preview, status: invalid ? "invalid" : "pending", error: invalid };
    });
  }

  useEffect(() => {
    // StrictMode 下 effect 会「setup → cleanup → setup」重跑：必须在这里把 closedRef 复位，
    // 否则上一次 cleanup 留下的 true 会让整个会话永久拒绝接收图片。
    closedRef.current = false;
    setItems(fileItems(initialFiles));
    const currentPreviews = previews.current;
    const currentControllers = controllers.current;
    return () => {
      closedRef.current = true;
      currentControllers.forEach((controller) => controller.abort());
      currentControllers.clear();
      currentPreviews.forEach((preview) => URL.revokeObjectURL(preview));
      currentPreviews.clear();
    };
    // A dialog is a single upload session; initial files are consumed once.
  }, []);

  // 预览层是另一个 dialog，它打开期间不允许接收粘贴，否则会把图片加到一个被遮住的弹窗里。
  useEffect(() => { previewRef.current = previewItem !== null; }, [previewItem]);

  function addFiles(files: File[]) {
    if (!canQueueFiles()) return;
    setError("");
    const added = fileItems(files);
    if (!added.length) return;
    setItems((current) => [...current, ...added]);
  }

  /** 「这次会话还能不能收新图片」：弹窗打开、没有正在上传、未开预览。 */
  function canQueueFiles() {
    return canAcceptClipboardImages({
      closed: closedRef.current,
      uploading: busyRef.current,
      previewing: previewRef.current,
      reading: false,
    });
  }

  /** 入口判定：在 canQueueFiles 之上再加一条「没有别的读取正在进行」，避免并发读取。 */
  function canStartClipboardRead() {
    return canQueueFiles() && !readingRef.current;
  }

  /** 从剪贴板读取图片并入队；读取前、异步返回后各判定一次，会话已失效则直接丢弃。 */
  async function addFromClipboard() {
    if (!canStartClipboardRead()) return;
    readingRef.current = true;
    setClipboardMessage("");
    setError("");
    try {
      const result = await applyClipboardImages({ canApply: () => canQueueFiles() });
      if (result.status === "failed") setError(result.message);
      else if (result.status === "added") {
        // 复用原有 addFiles 的格式/大小校验、预览与移除流程。
        addFiles(result.files);
        setClipboardMessage(`已从剪贴板添加 ${result.files.length} 张图片`);
      }
    } finally {
      readingRef.current = false;
    }
  }

  function onPaste(event: ClipboardEvent<HTMLDialogElement>) {
    // 真实浏览器的 clipboardData.types 通常只有 "Files" 而不是 "image/png"，
    // 所以用 extractPastedImageFiles（files + items kind=file）判断有没有图片。
    const { files: pasted, preventDefault } = shouldHandlePaste(event.clipboardData, event.target);
    // 没有图片：什么都不做，文字/链接/标签粘贴保持浏览器默认行为。
    if (!pasted.length) return;
    if (!canQueueFiles()) return;
    // 有图片：只有「目标不是可编辑控件」时才拦截，避免浏览器把图片当成一次页面导航；
    // 在输入框内粘贴混合内容时不拦截，文字照常进入输入框，同时图片也进入待上传队列。
    if (preventDefault) event.preventDefault();
    setClipboardMessage("");
    // 部分浏览器把任意粘贴图片都命名为 image.png，按 MIME 校正后缀。
    const now = new Date();
    addFiles(pasted.map((file, index) => normalizeClipboardImageFile(file, index + 1, now)));
  }

  function urlItems(text: string): UploadItem[] | undefined {
    const lines = [...new Set(text.split(/\r?\n/).map((line) => line.trim()).filter(Boolean))];
    const added: UploadItem[] = [];
    for (const line of lines) {
      try {
        const url = new URL(line);
        if (!["http:", "https:"].includes(url.protocol) || url.username || url.password || line.length > 8192) throw new Error();
        added.push({ id: crypto.randomUUID(), url: url.href, label: line, status: "pending" });
      } catch { setError(`无效的图片链接：${line}`); return; }
    }
    return added;
  }

  function addUrls() {
    const added = urlItems(urls);
    if (!added) return;
    setItems((current) => [...current, ...added.filter((item) => !current.some((entry) => entry.url === item.url))]);
    setUrls("");
    setError("");
  }

  function update(id: string, changes: Partial<UploadItem>) {
    setItems((current) => current.map((item) => item.id === id ? { ...item, ...changes } : item));
  }

  async function upload(retry = false) {
    if (busyRef.current) return;
    const values = draftTags(tags);
    if (!values) { setError(IMAGE_TAG_ERROR); return; }
    const added = retry ? [] : urlItems(urls);
    if (!added) return;
    const all = [...items, ...added.filter((item) => !items.some((entry) => entry.url === item.url))];
    const queue = all.filter((item) => retry ? item.status === "error" : item.status === "pending");
    if (!queue.length) { setError("请先添加可上传的图片或图片链接"); return; }
    busyRef.current = true;
    setBusy(true);
    setStarted(true);
    setTags({ tags: values, text: "" });
    setUrls("");
    setItems(all);
    setError("");
    setProgress({ done: 0, total: queue.length });
    try {
      let next = 0;
      let done = 0;
      await Promise.all(Array.from({ length: Math.min(Math.max(1, concurrency), queue.length) }, async () => {
        while (next < queue.length) {
          const item = queue[next++];
          const controller = new AbortController();
          controllers.current.set(item.id, controller);
          const timeoutId = window.setTimeout(() => {
            abortReasons.current.set(item.id, "timeout");
            controller.abort();
          }, UPLOAD_TIMEOUT_MS);
          update(item.id, { status: "uploading", error: undefined });
          try {
            if (item.file && item.file.size > maxBytes) throw new Error(`图片过大，单张最大 ${maxLabel}`);
            let response: Response;
            if (item.file) {
              const form = new FormData();
              form.append("file", item.file);
              form.append("tags", JSON.stringify(values));
              response = await fetch("/api/images", { method: "POST", body: form, signal: controller.signal });
            } else {
              response = await fetch("/api/images/from-url", {
                method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify({ url: item.url, tags: values }), signal: controller.signal,
              });
            }
            const data = await response.json().catch(() => ({}));
            if (!response.ok) throw new Error(data?.error?.message || (response.status === 413 ? `图片过大，单张最大 ${maxLabel}` : "上传失败"));
            update(item.id, { status: "success" });
          } catch (error) {
            const reason = abortReasons.current.get(item.id);
            const message = reason === "timeout"
              ? `上传超时（${UPLOAD_TIMEOUT_MS / 1000} 秒），请重试`
              : reason === "cancelled"
                ? "已取消上传，刷新列表可确认结果"
                : error instanceof Error && error.name === "AbortError"
                  ? "上传已中止"
                  : error instanceof Error ? error.message : "网络异常，上传失败";
            update(item.id, { status: "error", error: message });
          } finally {
            window.clearTimeout(timeoutId);
            controllers.current.delete(item.id);
            abortReasons.current.delete(item.id);
            setProgress({ done: ++done, total: queue.length });
          }
        }
      }));
      await refresh();
    } finally {
      busyRef.current = false;
      setBusy(false);
      setProgress(null);
    }
  }

  function cancelUpload(id: string) {
    const controller = controllers.current.get(id);
    if (!controller) return;
    abortReasons.current.set(id, "cancelled");
    controller.abort();
  }

  async function refresh() {
    try { await onUploaded(); setRefreshFailed(false); } catch { setRefreshFailed(true); }
  }

  const succeeded = items.filter((item) => item.status === "success").length;
  const failed = items.filter((item) => item.status === "error").length;
  const invalid = items.filter((item) => item.status === "invalid").length;
  const pending = items.filter((item) => item.status === "pending").length;
  const quotaFull = Boolean(usage && usage.quotaBytes > 0 && usage.usedBytes >= usage.quotaBytes);
  return (
    <ImageModal title="上传图片" busy={busy} onClose={onClose} onPaste={onPaste}>
      <div className="image-dialog-body">
        <div className="image-upload-drop" onDragOver={(event) => event.preventDefault()}
          onDrop={(event) => { event.preventDefault(); addFiles(Array.from(event.dataTransfer.files)); }}>
          <button type="button" className="image-button" disabled={busy} onClick={() => inputRef.current?.click()}>选择本地图片</button>
          <button type="button" className="image-button" disabled={busy} onClick={() => void addFromClipboard()}>从剪贴板添加</button>
          <span>或拖入多张图片 / 粘贴（Ctrl+V）</span>
          <input ref={inputRef} type="file" multiple accept="image/png,image/jpeg,image/gif,image/webp" hidden disabled={busy}
            onChange={(event) => { addFiles(Array.from(event.target.files ?? [])); event.target.value = ""; }} />
          <p className="image-help">PNG、JPG、GIF、WEBP，单张最大 {maxLabel}。可直接在弹窗内粘贴（Ctrl+V / ⌘V），添加后点击“开始上传”。</p>
          {usage && usage.quotaBytes > 0 && (
            <p className={`image-upload-quota${quotaFull ? " is-full" : ""}`} role="status">
              账号存储：已用 {formatBytes(usage.usedBytes)} / {formatBytes(usage.quotaBytes)}
              {quotaFull ? "。空间已满，请先彻底删除回收站中的图片。" : ""}
            </p>
          )}
        </div>
        <label className="image-field">图片链接（一行一个）
          <textarea value={urls} rows={3} disabled={busy} onChange={(event) => { setUrls(event.target.value); setError(""); }}
            placeholder="https://example.com/image.jpg" />
        </label>
        <button type="button" className="image-button" disabled={busy || !urls.trim()} onClick={addUrls}>添加链接到列表</button>
        <TagInput label="本次全部图片的标签" value={tags} onChange={(next) => { setTags(next); setError(""); }} suggestions={suggestions} disabled={busy} />
        <div className="image-upload-summary" role="status" aria-live="polite">共 {items.length} 项 · 成功 {succeeded} · 失败 {failed}{invalid > 0 ? ` · 无效 ${invalid}` : ""}{busy && progress ? ` · 进度 ${progress.done}/${progress.total}` : ""}</div>
        <ul className="image-upload-list">
          {items.map((item) => <li key={item.id}>
            {item.preview ? <button type="button" className="image-upload-preview" aria-label={`预览 ${item.label}`}
              onClick={() => setPreviewItem({ src: item.preview!, label: item.label })}>
              <img src={item.preview} alt="" />
            </button> : <span className="image-upload-link-icon" aria-hidden="true">↗</span>}
            <div><span className="image-upload-name" title={item.label}>{item.label}</span>
              <span className={`image-upload-status ${item.status}`}>
                {item.status === "pending" ? "待上传" : item.status === "uploading" ? "上传中…" : item.status === "success" ? "上传成功" : item.status === "invalid" ? `已跳过：${item.error ?? "文件无效"}` : item.error}
              </span>
            </div>
            {item.status === "uploading" && <button type="button" className="image-button" onClick={() => cancelUpload(item.id)}>取消</button>}
            {!busy && (item.status === "invalid" || item.status === "pending") && <button type="button" className="image-button" aria-label={`移除 ${item.label}`} onClick={() => {
              if (item.preview) { URL.revokeObjectURL(item.preview); previews.current.delete(item.preview); }
              setItems((current) => current.filter((entry) => entry.id !== item.id));
            }}>×</button>}
          </li>)}
        </ul>
        {refreshFailed && <p className="image-notice error">上传结果已保留，图片列表刷新失败。<button type="button" className="image-button" disabled={busy} onClick={() => void refresh()}>重试刷新列表</button></p>}
        {error && <p role="alert" className="image-notice error">{error}</p>}
        {clipboardMessage && <p role="status" className="image-notice">{clipboardMessage}</p>}
      </div>
      <footer className="image-dialog-footer">
        <button type="button" className="image-button" disabled={busy} onClick={onClose}>{started ? "完成" : "取消"}</button>
        {started ? (
          <>
            {pending > 0 && <button type="button" className="image-button primary" disabled={busy || quotaFull} onClick={() => void upload()}>上传新增（{pending}）</button>}
            {failed > 0 && <button type="button" className="image-button" disabled={busy || quotaFull} onClick={() => void upload(true)}>仅重试失败项（{failed}）</button>}
          </>
        ) : (
          <button type="button" className="image-button primary" disabled={busy || quotaFull || (!items.some((item) => item.status === "pending") && !urls.trim())} onClick={() => void upload()}>开始上传</button>
        )}
      </footer>
      {previewItem && <ImagePreviewDialog src={previewItem.src} label={previewItem.label} onClose={() => setPreviewItem(null)} />}
    </ImageModal>
  );
}
