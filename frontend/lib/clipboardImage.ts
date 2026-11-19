// 上传弹窗的剪贴板图片读取工具：纯函数 + 可注入依赖，方便在 node 回归脚本里直接断言。
// 这里只负责「把剪贴板里的图片变成带名字的 File」，格式/大小校验仍然由上传弹窗复用原有逻辑。

export const CLIPBOARD_IMAGE_MIME_TYPES = ["image/png", "image/jpeg", "image/gif", "image/webp"] as const;

export type ClipboardImageMimeType = (typeof CLIPBOARD_IMAGE_MIME_TYPES)[number];

const EXTENSIONS: Record<ClipboardImageMimeType, string> = {
  "image/png": "png",
  "image/jpeg": "jpg",
  "image/gif": "gif",
  "image/webp": "webp",
};

export function isClipboardImageMimeType(type: string): type is ClipboardImageMimeType {
  return (CLIPBOARD_IMAGE_MIME_TYPES as readonly string[]).includes(type);
}

/** 未知图片类型（如 image/bmp）按子类型给后缀，避免出现 “.png” 与实际内容不符的名字。 */
function extensionFor(mimeType: string): string {
  if (isClipboardImageMimeType(mimeType)) return EXTENSIONS[mimeType];
  const subtype = (mimeType.split("/")[1] ?? "").split("+")[0].trim().toLowerCase();
  return /^[a-z0-9]{1,10}$/.test(subtype) ? subtype : "bin";
}

function pad(value: number, width = 2) {
  return String(value).padStart(width, "0");
}

/**
 * 剪贴板图片没有文件名，按 MIME 生成 `clipboard-YYYYMMDD-HHMMSSmmm-NNN.<ext>`。
 * 名字里带毫秒与序号，避免连续两次粘贴产生同名条目。
 */
export function clipboardImageFileName(mimeType: string, index: number, now: Date): string {
  const stamp = [
    now.getFullYear(),
    pad(now.getMonth() + 1),
    pad(now.getDate()),
    "-",
    pad(now.getHours()),
    pad(now.getMinutes()),
    pad(now.getSeconds()),
    pad(now.getMilliseconds(), 3),
  ].join("");
  return `clipboard-${stamp}-${pad(Math.max(1, Math.trunc(index)), 3)}.${extensionFor(mimeType)}`;
}

/** 文件名后缀与 MIME 是否一致（`image/jpeg` 同时接受 .jpg 与 .jpeg）。 */
export function nameMatchesMime(name: string, mimeType: string): boolean {
  if (!name.includes(".")) return false;
  const extension = (name.split(".").pop() ?? "").toLowerCase();
  if (!extension) return false;
  if (mimeType === "image/jpeg") return extension === "jpg" || extension === "jpeg";
  return extension === extensionFor(mimeType);
}

/** 浏览器给粘贴图片套的占位名，如 `image.png` / `blob`，重复粘贴时会撞名。 */
function isGenericClipboardFileName(name: string): boolean {
  const base = (name.replace(/\.[^.]*$/, "") || name).trim().toLowerCase();
  return !base || ["image", "blob", "clipboard", "pasted-image", "untitled"].includes(base);
}

/**
 * 规范化剪贴板文件：某些浏览器把任意剪贴板图片都叫 `image.png`。
 * 名字后缀与 MIME 一致、且不是占位名时保留原文件名；否则按 MIME 重新命名，
 * 既避免「叫 .png 实际是 JPEG」，也避免连续粘贴出现一串同名条目。
 */
export function normalizeClipboardImageFile(file: File, index: number, now: Date): File {
  if (!file.type) return file;
  if (nameMatchesMime(file.name, file.type) && !isGenericClipboardFileName(file.name)) return file;
  return new File([file], clipboardImageFileName(file.type, index, now), { type: file.type, lastModified: file.lastModified });
}

