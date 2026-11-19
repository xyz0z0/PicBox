import { useEffect, useState, type ReactNode } from "react";

type ThemeMode = "light" | "dark";
const THEME_STORAGE_KEY = "picbox-theme";

function readInitialTheme(): ThemeMode {
  try {
    const saved = window.localStorage?.getItem(THEME_STORAGE_KEY);
    if (saved === "light" || saved === "dark") return saved;
    if (window.matchMedia?.("(prefers-color-scheme: dark)").matches) return "dark";
  } catch {
    // Ignore storage/matchMedia unavailability in SSR or restricted environments.
  }
  return "light";
}

/**
 * Mirrors the original layout: the page tree sits inside .site-content and the
 * version footer is rendered once for every route. The version is baked in at
 * build time from frontend/package.json.
 */
export function AppShell({ children }: { children: ReactNode }) {
  const year = new Date().getFullYear();
  const [theme, setTheme] = useState<ThemeMode>(readInitialTheme);

  useEffect(() => {
    try {
      document.documentElement.dataset.theme = theme;
    } catch {
      // Ignore DOM unavailability.
    }
  }, [theme]);

  function toggleTheme() {
    const next: ThemeMode = theme === "dark" ? "light" : "dark";
    setTheme(next);
    try {
      document.documentElement.dataset.theme = next;
      window.localStorage?.setItem(THEME_STORAGE_KEY, next);
    } catch {
      // Ignore storage unavailability.
    }
  }

  return (
    <>
      <div className="site-content">{children}</div>
      <footer className="site-footer">
        <span>© {year} PicBox · v{__PICBOX_VERSION__}</span>
        <button
          type="button"
          className="theme-toggle-btn"
          onClick={toggleTheme}
          aria-label={theme === "dark" ? "切换为日间美术纸主题" : "切换为夜间暗房主题"}
        >
          {theme === "dark" ? "日间美术纸" : "夜间暗房"}
        </button>
      </footer>
    </>
  );
}
