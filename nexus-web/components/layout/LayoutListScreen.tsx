"use client";

// LayoutListScreen — pure content: mounts LayoutList + wires row-open (→ detail) + create
// navigation. Chrome (Sidebar/Topbar) is provided by the (app) route-group's AppShell (E3 W3a) —
// this component renders no shell of its own. The route ([entity]/page.tsx) is a thin Server
// Component that awaits params and renders this. Navigation is CLIENT-SIDE (F7 SPA-ify): the
// guarded router (lib/navigation-blocker, same primitive as Topbar) pushes without a full
// document reload and still honours the unsaved-changes confirm when a dirty form is mounted.
// "Quick create" uses QuickCreateModal (SLDS modal) instead of a full page navigation. The list's
// header links (import, batch create, recycle bin) go through the same guarded router.

import { useState } from "react";
import { LayoutList } from "@/components/layout/LayoutList";
import { QuickCreateModal } from "@/components/layout/QuickCreateModal";
import { useGuardedRouter } from "@/lib/navigation-blocker";

export function LayoutListScreen({ entity }: { entity: string }) {
  const [quickOpen, setQuickOpen] = useState(false);
  const router = useGuardedRouter();

  const openRecord = (id: string) => router.push(`/${entity}/${id}`);
  // onCreate is wired to the quick-create slide-over (not a full page).
  const handleCreate = () => setQuickOpen(true);
  const handleSaved = (id: string) => {
    setQuickOpen(false);
    router.push(`/${entity}/${id}`);
  };

  return (
    <>
      <LayoutList entity={entity} onOpen={openRecord} onCreate={handleCreate} onNavigate={router.push} />

      {/* Quick create — opens from the list header's "Tạo mới" */}
      <QuickCreateModal
        entity={entity}
        open={quickOpen}
        onClose={() => setQuickOpen(false)}
        onSaved={handleSaved}
      />
    </>
  );
}