export type ClipboardReadReason = "unsupported" | "insecure" | "denied" | "empty" | "unsupported-format" | "read-failed";
export const CLIPBOARD_READ_MESSAGES: Record<ClipboardReadReason, string> = {
  unsupported: "当前浏览器不支持读取剪贴板图片，请改用“选择本地图片”。",
  insecure: "浏览器仅在 HTTPS 或 localhost 下允许读取剪贴板图片，请改用“选择本地图片”。",
  denied: "读取剪贴板的权限被拒绝，请在浏览器允许后重试，或改用“选择本地图片”。",
  empty: "剪贴板里没有图片（可能复制的是文字或链接），请先复制图片再试。",
  "unsupported-format": "剪贴板里的图片格式不受支持，仅支持 PNG、JPG、GIF、WEBP。",
  "read-failed": "读取剪贴板失败，请重试或改用“选择本地图片”。",
};

export type ClipboardImageReadResult =
  | { ok: true; files: File[] }
  | { ok: false; reason: ClipboardReadReason; message: string };

/** 与浏览器 ClipboardItem 保持结构兼容，测试可以注入假实现。 */
export type ClipboardItemLike = {
  readonly types: readonly string[];
  getType: (type: string) => Promise<Blob>;
};

export type ReadClipboardOptions = {
  /** 注入读取实现（测试用）；缺省走 navigator.clipboard.read()。 */
  read?: () => Promise<readonly ClipboardItemLike[]>;
  /** 注入时钟（测试用文件名确定性）。 */
  now?: () => Date;
};

export type ClipboardAcceptState = {
  /** 上传弹窗已关闭 */
  closed: boolean;
  /** 上传进行中 */
  uploading: boolean;
  /** 预览层已打开 */
  previewing: boolean;
  /** 已有一次读取在进行 */
  reading: boolean;
};

/**
 * 单一判定入口：弹窗打开、没有正在上传、未开预览、没有并发读取时才接收剪贴板图片。
 *
 * 注意：**不**按「已经开始过上传」拒绝。一批传完后用户常常还想补一张，
 * 此时允许继续入队，由上传进行中（uploading）负责阻止批次内的并发变更。
 */
export function canAcceptClipboardImages(state: ClipboardAcceptState): boolean {
  return !state.closed && !state.uploading && !state.previewing && !state.reading;
}

function failure(reason: ClipboardReadReason): ClipboardImageReadResult {
  return { ok: false, reason, message: CLIPBOARD_READ_MESSAGES[reason] };
}

/**
 * 从 paste 事件里取出图片文件。
 *
 * 关键：真实浏览器的 `clipboardData.types` 往往只有 `Files`，**不是** `image/png`，
 * 所以不能用 types 判断“这次粘贴有没有图片”；这里同时看 `files` 与 `items`（kind=file）。
 * 返回空数组意味着“这次粘贴没有图片”——调用方必须什么都不做，让文字/链接/标签粘贴保持默认行为。
 */
export type PasteDataLike = {
  files?: ArrayLike<File> | null;
  items?: ArrayLike<{ kind?: string; type?: string; getAsFile: () => File | null }> | null;
};

export function extractPastedImageFiles(data: PasteDataLike | null | undefined): File[] {
  if (!data) return [];
  const files = Array.from(data.files ?? []).filter((file) => file.type.startsWith("image/"));
  if (files.length) return files;
  const fromItems: File[] = [];
  for (const item of Array.from(data.items ?? [])) {
    if (item.kind !== "file" || !item.type?.startsWith("image/")) continue;
    const file = item.getAsFile();
    if (file) fromItems.push(file);
  }
  return fromItems;
}

/** 当前焦点是否在可编辑控件里：在那里粘贴文字必须保留默认行为。 */
export function isEditablePasteTarget(target: unknown): boolean {
  const element = target as { tagName?: string; isContentEditable?: boolean; closest?: (selector: string) => unknown } | null;
  if (!element?.tagName) return false;
  const tag = element.tagName.toUpperCase();
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return true;
  if (element.isContentEditable) return true;
  return typeof element.closest === "function" && !!element.closest("input, textarea, [contenteditable='true']");
}

