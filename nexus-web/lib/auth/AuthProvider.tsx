"use client";

// AuthProvider — tracks the current user session client-side.
// On mount: reads cached /auth/me from localStorage; if token present, refetches in bg.
// Exposes: user, isLoading, isAuthed, sessionExpired, login(), logout(), refetchMe(), clearExpired().
// sessionExpired: set true when silentRefresh fires "nexus:session-expired" event → shows modal.
import { useQueryClient } from "@tanstack/react-query";
import {
  createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode,
} from "react";
import {
  login as apiLogin,
  logout as apiLogout,
  verifyMfa as apiVerifyMfa,
  getMe,
  getCachedMe,
  isAuthed,
  ensureAuth,
  type AuthMe,
  type LoginResult,
} from "@/lib/api";

interface AuthCtx {
  user: AuthMe | null;
  isLoading: boolean;
  isAuthed: boolean;
  /** True when 401 → refresh-fail fires — shows SessionExpiredModal */
  sessionExpired: boolean;
  clearExpired: () => void;
  /**
   * Step 1. Two possible outcomes (ENG-AUTH2 #7):
   *   { status: "ok", user }          — session established, user set.
   *   { status: "mfa", pendingToken } — credentials correct but a second factor is required;
   *                                     NO token stored, user NOT set. Call completeMfa next.
   */
  login: (tenant: string, email: string, password: string, rememberMe?: boolean) => Promise<LoginResult>;
  /** Step 2 of a two-step login — redeems the pendingToken + TOTP/backup code for a session. */
  completeMfa: (pendingToken: string, code: string) => Promise<AuthMe>;
  logout: () => Promise<void>;
  refetchMe: () => Promise<void>;
}

const Ctx = createContext<AuthCtx | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  // Init to null on BOTH server and first client render (do NOT read localStorage in the initializer —
  // that makes the hydrated render differ from the SSR HTML → React #418 hydration mismatch). The
  // cached user is loaded post-mount in the effect below.
  const [user, setUser] = useState<AuthMe | null>(null);
  const [isLoading, setLoading] = useState(true);
  const [sessionExpired, setSessionExpired] = useState(false);
  const bootstrapped = useRef(false);

  // Listen for silentRefresh failures → show SessionExpiredModal instead of hard redirect
  useEffect(() => {
    function onExpired() { setSessionExpired(true); }
    window.addEventListener("nexus:session-expired", onExpired);
    return () => window.removeEventListener("nexus:session-expired", onExpired);
  }, []);

  // Signing in again after an expiry stays inside this page (router.replace to /login), so the cache would still hold
  // whatever the expired session was reading, a customer's lists included (specs/073 FR-025c). Dropped here, before
  // anything is drawn for whoever signs in next.
  const qc = useQueryClient();
  const clearExpired = useCallback(() => {
    void qc.cancelQueries();
    qc.clear();
    setSessionExpired(false);
  }, [qc]);

  // On mount: ensure token (DEMO bootstrap if needed) then fetch /auth/me
  useEffect(() => {
    if (bootstrapped.current) return;
    bootstrapped.current = true;

    let leaving = false;
    (async () => {
      // Show the cached user first (post-mount, inside async → no hydration mismatch), then refresh.
      const cached = getCachedMe();
      if (cached) setUser(cached);
      try {
        await ensureAuth();
        const me = await getMe();
        setUser(me);
      } catch (e) {
        setUser(null);
        // The builder session behind this tab is gone but the home login is not: back to the Studio, not /login.
        if ((e as { code?: string } | null)?.code === "WORKSPACE_ENDED") {
          // Keep isLoading true while the hard navigation is pending so RequireAuth cannot race it to /login.
          leaving = true;
          window.location.assign("/studio");
        }
      } finally {
        if (!leaving) setLoading(false);
      }
    })();
  }, []);

  const login = useCallback(async (
    tenant: string,
    email: string,
    password: string,
    rememberMe = false,
  ): Promise<LoginResult> => {
    const res = await apiLogin(tenant, email, password, rememberMe);
    // MFA-pending: no session yet — do NOT touch user state; the login screen shows step 2.
    if (res.status === "mfa") return res;
    // After login, fetch full /auth/me to get all fields
    try {
      const full = await getMe();
      setUser(full);
      return { status: "ok", user: full };
    } catch {
      setUser(res.user);
      return res;
    }
  }, []);

  const completeMfa = useCallback(async (pendingToken: string, code: string): Promise<AuthMe> => {
    const me = await apiVerifyMfa(pendingToken, code);
    try {
      const full = await getMe();
      setUser(full);
      return full;
    } catch {
      setUser(me);
      return me;
    }
  }, []);

  const logout = useCallback(async () => {
    await apiLogout();
    setUser(null);
  }, []);

  const refetchMe = useCallback(async () => {
    try {
      const me = await getMe();
      setUser(me);
    } catch {
      setUser(null);
    }
  }, []);

  return (
    <Ctx.Provider value={{
      user,
      isLoading,
      isAuthed: isAuthed(),
      sessionExpired,
      clearExpired,
      login,
      completeMfa,
      logout,
      refetchMe,
    }}>
      {children}
    </Ctx.Provider>
  );
}

export function useAuth(): AuthCtx {
  const ctx = useContext(Ctx);
  if (!ctx) throw new Error("useAuth must be inside AuthProvider");
  return ctx;
}
