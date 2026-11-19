import { StrictMode, type ReactNode } from "react";
import AccountRecoveryScreen from "../app/dashboard/AccountRecoveryScreen";
import { AppShell } from "./app-shell";
import { RootErrorBoundary } from "./error-boundary";
import { AdminPage } from "./pages/admin-page";
import { DashboardPage } from "./pages/dashboard-page";
import { HomePage } from "./pages/home-page";
import { ImagePage } from "./pages/image-page";
import { NotFoundPage } from "./pages/not-found-page";

/**
 * Route table for the static bundle. The Go binary serves this shell for
 * /, /dashboard, /admin, the three account-recovery pages, /images/{id} and —
 * with HTTP 404 — for any other address, so the browser alone decides which
 * UI that shell renders. Links between pages are plain <a> navigations, which
 * is what the original markup already used. Each load re-reads the session
 * cookie, so login state is never stale.
 */

export type Route = { title: string; element: ReactNode };

// Mirrors webImageIDPattern in internal/server/web.go. The Go handler only
// serves the detail shell when the decoded id is 1-64 of these characters;
// everything else already answers HTTP 404 with the shell, so the client must
// not route it to ImagePage (that would fire a meaningless /meta request).
const IMAGE_ID_PATTERN = /^[A-Za-z0-9._~-]{1,64}$/;

/**
 * Decodes one path segment into an image id, or null when the address cannot
 * name an image. Malformed escapes (%zz, %E0%A4) make decodeURIComponent
 * throw, and this runs before React is mounted: letting that escape would
 * blank the document without ever reaching the error boundary. Those
 * addresses are unknown paths, so they render the same 404 UI as Go serves.
 */
function decodeImageID(segment: string): string | null {
  let decoded: string;
  try {
    decoded = decodeURIComponent(segment);
  } catch {
    return null;
  }
  return IMAGE_ID_PATTERN.test(decoded) ? decoded : null;
}

export function currentPath(pathname: string): string {
  // Only the single trailing slash Go redirects (308) for a registered page is
  // canonicalized here. A double slash is an unknown path on the server, so it
  // must stay unknown in the client and render the 404 UI, not a live page.
  const path = pathname.replace(/\/$/, "");
  return path === "" ? "/" : path;
}

export function resolveRoute(path: string): Route {
  if (path === "/dashboard") return { title: "PicBox", element: <DashboardPage /> };
  if (path === "/admin") return { title: "PicBox", element: <AdminPage /> };
  if (path === "/forgot-password") {
    return { title: "PicBox", element: <AccountRecoveryScreen mode="forgot" /> };
  }
  if (path === "/reset-password") {
    return { title: "PicBox", element: <AccountRecoveryScreen mode="reset" /> };
  }
  if (path === "/verify-email") {
    return { title: "PicBox", element: <AccountRecoveryScreen mode="verify" /> };
  }

  const imageMatch = /^\/images\/([^/]+)$/.exec(path);
  if (imageMatch) {
    const id = decodeImageID(imageMatch[1]);
    if (id !== null) {
      return { title: "PicBox", element: <ImagePage id={id} /> };
    }
  }

  if (path === "/") {
    // The original home page overrides the layout title for the landing page.
    return { title: "PicBox · 私人图床", element: <HomePage /> };
  }

  // Everything else is the original App Router 404 boundary: unknown paths,
  // unusable /images/ ids and, deliberately, /index.html. The original app has
  // no /index.html route or public file, so Next.js answered that address with
  // its not-found page; the Go shell does the same (index.html bytes, HTTP 404)
  // and this table must not turn it back into a successful-looking home page.
  return { title: "PicBox", element: <NotFoundPage /> };
}

/**
 * The whole document tree the Go shell mounts into #root: the original layout
 * (AppShell: .site-content plus the version footer) with exactly one page
 * inside a render error boundary. Keeping the boundary inside AppShell means a
 * failing page leaves the shell and theme in place while ErrorPage is shown.
 */
export function RootApp({ route }: { route: Route }) {
  return (
    <StrictMode>
      <AppShell>
        <RootErrorBoundary>{route.element}</RootErrorBoundary>
      </AppShell>
    </StrictMode>
  );
}
