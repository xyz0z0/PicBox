import { useEffect, useState } from "react";
import HomeView from "../../app/components/HomeView";
import { fetchCurrentUser, type ApiUser } from "../api";

/**
 * The original home page decides the CTA from the server session. This page asks
 * /api/auth/me instead and renders the same extracted HomeView. Nothing is
 * painted before the answer arrives, so the nav never shows "登录 / 注册" to an
 * already signed-in user and then flips.
 */
export function HomePage() {
  const [ready, setReady] = useState(false);
  const [user, setUser] = useState<ApiUser | null>(null);

  useEffect(() => {
    let active = true;
    void fetchCurrentUser().then((result) => {
      if (!active) return;
      setUser(result.state === "authenticated" ? result.user : null);
      setReady(true);
    });
    return () => {
      active = false;
    };
  }, []);

  if (!ready) return null;

  return (
    <HomeView
      isLoggedIn={Boolean(user)}
      isAdmin={user?.role === "ADMIN"}
    />
  );
}
