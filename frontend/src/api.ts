/**
 * Runtime UI configuration (GET /api/config).
 *
 * Both values are operator settings in the Go deployment, so the page reads
 * them at load time instead of relying on values baked into the bundle:
 *   - uploadMaxBytes: UPLOAD_MAX_BYTES (the upload ceiling);
 *   - mailEnabled: whether SMTP + BASEURL are configured, i.e. whether
 *     "forgot password" and "change email" can actually deliver mail. The
 *     server only exposes this capability boolean, never SMTP host/account.
 * Missing, non-2xx or malformed answers are errors: the dashboard must not
 * silently fall back to the build-time 5MiB default, and mail entries must not
 * guess that mail works.
 */
export type RuntimeConfigResult =
  | { state: "ok"; uploadMaxBytes: number; mailEnabled: boolean }
  | { state: "error"; message: string };

export async function fetchRuntimeConfig(): Promise<RuntimeConfigResult> {
  let response: Response;
  try {
    response = await fetch("/api/config", {
      credentials: "same-origin",
      cache: "no-store",
      headers: { accept: "application/json" },
    });
  } catch {
    return { state: "error", message: "网络异常，无法读取上传限制" };
  }

  const body = (await response.json().catch(() => null)) as Record<string, unknown> | null;
  if (!response.ok) {
    const message =
      typeof body?.message === "string" && body.message !== ""
        ? body.message
        : `服务端返回 HTTP ${response.status}`;
    return { state: "error", message };
  }

  const rawValue = body?.uploadMaxBytes;
  if (
    typeof rawValue !== "number" ||
    !Number.isFinite(rawValue) ||
    rawValue <= 0 ||
    Math.floor(rawValue) !== rawValue
  ) {
    return { state: "error", message: "上传限制配置无效" };
  }

  return { state: "ok", uploadMaxBytes: rawValue, mailEnabled: body?.mailEnabled === true };
}

/**
 * Session helpers for the static bundle.
 *
 * The Go service exposes /api/auth/me as `{user}` on 200 and a top-level
 * `{code,message}` on failure (see MIGRATION.md). "Anonymous" and
 * "account disabled" are different states: the dashboard shows the login card
 * for the first and the disabled notice for the second.
 */

export type ApiUserRole = "USER" | "ADMIN";

export type ApiUser = {
  id: number;
  email: string | null;
  username: string;
  role: ApiUserRole;
  mustChangePassword: boolean;
};

export type CurrentUserResult =
  | { state: "authenticated"; user: ApiUser }
  | { state: "anonymous" }
  | { state: "disabled" }
  | { state: "error"; message: string };

function normalizeUser(raw: unknown): ApiUser | null {
  if (typeof raw !== "object" || raw === null) return null;
  const record = raw as Record<string, unknown>;
  if (typeof record.id !== "number" || typeof record.username !== "string") return null;
  const role = record.role === "ADMIN" ? "ADMIN" : record.role === "USER" ? "USER" : null;
  if (!role) return null;
  return {
    id: record.id,
    email: typeof record.email === "string" && record.email !== "" ? record.email : null,
    username: record.username,
    role,
    mustChangePassword: record.mustChangePassword === true,
  };
}

export async function fetchCurrentUser(): Promise<CurrentUserResult> {
  let response: Response;
  try {
    response = await fetch("/api/auth/me", {
      credentials: "same-origin",
      cache: "no-store",
      headers: { accept: "application/json" },
    });
  } catch {
    return { state: "error", message: "网络异常，无法确认登录状态" };
  }

  const body = (await response.json().catch(() => null)) as Record<string, unknown> | null;

  if (response.ok && body) {
    const user = normalizeUser(body.user);
    if (user) return { state: "authenticated", user };
    return { state: "error", message: "登录状态响应格式不正确" };
  }
  if (response.status === 401) return { state: "anonymous" };
  if (response.status === 403 && body?.code === "ACCOUNT_DISABLED") return { state: "disabled" };
  if (response.status === 403) return { state: "anonymous" };

  const message =
    typeof body?.message === "string" ? body.message : `无法确认登录状态（HTTP ${response.status}）`;
  return { state: "error", message };
}

