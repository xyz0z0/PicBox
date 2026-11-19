"use client";

import { useState } from "react";
import { writeClipboardText } from "@/lib/clipboardText";

interface CopyButtonsProps {
  imageUrl: string;
  originalUrl: string;
  originalName: string;
  downloadName: string;
}

function useCopy() {
  const [copied, setCopied] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const copy = async (text: string, key: string) => {
    const ok = await writeClipboardText(text);
    if (!ok) {
      setError("复制失败，请手动选中复制");
      return;
    }
    setError(null);
    setCopied(key);
    setTimeout(() => setCopied(null), 2000);
  };
  return { copied, error, copy };
}

export default function CopyButtons({ imageUrl, originalUrl, originalName, downloadName }: CopyButtonsProps) {
  const { copied, error, copy } = useCopy();

  const markdown = `![${originalName}](${imageUrl})`;

  return (
    <>
      <div className="copy-actions">
        <button
          className={`copy-btn${copied === "url" ? " copied" : ""}`}
          onClick={() => copy(imageUrl, "url")}
        >
          {copied === "url" ? "✓ 已复制" : "复制图片链接"}
        </button>
        <button
          className={`copy-btn secondary${copied === "md" ? " copied" : ""}`}
          onClick={() => copy(markdown, "md")}
        >
          {copied === "md" ? "✓ 已复制" : "复制 Markdown"}
        </button>
        <a className="copy-btn outline" href={originalUrl} target="_blank" rel="noreferrer">
          查看原图
        </a>
        <a className="copy-btn outline" href={originalUrl} download={downloadName}>
          下载原图
        </a>
      </div>
      <p
        className="copy-feedback"
        role={error ? "alert" : "status"}
        aria-live="polite"
      >
        {error ?? ""}
      </p>
    </>
  );
}
