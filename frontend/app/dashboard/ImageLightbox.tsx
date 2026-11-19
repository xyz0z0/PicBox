"use client";

import { useEffect, useId, useRef, useState } from "react";

export type LightboxImage = {
  id: string;
  originalName: string;
  url: string;
  pageUrl?: string;
  width?: number | null;
  height?: number | null;
};

export default function ImageLightbox({
  items,
  initialId,
  trashed,
  onClose,
}: {
  items: LightboxImage[];
  initialId: string;
  trashed: boolean;
  onClose: () => void;
}) {
  const [currentId, setCurrentId] = useState(initialId);
  const [zoomed, setZoomed] = useState(false);
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();

  const index = items.findIndex((item) => item.id === currentId);
  const image = index >= 0 ? items[index] : items[0];

  useEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    const previous = document.activeElement;
    dialog.showModal();
    return () => {
      dialog.close();
      if (previous instanceof HTMLElement && previous.isConnected) {
        previous.focus();
      }
    };
  }, []);

  useEffect(() => {
    setZoomed(false);
  }, [currentId]);

  if (!image) return null;

  function move(delta: number) {
    if (items.length < 2) return;
    const base = index >= 0 ? index : 0;
    const next = (base + delta + items.length) % items.length;
    setCurrentId(items[next].id);
  }

  const detailHref = image.pageUrl || image.url;
  const position = index >= 0 ? index + 1 : 1;

  return (
    <dialog
      ref={ref}
      className="image-dialog image-lightbox"
      aria-labelledby={titleId}
      onCancel={(event) => {
        event.preventDefault();
        onClose();
      }}
      onKeyDown={(event) => {
        if (event.key === "ArrowLeft") {
          event.preventDefault();
          move(-1);
        } else if (event.key === "ArrowRight") {
          event.preventDefault();
          move(1);
        }
      }}
    >
      <header className="image-dialog-header">
        <h2 id={titleId} className="image-lightbox-title" title={image.originalName}>
          {image.originalName}
        </h2>
        <span className="image-lightbox-count">
          {position} / {items.length}
        </span>
        <button
          type="button"
          className="image-button"
          aria-label="关闭预览"
          onClick={onClose}
        >
          ×
        </button>
      </header>

      <div
        className={`image-lightbox-stage${zoomed ? " zoomed" : ""}`}
        onClick={() => setZoomed((value) => !value)}
      >
        <img src={image.url} alt={image.originalName} decoding="async" />
      </div>

      <footer className="image-dialog-footer image-lightbox-footer">
        <button
          type="button"
          className="image-button"
          aria-label="上一张"
          disabled={items.length < 2}
          onClick={() => move(-1)}
        >
          ← 上一张
        </button>
        <button
          type="button"
          className="image-button"
          aria-label="下一张"
          disabled={items.length < 2}
          onClick={() => move(1)}
        >
          下一张 →
        </button>
        <button
          type="button"
          className="image-button"
          onClick={() => setZoomed((value) => !value)}
        >
          {zoomed ? "适应窗口" : "原始大小"}
        </button>
        {!trashed && (
          <a
            className="image-button"
            href={detailHref}
            target="_blank"
            rel="noreferrer"
          >
            打开详情页
          </a>
        )}
        <a
          className="image-button"
          href={image.url}
          download={image.originalName}
        >
          下载原图
        </a>
      </footer>
    </dialog>
  );
}
