"use client";

// (app) route group layout — authenticated shell with sidebar + topbar.
// RequireAuth redirects to /login if session is absent.
// Sidebar renders dynamically from GET /metadata/entities + permissions.
import { RequireAuth } from "@/lib/auth/guards";
import { AppShell } from "@/components/shell/AppShell";

export default function AppLayout({ children }: { children: React.ReactNode }) {
  return (
    <RequireAuth>
      <AppShell>{children}</AppShell>
    </RequireAuth>
  );
}
