/**
 * The original client components were written for Next.js and call
 * `useRouter().refresh()` after login, logout and password changes. The static
 * bundle has no server render to re-run, so refresh has to be a real browser
 * navigation: the Go binary re-evaluates the session cookie and serves the same
 * shell, and the page then refetches /api/auth/me.
 *
 * A no-op here would leave the UI showing the pre-login state with a valid
 * cookie, so every navigation helper below performs an actual load.
 */

const router = {
  refresh(): void {
    window.location.reload();
  },
  push(href: string): void {
    window.location.assign(href);
  },
  replace(href: string): void {
    window.location.replace(href);
  },
  back(): void {
    window.history.back();
  },
  forward(): void {
    window.history.forward();
  },
  prefetch(): void {
    // Nothing to prefetch: routes are separate document loads served by Go.
  },
};

export function useRouter() {
  return router;
}

export function usePathname(): string {
  return window.location.pathname;
}

export function useSearchParams(): URLSearchParams {
  return new URLSearchParams(window.location.search);
}

export function redirect(href: string): never {
  window.location.replace(href);
  // The navigation replaces the document; this keeps the return type honest
  // for call sites that expect a never-returning helper.
  throw new Error(`redirecting to ${href}`);
}

export function notFound(): never {
  throw new Error("not found");
}
