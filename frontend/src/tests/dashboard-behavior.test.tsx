import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { act, type ReactElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import DashboardClient from "../../app/dashboard/DashboardClient";
import AccountRecoveryScreen from "../../app/dashboard/AccountRecoveryScreen";
import LoginCard from "../../app/dashboard/LoginCard";
import { DashboardPage } from "../pages/dashboard-page";

// The Go static bundle maps next/navigation to a shim whose refresh() is a real
// location.reload(). Mock it here so the password-change tests can prove which
// callers refresh without jsdom trying to navigate away.
const navigation = vi.hoisted(() => ({ refresh: vi.fn() }));
vi.mock("next/navigation", () => ({
  useRouter: () => ({
    refresh: navigation.refresh,
    push: vi.fn(),
    replace: vi.fn(),
    back: vi.fn(),
    forward: vi.fn(),
    prefetch: vi.fn(),
  }),
  usePathname: () => "/dashboard",
  useSearchParams: () => new URLSearchParams(),
  redirect: vi.fn(),
  notFound: vi.fn(),
}));

/**
 * Offline behaviour tests for the Go bundle. No browser or preview server is
 * started: jsdom renders the real reused components and fetch is mocked.
 *
 * Covered here (not by string assertions on source):
 *  - runtime /api/config reaches the upload hint, including sub-MiB values;
 *  - the same config's mailEnabled flag decides whether "forgot password" and
 *    "change email" are offered, so an SMTP-less deployment points at the admin
 *    instead of walking into 503 MAIL_UNAVAILABLE;
 *  - a failed or malformed config is an explicit, retryable error;
 *  - a partially successful purge keeps failed rows visible and selected,
 *    removes only updatedIds, and reports cleanupPending/failedCount;
 *  - the static dashboard keeps the password-change success notice without a
 *    full reload, while the default (Next) component still calls refresh.
 */

// React 19's act() needs this flag to flush effects like a test renderer.
(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

type RawBody = unknown;
type Reply = { status?: number; body: RawBody } | "network";
type FetchCall = { url: string; init?: RequestInit };

function jsonResponse(body: RawBody, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => null },
    json: async () => body,
  } as unknown as Response;
}

function installFetch(handler: (url: string, init?: RequestInit) => Reply): FetchCall[] {
  const calls: FetchCall[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
      calls.push({ url, init });
      const reply = handler(url, init);
      if (reply === "network") throw new Error("network down");
      return jsonResponse(reply.body, reply.status ?? 200);
    }),
  );
  return calls;
}

const adminUser = {
  id: 1,
  email: "owner@example.com",
  username: "owner",
  role: "ADMIN" as const,
  mustChangePassword: false,
};

function imageItem(id: string, name: string, trashed: boolean) {
  return {
    id,
    originalName: name,
    tag: null,
    tags: [],
    createdAt: "2026-01-02T03:04:05.000Z",
    isVisible: true,
    deletedAt: trashed ? "2026-01-03T00:00:00.000Z" : null,
    url: `/api/images/${id}?variant=original`,
    pageUrl: `/images/${id}`,
    thumbUrl: `/api/images/${id}?variant=thumb`,
    width: 10,
    height: 10,
    size: 1234,
    originalMimeType: "image/png",
  };
}

function dashboardHandler(options: {
  uploadMaxBytes?: unknown;
  mailEnabled?: boolean;
  configStatus?: number;
  trashItems?: ReturnType<typeof imageItem>[];
  onBatch?: (body: Record<string, unknown>) => Reply;
}) {
  return (url: string, init?: RequestInit): Reply => {
    if (url === "/api/auth/me") {
      return { body: { user: adminUser } };
    }
    if (url === "/api/config") {
      const status = options.configStatus ?? 200;
      if (status !== 200) return { status, body: { message: "配置读取失败" } };
      return {
        body: {
          ok: true,
          uploadMaxBytes: options.uploadMaxBytes ?? (10 << 20),
          mailEnabled: options.mailEnabled ?? true,
        },
      };
    }
    if (url === "/api/images/tags") {
      return { body: { ok: true, tags: [] } };
    }
    if (url === "/api/images/usage") {
      return { body: { ok: true, usedBytes: 0, quotaBytes: 0 } };
    }
    if (url === "/api/images/batch") {
      const body = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
      return options.onBatch ? options.onBatch(body) : { body: { ok: true, updatedCount: 0 } };
    }
    if (url.startsWith("/api/images?")) {
      const trashed = url.includes("trashed=true");
      const items = trashed ? options.trashItems ?? [] : [];
      return { body: { ok: true, items, nextCursor: null, total: items.length } };
    }
    return { status: 404, body: { message: `unexpected fetch: ${url}` } };
  };
}

