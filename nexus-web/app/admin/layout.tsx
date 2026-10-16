"use client";

// Admin area layout — /admin/* renders inside the SAME AppShell as the (app) route group
// (Phase 4 IA cleanup, ../specs/010-app-container/spec.md §7): one App face, no separately-mounted
// Sidebar/Topbar copy. AppShell derives the "Quản trị / …" crumbs + admin search placeholder
// from the pathname (see ADMIN_LABELS there).
// SLDS Setup frame (specs/038 T047): the Setup column (vertical-navigation) on the left, the page on
// the right, each scrolling on its own. Below md the column is hidden — the header's Setup gear
// lists the same pages.
// W0-QW1 [SECURITY] kept: fail-closed RequireAuth → RequireAdmin. Non-admins → /home.
import { RequireAuth, RequireAdmin } from "@/lib/auth/guards";
import { AppShell } from "@/components/shell/AppShell";
import { SetupNav } from "@/components/shell/SetupNav";

export default function AdminLayout({ children }: { children: React.ReactNode }) {
  return (
    <RequireAuth>
      <RequireAdmin>
        <AppShell>
          <div className="flex min-h-0 flex-1">
            <aside className="hidden w-60 shrink-0 overflow-y-auto slds-border_right md:block print:hidden">
              <SetupNav className="py-2" />
            </aside>
            <div className="flex min-h-0 min-w-0 flex-1 flex-col overflow-y-auto">{children}</div>
          </div>
        </AppShell>
      </RequireAdmin>
    </RequireAuth>
  );
}
