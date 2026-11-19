import { afterEach, describe, expect, it, vi } from "vitest";
import { act, useState, type ReactElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { NotFoundPage } from "../pages/not-found-page";
import { DashboardPage } from "../pages/dashboard-page";
import { HomePage } from "../pages/home-page";
import { ImagePage } from "../pages/image-page";
import { currentPath, resolveRoute, RootApp, type Route } from "../routes";

/**
 * Offline behaviour tests for the two fallbacks the static shell owes the
 * original Next.js app:
 *
 *  - unknown addresses (including /index.html, which Next.js never routes,
 *    and /images/ ids that are malformed or outside the Go route pattern)
 *    must render the original Chinese 404 UI inside the normal shell, not a
 *    stripped page or a blank document;
 *  - a page that throws during render must show the original app/error.tsx
 *    copy with 重试 / 返回首页, keep the shell and theme mounted, and remount
 *    the page on retry so a permanently failing page is never a white screen.
 *
 * No network is used: these tests render only routes that do not fetch.
 */

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let container: HTMLDivElement;
let root: Root;

afterEach(async () => {
  await act(async () => {
    root.unmount();
  });
  container.remove();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function setupRoot() {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
}

async function renderRoute(route: Route) {
  await act(async () => {
    root.render(<RootApp route={route} />);
  });
}

function text(): string {
  return container.textContent ?? "";
}

function elementType(route: Route): unknown {
  return (route.element as { type?: unknown }).type;
}

function elementProps(route: Route): Record<string, unknown> {
  return (route.element as { props?: Record<string, unknown> }).props ?? {};
}

function linkByText(label: string, scope: ParentNode = container): HTMLAnchorElement | null {
  return (
    [...scope.querySelectorAll("a")].find(
      (candidate) => candidate.textContent?.trim() === label,
    ) ?? null
  );
}

function buttonByText(label: string, scope: ParentNode = container): HTMLButtonElement | null {
  return (
    [...scope.querySelectorAll("button")].find(
      (candidate) => candidate.textContent?.trim() === label,
    ) ?? null
  );
}

async function click(element: Element | null) {
  if (!element) throw new Error("点击目标不存在");
  await act(async () => {
    (element as HTMLElement).click();
  });
}

function silenceErrorLog() {
  return vi.spyOn(console, "error").mockImplementation(() => {});
}

describe("unknown address fallback", () => {
  it("renders the original Chinese 404 UI inside the shell", async () => {
    setupRoot();
    const path = currentPath("/ui-audit-unknown");
    expect(path).toBe("/ui-audit-unknown");

    await renderRoute(resolveRoute(path));

    expect(text()).toContain("404");
    expect(text()).toContain("页面不存在，或图片已被删除 / 不可见。");
    const home = linkByText("返回首页");
    expect(home?.getAttribute("href")).toBe("/");

    // The normal layout stays: .site-content wrapper, version footer and the
    // page uses the same theme tokens instead of inline hard-coded colours.
    expect(container.querySelector(".site-content")).not.toBeNull();
    expect(container.querySelector(".site-footer")?.textContent).toContain("PicBox · v");
    expect(container.querySelector("main")?.getAttribute("style")).toContain("var(--ph-bg)");
    expect(container.querySelector("main")?.getAttribute("style")).toContain("var(--ph-text)");
  });

  it("treats /index.html as unknown instead of showing the home page", async () => {
    // The Go shell serves dist/index.html bytes for /index.html with HTTP 404
    // (no such route exists in the original app), so the client must render
    // the 404 UI there as well.
    expect(currentPath("/index.html")).toBe("/index.html");
    const route = resolveRoute("/index.html");
    expect(elementType(route)).toBe(NotFoundPage);
    expect(route.title).toBe("PicBox");

    setupRoot();
    await renderRoute(route);
    expect(text()).toContain("页面不存在，或图片已被删除 / 不可见。");
    expect(linkByText("返回首页")).not.toBeNull();
  });

  it("keeps the original page routes and normalizes trailing slashes", () => {
    expect(currentPath("/")).toBe("/");
    expect(currentPath("/dashboard/")).toBe("/dashboard");
    expect(currentPath("/images/abc/")).toBe("/images/abc");
    // Double slashes are not redirected by the Go page routes (they fall to the
    // 404 shell), so they must not resolve to a live page either.
    expect(currentPath("/dashboard//")).toBe("/dashboard/");
    expect(elementType(resolveRoute(currentPath("/dashboard//")))).toBe(NotFoundPage);

    expect(elementType(resolveRoute(currentPath("/dashboard/")))).toBe(DashboardPage);
    expect(elementType(resolveRoute(currentPath("/images/abc/")))).toBe(ImagePage);
    expect(elementType(resolveRoute("/"))).toBe(HomePage);
    expect(resolveRoute("/").title).toBe("PicBox · 私人图床");

    expect(elementType(resolveRoute("/nope"))).toBe(NotFoundPage);
    expect(elementType(resolveRoute("/images"))).toBe(NotFoundPage);
    expect(elementType(resolveRoute("/images/abc/extra"))).toBe(NotFoundPage);
    expect(resolveRoute("/nope").title).toBe("PicBox");
  });

  it("decodes usable image ids and rejects addresses the Go route cannot serve", async () => {
    // Valid ids decode once and reach the detail page, including percent-encoded
    // characters that stay inside the Go pattern.
    expect(elementProps(resolveRoute("/images/abc-123_x.y~z")).id).toBe("abc-123_x.y~z");
    expect(elementProps(resolveRoute("/images/abc%2D123")).id).toBe("abc-123");
    const maxID = "a".repeat(64);
    expect(elementProps(resolveRoute(`/images/${maxID}`)).id).toBe(maxID);

    // Malformed escapes must not throw outside the React root (that was a blank
    // page), and ids the Go regexp rejects are served HTTP 404 there.
    for (const path of [
      "/images/%",
      "/images/%zz",
      "/images/%E0%A4",
      "/images/abc%2Fdef",
      "/images/abc%20def",
      "/images/..%2Fetc",
      `/images/${"a".repeat(65)}`,
    ]) {
      expect(() => resolveRoute(path)).not.toThrow();
      expect(elementType(resolveRoute(path))).toBe(NotFoundPage);
    }

    // Rendering such an address is the plain 404 page and sends no /meta request.
    const fetchSpy = vi.fn(() => {
      throw new Error("unexpected fetch");
    });
    vi.stubGlobal("fetch", fetchSpy);
    setupRoot();
    await renderRoute(resolveRoute("/images/%E0%A4"));
    expect(text()).toContain("页面不存在，或图片已被删除 / 不可见。");
    expect(text()).not.toContain("页面出错了");
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

function Boom({ fail }: { fail: boolean }) {
  if (fail) throw new Error("渲染失败（模拟）");
  return <p>恢复后的页面</p>;
}

function Counter({ fail }: { fail: boolean }) {
  const [count, setCount] = useState(0);
  if (fail) throw new Error("渲染失败（模拟）");
  return (
    <button type="button" onClick={() => setCount((value) => value + 1)}>
      计数 {count}
    </button>
  );
}

describe("render error fallback", () => {
  it("shows the original 页面出错了 copy, retry and home link, keeping the shell", async () => {
    setupRoot();
    const logged = silenceErrorLog();

    await renderRoute({ title: "PicBox", element: <Boom fail /> });

    expect(text()).toContain("页面出错了");
    expect(text()).toContain("渲染失败（模拟）");
    expect(text()).not.toContain("恢复后的页面");

    const retry = buttonByText("重试");
    expect(retry).not.toBeNull();
    expect(retry?.getAttribute("type")).toBe("button");
    expect(linkByText("返回首页")?.getAttribute("href")).toBe("/");

    // Layout and theme survive the failure.
    expect(container.querySelector(".site-content")).not.toBeNull();
    expect(container.querySelector(".site-footer")?.textContent).toContain("PicBox · v");
    expect(container.querySelector("main")?.getAttribute("style")).toContain("var(--ph-bg)");

    // The stack is still reported for diagnosis.
    expect(logged).toHaveBeenCalled();
  });

  it("uses the original fallback message when the thrown error has none", async () => {
    setupRoot();
    silenceErrorLog();
    function SilentBoom(): ReactElement {
      throw new Error("");
    }

    await renderRoute({ title: "PicBox", element: <SilentBoom /> });

    expect(text()).toContain("页面出错了");
    expect(text()).toContain("发生了未预期的错误，请重试。");
    expect(buttonByText("重试")).not.toBeNull();
  });

  it("keeps the error page after retrying a page that always fails", async () => {
    setupRoot();
    silenceErrorLog();

    await renderRoute({ title: "PicBox", element: <Boom fail /> });
    await click(buttonByText("重试"));

    // Caught again instead of leaving an empty #root.
    expect(text()).toContain("页面出错了");
    expect(buttonByText("重试")).not.toBeNull();
    expect(linkByText("返回首页")).not.toBeNull();
    expect(container.querySelector(".site-footer")).not.toBeNull();
    expect(text().trim().length).toBeGreaterThan(0);
  });

  it("remounts the page on 重试 instead of resuming the tree that threw", async () => {
    setupRoot();
    silenceErrorLog();

    await renderRoute({ title: "PicBox", element: <Counter fail={false} /> });
    await click(buttonByText("计数 0"));
    expect(buttonByText("计数 1")).not.toBeNull();

    await renderRoute({ title: "PicBox", element: <Counter fail /> });
    expect(text()).toContain("页面出错了");

    // Fix the cause, then retry: the page must start from a clean mount (0),
    // proving it was unmounted rather than resumed after the error.
    await renderRoute({ title: "PicBox", element: <Counter fail={false} /> });
    await click(buttonByText("重试"));

    expect(buttonByText("计数 0")).not.toBeNull();
    expect(text()).not.toContain("页面出错了");
  });
});