let container: HTMLDivElement;
let root: Root;

beforeAll(() => {
  // jsdom has no modal dialog implementation; the reused AppDialog calls these.
  const prototype = window.HTMLDialogElement?.prototype;
  if (prototype) {
    prototype.showModal = function showModal(this: HTMLDialogElement) {
      this.setAttribute("open", "");
    };
    prototype.close = function close(this: HTMLDialogElement) {
      this.removeAttribute("open");
    };
  }
  // jsdom logs "not implemented" for scrolling; the workspace calls it on tab switches.
  window.scrollTo = (() => {}) as typeof window.scrollTo;
});

function setupRoot() {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
}

afterEach(async () => {
  await act(async () => {
    root.unmount();
  });
  container.remove();
  navigation.refresh.mockClear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function render(element: ReactElement) {
  await act(async () => {
    root.render(element);
  });
}

async function waitFor(predicate: () => boolean, message: string, timeoutMs = 3000) {
  const started = Date.now();
  while (Date.now() - started < timeoutMs) {
    if (predicate()) return;
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
  }
  throw new Error(`${message}\nDOM: ${container.textContent ?? ""}`);
}

function text(): string {
  return container.textContent ?? "";
}

function buttonByText(label: string, scope: ParentNode = container): HTMLButtonElement {
  const match = [...scope.querySelectorAll("button")].find(
    (candidate) => candidate.textContent?.trim() === label,
  );
  if (!match) throw new Error(`找不到按钮: ${label}`);
  return match as HTMLButtonElement;
}

async function click(element: Element | null) {
  if (!element) throw new Error("点击目标不存在");
  await act(async () => {
    (element as HTMLElement).click();
  });
}

async function setInputValue(input: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(
    window.HTMLInputElement.prototype,
    "value",
  )?.set;
  if (!setter) throw new Error("缺少 HTMLInputElement.value setter");
  await act(async () => {
    setter.call(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

async function submitPasswordChange(oldPassword: string, newPassword: string) {
  await click(buttonByText("账号设置"));
  await waitFor(() => container.querySelector("dialog") !== null, "账号设置弹窗未出现");
  await click(buttonByText("修改密码", container.querySelector("dialog") ?? container));
  await waitFor(() => container.querySelector("dialog") !== null, "修改密码弹窗未出现");

  const dialog = container.querySelector("dialog");
  const inputs = [...(dialog?.querySelectorAll<HTMLInputElement>('input[type="password"]') ?? [])];
  expect(inputs).toHaveLength(3);
  await setInputValue(inputs[0], oldPassword);
  await setInputValue(inputs[1], newPassword);
  await setInputValue(inputs[2], newPassword);
  await click(buttonByText("保存密码", dialog ?? container));
}

describe("dashboard runtime upload limit", () => {
  it("shows the runtime 10MiB limit instead of the build-time 5MiB default", async () => {
    setupRoot();
    const calls = installFetch(dashboardHandler({ uploadMaxBytes: 10 << 20 }));

    await render(<DashboardPage />);
    await waitFor(() => text().includes("单张最大 10MB"), "未显示运行时 10MB 上限");

    expect(text()).not.toContain("单张最大 5MB");
    expect(calls.some((call) => call.url === "/api/config")).toBe(true);
  });

  it("renders sub-MiB limits in KB instead of 0MB", async () => {
    setupRoot();
    installFetch(dashboardHandler({ uploadMaxBytes: 512 << 10 }));

    await render(<DashboardPage />);
    await waitFor(() => text().includes("单张最大 512KB"), "未显示 512KB 上限");

    expect(text()).not.toContain("单张最大 0MB");
  });

  it("shows an explicit retryable error when the config request fails", async () => {
    setupRoot();
    let configHealthy = false;
    installFetch((url, init) => {
      if (url === "/api/config" && !configHealthy) {
        return { status: 500, body: { message: "配置读取失败" } };
      }
      return dashboardHandler({ uploadMaxBytes: 10 << 20 })(url, init);
    });

    await render(<DashboardPage />);
    await waitFor(() => text().includes("无法读取上传限制"), "配置失败时未显示明确错误");
    expect(text()).toContain("配置读取失败");
    expect(text()).not.toContain("单张最大");

    configHealthy = true;
    await click(buttonByText("重试"));
    await waitFor(() => text().includes("单张最大 10MB"), "重试后未加载工作台");
  });

  it("rejects a malformed config instead of guessing a default", async () => {
    setupRoot();
    installFetch(dashboardHandler({ uploadMaxBytes: "big" }));

    await render(<DashboardPage />);
    await waitFor(() => text().includes("上传限制配置无效"), "非法配置未被拒绝");
    expect(text()).not.toContain("单张最大");
  });
});

describe("password change success notice", () => {
  function handlerWithPasswordChange() {
    return (url: string, init?: RequestInit): Reply => {
      if (url === "/api/auth/change-password") {
        return { body: { ok: true } };
      }
      return dashboardHandler({ uploadMaxBytes: 10 << 20 })(url, init);
    };
  }

  it("keeps the notice visible on the static page without a full reload", async () => {
    setupRoot();
    const calls = installFetch(handlerWithPasswordChange());

    await render(<DashboardPage />);
    await waitFor(() => text().includes("我的图片"), "工作台未加载");
    await submitPasswordChange("old-password", "new-password");
    await waitFor(
      () => text().includes("密码已修改，其他设备需重新登录。"),
      "静态页改密后未显示成功提示",
    );

    expect(navigation.refresh).not.toHaveBeenCalled();
    expect(text()).toContain("owner@example.com");
    expect(
      calls.some(
        (call) =>
          call.url === "/api/auth/change-password" && call.init?.method === "POST",
      ),
    ).toBe(true);
  });

  it("still refreshes for the default Next component", async () => {
    setupRoot();
    installFetch(handlerWithPasswordChange());

    await render(<DashboardClient user={adminUser} />);
    await waitFor(() => text().includes("我的图片"), "工作台未加载");
    await submitPasswordChange("old-password", "new-password");
    await waitFor(
      () => navigation.refresh.mock.calls.length === 1,
      "默认组件改密后未调用 router.refresh()",
    );
  });
});

describe("purge partial results", () => {
  async function openTrashWithTwoImages(onBatch: (body: Record<string, unknown>) => Reply) {
    setupRoot();
    const trashItems = [imageItem("id-a", "alpha.png", true), imageItem("id-b", "beta.png", true)];
    const calls = installFetch(
      dashboardHandler({ uploadMaxBytes: 10 << 20, trashItems, onBatch }),
    );

    await render(<DashboardPage />);
    await waitFor(() => text().includes("我的图片"), "工作台未加载");
    await click(buttonByText("🗑 回收站"));
    await waitFor(
      () => container.querySelectorAll(".compact-image-card").length === 2,
      "回收站未加载两张图片",
    );

    const selectAll = container.querySelector<HTMLInputElement>(".image-select-all input");
    await click(selectAll);
    await waitFor(() => text().includes("已选择 2 张"), "全选未生效");
    return { calls, dialog: () => container.querySelector("dialog") };
  }

  it("removes only updatedIds, keeps failed rows selected, and reports cleanupPending", async () => {
    const batchBodies: Record<string, unknown>[] = [];
    const { calls, dialog } = await openTrashWithTwoImages((body) => {
      batchBodies.push(body);
      const ids = body.ids as string[];
      if (batchBodies.length === 1) {
        return {
          body: {
            ok: true,
            updatedCount: 1,
            failedCount: 1,
            cleanupPending: 1,
            updatedIds: [ids[0]],
          },
        };
      }
      return {
        body: { ok: true, updatedCount: ids.length, failedCount: 0, cleanupPending: 0, updatedIds: ids },
      };
    });

    await click(buttonByText("彻底删除"));
    await waitFor(() => dialog() !== null, "确认弹窗未出现");
    const dialogText = dialog()?.textContent ?? "";
    expect(dialogText).toContain("不可恢复");
    expect(dialogText).not.toContain("立即从磁盘移除");

    await click(buttonByText("彻底删除", dialog() ?? container));
    await waitFor(() => text().includes("已彻底删除 1 张"), "未显示精确删除数量");
    expect(text()).toContain("1 张删除失败，仍保留在列表中");
    expect(text()).toContain("1 张的文件清理尚未完成");
    expect(text()).toContain("重启服务重试");
    expect(text()).not.toContain("自动重试");
    // 失败的图片必须还在网格里，且保持选中以便只重试它。
    expect(container.querySelectorAll(".compact-image-card").length).toBe(1);
    expect(container.textContent).toContain("beta.png");
    expect(container.textContent).not.toContain("alpha.png");
    await waitFor(() => text().includes("已选择 1 张"), "失败项未保持选中");

    // 重试只提交失败项（不能带上已删除的 id，否则整批 403）。
    await click(buttonByText("彻底删除"));
    await waitFor(() => dialog() !== null, "重试确认弹窗未出现");
    await click(buttonByText("彻底删除", dialog() ?? container));
    await waitFor(() => text().includes("已彻底删除 1 张") && !text().includes("已选择"), "重试后未清空列表与选择");
    expect(container.querySelectorAll(".compact-image-card").length).toBe(0);

    expect(batchBodies).toHaveLength(2);
    expect(batchBodies[0].ids).toEqual(["id-a", "id-b"]);
    expect(batchBodies[1].ids).toEqual(["id-b"]);
    const purgeCalls = calls.filter((call) => call.url === "/api/images/batch");
    expect(purgeCalls).toHaveLength(2);
  });

  it("discards the stale confirm snapshot when the purge request itself fails", async () => {
    const batchBodies: Record<string, unknown>[] = [];
    const { calls } = await openTrashWithTwoImages((body) => {
      batchBodies.push(body);
      return { status: 500, body: { ok: false, error: { code: "INTERNAL_ERROR", message: "批量删除失败（模拟）" } } };
    });

    await click(buttonByText("彻底删除"));
    await waitFor(() => container.querySelector("dialog") !== null, "确认弹窗未出现");
    await click(buttonByText("彻底删除", container.querySelector("dialog") ?? container));

    await waitFor(() => text().includes("批量删除失败（模拟）"), "未显示请求失败的错误提示");
    // 失败后确认框必须关闭，旧 id 快照不能继续提供可重试按钮。
    expect(container.querySelector("dialog")).toBeNull();
    expect(
      [...container.querySelectorAll("button")].filter(
        (candidate) => candidate.textContent?.trim() === "彻底删除",
      ),
    ).toHaveLength(0);
    // 选择已清空；列表按刷新后的现状重新显示（mock 仍返回两张回收站图片）。
    expect(text()).not.toContain("已选择");
    expect(container.querySelectorAll(".compact-image-card").length).toBe(2);
    // 没有自动重发旧 id：批次请求只发生一次，刷新则重新拉取了回收站列表。
    expect(batchBodies).toHaveLength(1);
    expect(calls.filter((call) => call.url === "/api/images/batch")).toHaveLength(1);
    expect(calls.filter((call) => call.url.includes("trashed=true")).length).toBeGreaterThanOrEqual(2);
  });

  it("keeps every row visible when the backend reports an empty updatedIds set", async () => {
    const { dialog } = await openTrashWithTwoImages(() => ({
      body: {
        ok: true,
        updatedCount: 0,
        failedCount: 2,
        cleanupPending: 0,
        updatedIds: [],
      },
    }));

    await click(buttonByText("彻底删除"));
    await waitFor(() => dialog() !== null, "确认弹窗未出现");
    await click(buttonByText("彻底删除", dialog() ?? container));
    await waitFor(() => text().includes("2 张删除失败"), "未显示失败数量");

    expect(container.querySelectorAll(".compact-image-card").length).toBe(2);
    expect(text()).toContain("已选择 2 张");
    expect(text()).not.toContain("已彻底删除 1 张");
  });
});

describe("mail capability gating", () => {
  // Anonymous browser: /api/auth/me 401 plus the public runtime config.
  function anonymousHandler(mailEnabled: unknown) {
    return (url: string): Reply => {
      if (url === "/api/auth/me") {
        return { status: 401, body: { code: "UNAUTHORIZED", message: "请先登录" } };
      }
      if (url === "/api/config") {
        return { body: { ok: true, uploadMaxBytes: 10 << 20, mailEnabled } };
      }
      return { status: 404, body: { message: `unexpected fetch: ${url}` } };
    };
  }

  function forgotPasswordLink() {
    return container.querySelector('a[href="/forgot-password"]');
  }

  it("points login users at the admin when mail is not configured", async () => {
    setupRoot();
    const calls = installFetch(anonymousHandler(false));

    await render(<DashboardPage />);
    await waitFor(() => text().includes("请联系管理员重置密码"), "未显示联系管理员重置密码");
    expect(forgotPasswordLink()).toBeNull();
    expect(text()).toContain("邮件服务未启用");
    expect(calls.some((call) => call.url === "/api/config")).toBe(true);
  });

  it("keeps the original forgot-password link when mail is enabled", async () => {
    setupRoot();
    installFetch(anonymousHandler(true));

    await render(<DashboardPage />);
    await waitFor(() => forgotPasswordLink() !== null, "启用邮件后未显示找回密码入口");
    expect(text()).not.toContain("请联系管理员重置密码");
  });

  it("keeps the link when the config read fails, since the capability is unknown", async () => {
    setupRoot();
    installFetch((url) => {
      if (url === "/api/auth/me") {
        return { status: 401, body: { code: "UNAUTHORIZED", message: "请先登录" } };
      }
      if (url === "/api/config") {
        return { status: 500, body: { message: "配置读取失败" } };
      }
      return { status: 404, body: { message: `unexpected fetch: ${url}` } };
    });

    await render(<DashboardPage />);
    await waitFor(() => forgotPasswordLink() !== null, "配置失败时应保留原找回密码入口");
    expect(text()).not.toContain("请联系管理员重置密码");
  });

  it("replaces the forgot-password form with an admin notice when mail is off", async () => {
    setupRoot();
    installFetch(anonymousHandler(false));

    await render(<AccountRecoveryScreen mode="forgot" />);
    await waitFor(() => text().includes("当前部署未启用邮件服务"), "未显示邮件未启用提示");
    expect(text()).toContain("请联系管理员重置密码");
    expect(container.querySelector('input[type="email"]')).toBeNull();
    expect(container.querySelector('button[type="submit"]')).toBeNull();
  });

  it("keeps the forgot-password form when mail is enabled", async () => {
    setupRoot();
    installFetch(anonymousHandler(true));

    await render(<AccountRecoveryScreen mode="forgot" />);
    await waitFor(() => text().includes("发送重置邮件"), "启用邮件后未显示发送按钮");
    expect(container.querySelector('input[type="email"]')).not.toBeNull();
  });

  it("disables change-email with an admin hint when mail is off", async () => {
    setupRoot();
    installFetch(dashboardHandler({ uploadMaxBytes: 10 << 20, mailEnabled: false }));

    await render(<DashboardPage />);
    await waitFor(() => text().includes("我的图片"), "工作台未加载");
    await click(buttonByText("账号设置"));
    await waitFor(() => container.querySelector("dialog") !== null, "账号设置弹窗未出现");

    const emailButton = buttonByText("修改邮箱", container.querySelector("dialog") ?? container);
    expect(emailButton.disabled).toBe(true);
    expect(text()).toContain("请联系管理员在后台修改");
  });

  it("keeps change-email usable when mail is enabled", async () => {
    setupRoot();
    installFetch(dashboardHandler({ uploadMaxBytes: 10 << 20, mailEnabled: true }));

    await render(<DashboardPage />);
    await waitFor(() => text().includes("我的图片"), "工作台未加载");
    await click(buttonByText("账号设置"));
    await waitFor(() => container.querySelector("dialog") !== null, "账号设置弹窗未出现");

    const emailButton = buttonByText("修改邮箱", container.querySelector("dialog") ?? container);
    expect(emailButton.disabled).toBe(false);
    expect(text()).not.toContain("请联系管理员在后台修改");
  });
});

describe("login card copy, layout and failure feedback", () => {
  function authTab(mode: "login" | "register") {
    return container.querySelector<HTMLButtonElement>(`#auth-tab-${mode}`);
  }

  function submitButton() {
    return container.querySelector<HTMLButtonElement>('button[type="submit"]');
  }

  function passwordInput() {
    return container.querySelector<HTMLInputElement>("#auth-password");
  }

  async function fillCredentials(email = "owner@example.com", password = "secret123") {
    const emailInput = container.querySelector<HTMLInputElement>('input[type="email"]');
    if (!emailInput) throw new Error("找不到邮箱输入框");
    await setInputValue(emailInput, email);
    const passwordField = passwordInput();
    if (!passwordField) throw new Error("找不到密码输入框");
    await setInputValue(passwordField, password);
  }

  it("describes first-account registration consistently in head and code hint", async () => {
    setupRoot();
    await render(<LoginCard />);
    await click(authTab("register"));
    await waitFor(() => text().includes("创建账号"), "未切到注册标签");

    const head = container.querySelector(".auth-card-head")?.textContent ?? "";
    expect(head).toContain("首个账号无需激活码");
    expect(head).toContain("管理员");
    expect(head).not.toContain("注册需要管理员提供的激活码");

    const codeNote = container.querySelector(".auth-label .auth-note")?.textContent ?? "";
    expect(codeNote).toContain("首个账号无需激活码");
    expect(codeNote).toContain("管理员");
    expect(text()).not.toContain("第一个账号无需激活码");
  });

  it("puts the mail-off recovery hint below the password input and links it", async () => {
    setupRoot();
    await render(<LoginCard mailEnabled={false} />);

    const input = passwordInput();
    const note = container.querySelector<HTMLElement>("#auth-password-note");
    expect(input).not.toBeNull();
    expect(note).not.toBeNull();
    expect(input?.getAttribute("aria-describedby")).toBe("auth-password-note");
    expect(note?.className).toBe("auth-note");
    expect(note?.textContent).toContain("邮件服务未启用");
    expect(note?.textContent).toContain("请联系管理员重置密码");

    // 提示在输入框之后，且不在「密码」标签那一行里。
    expect(
      (input as HTMLElement).compareDocumentPosition(note as HTMLElement) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    const labelRow = container.querySelector(".auth-password-head");
    expect(labelRow).not.toBeNull();
    expect(labelRow?.contains(note as HTMLElement)).toBe(false);
    expect(labelRow?.textContent).not.toContain("邮件服务未启用");
    expect(container.querySelector('a[href="/forgot-password"]')).toBeNull();

    // 可见密码按钮仍然可用。
    const toggle = container.querySelector<HTMLButtonElement>(".auth-password-toggle");
    expect(toggle).not.toBeNull();
    expect(toggle?.getAttribute("aria-label")).toBe("显示密码");
    await click(toggle);
    expect(passwordInput()?.type).toBe("text");
    expect(toggle?.getAttribute("aria-label")).toBe("隐藏密码");
  });

  it("keeps the forgot-password entry and no admin note when mail is enabled", async () => {
    setupRoot();
    await render(<LoginCard mailEnabled={true} />);

    expect(container.querySelector('a[href="/forgot-password"]')).not.toBeNull();
    expect(container.querySelector("#auth-password-note")).toBeNull();
    expect(passwordInput()?.getAttribute("aria-describedby")).toBeNull();
    expect(container.querySelector(".auth-password-toggle")).not.toBeNull();
    expect(text()).not.toContain("请联系管理员重置密码");
  });

  it("shows a retryable message when the login request fails at the network layer", async () => {
    setupRoot();
    let networkDown = true;
    installFetch(() =>
      networkDown ? "network" : { body: { ok: true, user: adminUser } },
    );

    await render(<LoginCard mailEnabled={true} />);
    await fillCredentials();
    await click(submitButton());
    await waitFor(() => text().includes("网络异常"), "网络异常时未显示错误提示");
    expect(text()).toContain("重试");
    expect(text()).not.toContain("network down");
    expect(submitButton()?.disabled).toBe(false);
    expect(navigation.refresh).not.toHaveBeenCalled();

    networkDown = false;
    await click(submitButton());
    await waitFor(() => navigation.refresh.mock.calls.length === 1, "重试成功后未刷新");
    expect(text()).not.toContain("网络异常");
  });

  it("surfaces the server error message and keeps the form retryable", async () => {
    setupRoot();
    let attempt = 0;
    installFetch(() => {
      attempt += 1;
      return attempt === 1
        ? { status: 401, body: { error: { message: "邮箱或密码错误" } } }
        : { body: { ok: true, user: adminUser } };
    });

    await render(<LoginCard mailEnabled={true} />);
    await fillCredentials();
    await click(submitButton());
    await waitFor(() => text().includes("邮箱或密码错误"), "未显示服务端错误信息");
    expect(container.querySelector('[role="alert"]')?.textContent).toBe("邮箱或密码错误");
    expect(submitButton()?.disabled).toBe(false);

    await click(submitButton());
    await waitFor(() => navigation.refresh.mock.calls.length === 1, "重试成功后未刷新");
    expect(text()).not.toContain("邮箱或密码错误");
  });

  it("disables both mode tabs and the register switch while the request is in flight", async () => {
    setupRoot();
    const gate: { release: null | ((reply: { status?: number; body: RawBody }) => void) } = {
      release: null,
    };
    vi.stubGlobal(
      "fetch",
      vi.fn(
        () =>
          new Promise<Response>((resolve) => {
            gate.release = (reply) => resolve(jsonResponse(reply.body, reply.status ?? 200));
          }),
      ),
    );

    await render(<LoginCard mailEnabled={true} />);
    await fillCredentials();
    await click(submitButton());
    await waitFor(() => text().includes("处理中..."), "未进入加载态");

    // 提交期间登录/注册都不能切：凭证可能已写入会话 Cookie，界面不能先变。
    expect(authTab("login")?.disabled).toBe(true);
    expect(authTab("register")?.disabled).toBe(true);
    await click(authTab("register"));
    expect(authTab("login")?.getAttribute("aria-selected")).toBe("true");
    expect(authTab("register")?.getAttribute("aria-selected")).toBe("false");
    expect(text()).toContain("欢迎回来");
    expect(text()).not.toContain("创建账号");

    // 失败后恢复：两个 tab 与「立即注册」重新可用，能切到注册。
    await act(async () => {
      gate.release?.({
        status: 401,
        body: { error: { message: "邮箱或密码错误" } },
      });
      await new Promise((resolve) => setTimeout(resolve, 20));
    });

    await waitFor(() => text().includes("邮箱或密码错误"), "未显示失败提示");
    expect(authTab("login")?.disabled).toBe(false);
    expect(authTab("register")?.disabled).toBe(false);
    const registerLink = [...container.querySelectorAll<HTMLButtonElement>(".auth-switch-link")].find(
      (candidate) => candidate.textContent?.trim() === "立即注册",
    );
    expect(registerLink?.disabled).toBe(false);

    await click(authTab("register"));
    await waitFor(() => text().includes("创建账号"), "未切到注册标签");
    expect(authTab("register")?.getAttribute("aria-selected")).toBe("true");
    expect(text()).not.toContain("邮箱或密码错误");
    expect(submitButton()?.textContent?.trim()).toBe("注册账号");
    expect(submitButton()?.disabled).toBe(false);
    expect(navigation.refresh).not.toHaveBeenCalled();
  });
});
