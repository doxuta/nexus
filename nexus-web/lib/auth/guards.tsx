"use client";

// Route guards: RequireAuth, RequireAdmin — used in layout.tsx of (app) route group.
import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "@/lib/auth/AuthProvider";
import { toast } from "@/components/ui/Toaster";
import { t } from "@/lib/i18n";

// Redirect to /login if not authenticated, preserving ?next= intent.
export function RequireAuth({ children }: { children: React.ReactNode }) {
  const { isLoading, user } = useAuth();
  const router = useRouter();

  useEffect(() => {
    if (!isLoading && !user) {
      const next = encodeURIComponent(window.location.pathname + window.location.search);
      router.replace(`/login?next=${next}`);
    }
  }, [isLoading, user, router]);

  if (isLoading) return null; // shell skeleton rendered by layout
  if (!user) return null;
  return <>{children}</>;
}

// Admin-only gate — fail-closed: redirects to /home if the (authenticated) user is not admin.
// Used by app/admin/layout.tsx — a customer's tenant-admin governs users/roles/teams/data here.
// Nest inside RequireAuth so `user` is already resolved here.
export function RequireAdmin({ children }: { children: React.ReactNode }) {
  const { user, isLoading } = useAuth();
  const router = useRouter();

  useEffect(() => {
    if (!isLoading && user && !user.isAdmin) {
      router.replace("/home");
    }
  }, [isLoading, user, router]);

  if (isLoading || !user?.isAdmin) return null; // fail-closed while loading / redirecting
  return <>{children}</>;
}

// Customer-owner gate (specs/065): "Cài đặt › Người dùng" is for a non-admin owner (/auth/me canManageUsers).
// An admin has no business here either (they have /admin/users), so anyone else lands on /home with a toast.
// Fail-closed while /auth/me loads; the backend enforces the real gate per request regardless.
export function RequireUserManager({ children }: { children: React.ReactNode }) {
  const { user, isLoading } = useAuth();
  const router = useRouter();
  const allowed = !!user?.canManageUsers;

  useEffect(() => {
    if (!isLoading && user && !allowed) {
      toast.info(t("settings.users.forbidden"));
      router.replace("/home");
    }
  }, [isLoading, user, allowed, router]);

  if (isLoading || !user || !allowed) return null;
  return <>{children}</>;
}

// Operator-only gate (EASE Nước 6, ../specs/034-customer-admin-tier/spec.md §4): the Studio/Builder face is
// the Nexus FACTORY — it requires the platform-operator flag, not mere tenant-admin. A customer's
// admin landing here (old bookmark, typed URL) gets an HONEST explanation + redirect instead of
// the silent boomerang the felt-ease audit flagged. Fail-closed while /auth/me is loading.
// The flag arrives via GET /auth/me (isPlatformOperator) — the backend enforces the real gate
// per request regardless of what this renders.
export function RequireOperator({ children }: { children: React.ReactNode }) {
  const { user, isLoading } = useAuth();
  const router = useRouter();
  const isOperator = !!user?.isPlatformOperator;

  useEffect(() => {
    if (!isLoading && user && !isOperator) {
      toast.info("Khu vực xưởng Nexus — tài khoản của bạn quản trị vận hành (người dùng, phân quyền, dữ liệu) trong app.");
      router.replace("/home");
    }
  }, [isLoading, user, isOperator, router]);

  if (isLoading || !user || !isOperator) return null; // fail-closed while loading / redirecting
  return <>{children}</>;
}