/**
 * 是否应该拦截这次粘贴（`preventDefault`）。
 *
 * - 没有图片：不拦截（文字/链接/标签照常粘贴）。
 * - 有图片且粘贴目标可编辑（标签输入框、链接输入框）：不拦截，混合粘贴里的文字照常进入输入框。
 * - 有图片且目标不可编辑：拦截，避免浏览器把图片当成一次页面导航。
 */
export function shouldHandlePaste(data: PasteDataLike | null | undefined, target: unknown): { files: File[]; preventDefault: boolean } {
  const files = extractPastedImageFiles(data);
  if (!files.length) return { files, preventDefault: false };
  if (isEditablePasteTarget(target)) return { files, preventDefault: false };
  return { files, preventDefault: true };
}

function classifyReadError(error: unknown): ClipboardReadReason {
  const name = error instanceof Error ? error.name : "";
  // 浏览器在用户拒绝、策略禁止或非安全上下文下分别抛 NotAllowedError / SecurityError。
  if (name === "NotAllowedError" || name === "SecurityError") return "denied";
  // 部分浏览器在剪贴板为空时抛 NotFoundError，而不是返回空数组。
  if (name === "NotFoundError") return "empty";
  return "read-failed";
}

function resolveReader(read: ReadClipboardOptions["read"]): (() => Promise<readonly ClipboardItemLike[]>) | ClipboardReadReason {
  if (read) return read;
  if (typeof window === "undefined" || typeof navigator === "undefined") return "unsupported";
  if (!window.isSecureContext) return "insecure";
  if (typeof navigator.clipboard?.read !== "function") return "unsupported";
  return () => navigator.clipboard.read();
}

/**
 * 读取剪贴板中的图片。只创建 File 对象，**不创建 object URL**：
 * 这样调用方在异步返回后发现弹窗已关闭/已开始上传/已开预览时直接丢弃结果即可，不会泄漏。
 */
export async function readClipboardImages(options: ReadClipboardOptions = {}): Promise<ClipboardImageReadResult> {
  const reader = resolveReader(options.read);
  if (typeof reader === "string") return failure(reader);

  let items: readonly ClipboardItemLike[];
  try {
    items = await reader();
  } catch (error) {
    return failure(classifyReadError(error));
  }

  if (!items?.length) return failure("empty");

  const picked: { item: ClipboardItemLike; type: string }[] = [];
  for (const item of items) {
    const type = (item.types ?? []).find((entry) => entry.startsWith("image/"));
    if (type) picked.push({ item, type });
  }
  if (!picked.length) return failure("unsupported-format");

  const now = (options.now ?? (() => new Date()))();
  const files: File[] = [];
  for (const { item, type } of picked) {
    let blob: Blob;
    try {
      blob = await item.getType(type);
    } catch { continue; } // 单张读不出来不应该拖垮整批
    if (!blob || blob.size === 0) continue;
    const mimeType = isClipboardImageMimeType(blob.type) ? blob.type : type;
    const raw = new File([blob], clipboardImageFileName(mimeType, files.length + 1, now), { type: mimeType });
    files.push(normalizeClipboardImageFile(raw, files.length + 1, now));
  }
  if (!files.length) return failure("empty");
  return { ok: true, files };
}

export type ApplyClipboardResult =
  | { status: "added"; files: File[] }
  | { status: "cancelled" }
  | { status: "failed"; reason: ClipboardReadReason; message: string };

export type ApplyClipboardOptions = ReadClipboardOptions & {
  /** 读取前与异步返回后各判定一次；返回 false 表示这次结果必须丢弃（不入队、不创建 object URL）。 */
  canApply: () => boolean;
};

/**
 * 「读取并在仍然有效时交回图片」的完整流程。异步返回后必须重新判定 canApply()：
 * 弹窗关闭、已经开始上传或已经打开预览时直接丢弃结果。
 */
export async function applyClipboardImages(options: ApplyClipboardOptions): Promise<ApplyClipboardResult> {
  if (!options.canApply()) return { status: "cancelled" };
  const result = await readClipboardImages({ read: options.read, now: options.now });
  if (!result.ok) return { status: "failed", reason: result.reason, message: result.message };
  if (!options.canApply()) return { status: "cancelled" };
  return { status: "added", files: result.files };
}
