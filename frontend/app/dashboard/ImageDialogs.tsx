"use client";

import { useEffect, useId, useRef, useState, type ClipboardEvent, type ReactNode } from "react";
import { IMAGE_TAG_ERROR, parseImageTags, type TagMode } from "@/lib/imageTag";

export type TagDraft = { tags: string[]; text: string };
export const emptyTagDraft = (): TagDraft => ({ tags: [], text: "" });
export function draftTags(value: TagDraft) {
  return parseImageTags([...value.tags, value.text]);
}

export function ImageModal({ title, busy, onClose, children, className, onPaste }: {
  title: string; busy: boolean; onClose: () => void; children: ReactNode; className?: string;
  // 可选：上传弹窗用它接收粘贴图片，其余对话框不传，行为不变。
  onPaste?: (event: ClipboardEvent<HTMLDialogElement>) => void;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  useEffect(() => {
    const dialog = ref.current!;
    const previous = document.activeElement;
    dialog.showModal();
    return () => {
      dialog.close();
      if (previous instanceof HTMLElement && previous.isConnected) previous.focus();
    };
  }, []);
  return (
    <dialog ref={ref} className={`image-dialog${className ? ` ${className}` : ""}`} aria-labelledby={titleId}
      onPaste={onPaste}
      onCancel={(event) => {
        if (event.target !== event.currentTarget) return;
        event.preventDefault();
        if (!busy) onClose();
      }}
      onDragOver={(event) => event.stopPropagation()} onDrop={(event) => event.stopPropagation()}>
      <header className="image-dialog-header">
        <h2 id={titleId}>{title}</h2>
        <button type="button" className="image-button" aria-label="关闭弹窗" disabled={busy} onClick={onClose}>×</button>
      </header>
      {children}
    </dialog>
  );
}

export function ImagePreviewDialog({ src, label, onClose }: {
  src: string; label: string; onClose: () => void;
}) {
  return (
    <ImageModal title={`图片预览：${label}`} busy={false} onClose={onClose} className="image-preview-dialog">
      <div className="image-preview-body"><img src={src} alt={label} /></div>
    </ImageModal>
  );
}

export function TagInput({ label, value, onChange, suggestions, disabled = false }: {
  label: string; value: TagDraft; onChange: (value: TagDraft) => void; suggestions: string[]; disabled?: boolean;
}) {
  const id = useId();
  const [error, setError] = useState("");
  function add(text = value.text) {
    const tags = parseImageTags([...value.tags, text]);
    if (!tags) { setError(IMAGE_TAG_ERROR); return; }
    onChange({ tags, text: "" });
    setError("");
  }
  return (
    <div className="image-tag-editor">
      <label htmlFor={id}>{label}</label>
      <div className="image-tag-chips">
        {value.tags.map((tag) => (
          <span className="image-tag-chip" key={tag}>
            <span>{tag}</span>
            <button type="button" aria-label={`移除标签 ${tag}`} disabled={disabled}
              onClick={() => onChange({ ...value, tags: value.tags.filter((entry) => entry !== tag) })}>×</button>
          </span>
        ))}
      </div>
      <div className="image-inline-controls">
        <input id={id} value={value.text} disabled={disabled} list={`${id}-suggestions`}
          aria-describedby={`${id}-help`} aria-invalid={!!error}
          placeholder="输入标签后按回车，可选择已有标签"
          onChange={(event) => { onChange({ ...value, text: event.target.value }); setError(""); }}
          onKeyDown={(event) => { if (event.key === "Enter" && !event.nativeEvent.isComposing) { event.preventDefault(); add(); } }} />
        <datalist id={`${id}-suggestions`}>
          {suggestions.filter((tag) => !value.tags.includes(tag)).map((tag) => <option value={tag} key={tag} />)}
        </datalist>
        <button type="button" className="image-button" disabled={disabled || !value.text.trim()} onClick={() => add()}>添加标签</button>
      </div>
      <p id={`${id}-help`} className="image-help">最多 20 个标签，每个最多 64 个字；留空可不设置标签。</p>
      {error && <p role="alert" className="image-notice error">{error}</p>}
    </div>
  );
}

export function EditTagsDialog({ ids, initialTags, suggestions, onClose, onSaved }: {
  ids: string[]; initialTags?: string[]; suggestions: string[]; onClose: () => void; onSaved: () => Promise<void>;
}) {
  const single = initialTags !== undefined;
  const [value, setValue] = useState<TagDraft>({ tags: initialTags ?? [], text: "" });
  const [mode, setMode] = useState<TagMode>(single ? "replace" : "add");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);
  const busyRef = useRef(false);
  async function save() {
    if (busyRef.current) return;
    const tags = draftTags(value);
    if (!tags) { setError(IMAGE_TAG_ERROR); return; }
    if (!single && mode !== "replace" && tags.length === 0) { setError("请至少输入一个标签"); return; }
    busyRef.current = true;
    setBusy(true);
    setError("");
    try {
      if (!saved) {
        const response = await fetch(single ? `/api/images/${ids[0]}` : "/api/images/batch", {
          method: "PATCH", headers: { "content-type": "application/json" },
          body: JSON.stringify(single ? { tags } : { ids, action: "tag", mode, tags }),
        });
        const data = await response.json().catch(() => ({}));
        if (!response.ok) throw new Error(data?.error?.message || data?.message || "标签保存失败");
        setSaved(true);
      }
      await onSaved();
      onClose();
    } catch (error) {
      setError(error instanceof Error ? error.message : "操作失败，请重试");
    } finally { setBusy(false); busyRef.current = false; }
  }
  return (
    <ImageModal title={single ? "编辑图片标签" : `编辑 ${ids.length} 张图片的标签`} busy={busy} onClose={onClose}>
      <div className="image-dialog-body">
        {!single && <label className="image-field">操作方式
          <select value={mode} disabled={busy || saved} onChange={(event) => { setMode(event.target.value as TagMode); setError(""); }}>
            <option value="add">追加标签（保留其他标签）</option>
            <option value="remove">移除指定标签（保留其他标签）</option>
            <option value="replace">替换全部标签（留空即清空）</option>
          </select>
        </label>}
        <TagInput label="图片标签" value={value} onChange={(next) => { setValue(next); setError(""); }} suggestions={suggestions} disabled={busy || saved} />
        {saved && <p role="status" className="image-notice success">标签已保存；列表刷新失败时可重试刷新。</p>}
        {error && <p role="alert" className="image-notice error">{error}</p>}
      </div>
      <footer className="image-dialog-footer">
        <button type="button" className="image-button" disabled={busy} onClick={onClose}>关闭</button>
        <button type="button" className="image-button primary" disabled={busy} onClick={() => void save()}>{busy ? "处理中…" : saved ? "重试刷新列表" : "保存标签"}</button>
      </footer>
    </ImageModal>
  );
}
