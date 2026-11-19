import { useEffect, useState } from "react";
import CopyButtons from "../../app/images/[id]/CopyButtons";
import { fetchImageDetail, type ImageDetailResult } from "../api";
import { NotFoundPage } from "./not-found-page";

/**
 * Static port of the original Next.js image detail page.
 *
 * The original page was server-rendered from the database. This bundle keeps the
 * exact markup and classes and reads the same facts from GET /api/images/{id}/meta,
 * which is required: a 404 there is the page's notFound(), including the
 * visibility rules (hidden images are 404 for everyone but their uploader,
 * soft-deleted and admin-removed images are 404 for everyone). Any other failure
 * is shown as an explicit error instead of being rendered as a successful page.
 */

const MIME_LABELS: Record<string, string> = {
  "image/jpeg": "JPEG",
  "image/png": "PNG",
  "image/gif": "GIF",
  "image/webp": "WebP",
};

function formatSize(bytes: number) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`;
}

function formatUploadedAt(iso: string) {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleString("zh-CN", {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function ImagePage({ id }: { id: string }) {
  const [result, setResult] = useState<ImageDetailResult | null>(null);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let active = true;
    setResult(null);
    void fetchImageDetail(id).then((value) => {
      if (active) setResult(value);
    });
    return () => {
      active = false;
    };
  }, [id, attempt]);

  if (!result) {
    return (
      <div className="ip-root">
        <header className="ip-header">
          <a href="/" className="ip-logo">PicBox</a>
          <a href="/dashboard" className="ip-back">← 返回工作台</a>
        </header>
        <main className="ip-main">
          <p role="status" style={{ color: "var(--ph-muted)" }}>正在加载原图…</p>
        </main>
      </div>
    );
  }

  if (result.state === "missing") {
    return <NotFoundPage />;
  }

  if (result.state === "error") {
    return (
      <div className="ip-root">
        <header className="ip-header">
          <a href="/" className="ip-logo">PicBox</a>
          <a href="/dashboard" className="ip-back">← 返回工作台</a>
        </header>
        <main className="ip-main">
          <p role="alert" style={{ color: "var(--ph-danger)" }}>{result.message}</p>
          <p>
            <button
              type="button"
              className="copy-btn outline"
              onClick={() => setAttempt((current) => current + 1)}
            >
              重试
            </button>
          </p>
        </main>
      </div>
    );
  }

  const { metadata } = result;
  // The metadata route returns the same url/pageUrl/thumbUrl shape as the list
  // API (absolute when BASEURL is configured).
  const originalUrl =
    metadata.url ?? `/api/images/${encodeURIComponent(id)}?variant=original`;
  const originalName = metadata.originalName;

  const uploadedAt = formatUploadedAt(metadata.createdAt);
  const tags = [...metadata.tags].sort();

  const mimeLabel = metadata.originalMimeType
    ? MIME_LABELS[metadata.originalMimeType] ??
      metadata.originalMimeType.replace(/^image\//, "").toUpperCase()
    : "";

  return (
    <div className="ip-root">
      <header className="ip-header">
        <a href="/" className="ip-logo">PicBox</a>
        <a href="/dashboard" className="ip-back">← 返回工作台</a>
      </header>

      <main className="ip-main">
        <div className="ip-card">
          <div className="ip-image-wrap">
            <a href={originalUrl} target="_blank" rel="noreferrer">
              {/* 详情页展示原文件本身：照片保真，不做缩略图降采样。缩略图只用于列表卡片。 */}
              <img src={originalUrl} alt={originalName} className="ip-image" />
            </a>
          </div>

          <div className="ip-info">
            <h1 className="ip-filename" title={originalName}>
              {originalName}
            </h1>

            {!metadata.isVisible && (
              <p className="ip-hidden-note">
                这张图片当前不可见，只有你本人能看到此页面；其他人打开会得到 404。
              </p>
            )}

            <div className="ip-meta">
              {metadata.width && metadata.height && (
                <span className="ip-tag">{metadata.width} × {metadata.height}</span>
              )}
              {metadata.size > 0 && <span className="ip-tag">{formatSize(metadata.size)}</span>}
              {mimeLabel && <span className="ip-tag">{mimeLabel}</span>}
              {uploadedAt && <span className="ip-tag">{uploadedAt}</span>}
            </div>

            {tags.length > 0 && (
              <div className="ip-meta ip-tags">
                {tags.map((tag) => (
                  <span className="ip-tag" key={tag}>#{tag}</span>
                ))}
              </div>
            )}

            <CopyButtons
              imageUrl={originalUrl}
              originalUrl={originalUrl}
              originalName={originalName}
              downloadName={originalName}
            />
          </div>
        </div>
      </main>
    </div>
  );
}