/**
 * Image detail metadata, from GET /api/images/{id}/meta.
 *
 * The original detail page was server-rendered from the database. Under the Go
 * binary the page is a static shell, so this route is the only source of the
 * name, tags, dimensions, timestamps and visibility flag.
 *
 * The Go handler decides visibility with the original rules: missing,
 * soft-deleted or admin-removed images answer 404, and a hidden image answers
 * 404 to everyone except its uploader. A 404 is therefore authoritative and
 * must end at the not-found page: the bytes endpoint is not a fallback, because
 * it serves a placeholder for hidden images and must not turn a forbidden
 * detail page into a successful-looking one.
 */
export type ImageMetadata = {
  id: string;
  originalName: string;
  originalMimeType: string;
  size: number;
  width: number | null;
  height: number | null;
  createdAt: string;
  isVisible: boolean;
  tags: string[];
  url?: string;
  pageUrl?: string;
  thumbUrl?: string;
};

function normalizeMetadata(raw: unknown): ImageMetadata | null {
  if (typeof raw !== "object" || raw === null) return null;
  const record = raw as Record<string, unknown>;
  if (typeof record.id !== "string" || typeof record.originalName !== "string") return null;
  const tags = Array.isArray(record.tags)
    ? record.tags.filter((tag): tag is string => typeof tag === "string")
    : [];
  return {
    id: record.id,
    originalName: record.originalName,
    originalMimeType: typeof record.originalMimeType === "string" ? record.originalMimeType : "",
    size: typeof record.size === "number" ? record.size : 0,
    width: typeof record.width === "number" ? record.width : null,
    height: typeof record.height === "number" ? record.height : null,
    createdAt: typeof record.createdAt === "string" ? record.createdAt : "",
    isVisible: record.isVisible !== false,
    tags,
    url: typeof record.url === "string" ? record.url : undefined,
    pageUrl: typeof record.pageUrl === "string" ? record.pageUrl : undefined,
    thumbUrl: typeof record.thumbUrl === "string" ? record.thumbUrl : undefined,
  };
}

export type ImageDetailResult =
  | { state: "found"; metadata: ImageMetadata }
  | { state: "missing" }
  | { state: "error"; message: string };

function detailErrorMessage(status: number, body: Record<string, unknown> | null): string {
  const nested = body?.error as Record<string, unknown> | undefined;
  if (typeof nested?.message === "string" && nested.message !== "") return nested.message;
  if (typeof body?.message === "string" && body.message !== "") return body.message;
  return `加载图片信息失败（HTTP ${status}）`;
}

export async function fetchImageDetail(id: string): Promise<ImageDetailResult> {
  const encoded = encodeURIComponent(id);

  let metaResponse: Response;
  try {
    metaResponse = await fetch(`/api/images/${encoded}/meta`, {
      credentials: "same-origin",
      cache: "no-store",
      headers: { accept: "application/json" },
    });
  } catch {
    return { state: "error", message: "网络异常，无法加载图片信息" };
  }

  const body = (await metaResponse.json().catch(() => null)) as Record<string, unknown> | null;
  if (metaResponse.status === 404) return { state: "missing" };
  if (!metaResponse.ok) {
    return { state: "error", message: detailErrorMessage(metaResponse.status, body) };
  }

  const metadata = normalizeMetadata(body?.image ?? null);
  if (!metadata) return { state: "error", message: "图片信息响应格式不正确" };

  // The record is visible, so its original file should be readable. A vanished
  // file must fail loudly instead of rendering a detail page with a broken
  // image; 404 still means the image is gone.
  let headResponse: Response;
  try {
    headResponse = await fetch(`/api/images/${encoded}?variant=original`, {
      method: "HEAD",
      credentials: "same-origin",
      cache: "no-store",
    });
  } catch {
    return { state: "error", message: "网络异常，无法读取原图" };
  }
  if (headResponse.status === 404) return { state: "missing" };
  if (!headResponse.ok) {
    return { state: "error", message: `原图不可读取（HTTP ${headResponse.status}）` };
  }

  return { state: "found", metadata };
}
