"use client";

// LayoutFormScreen — pure content: mounts LayoutForm + wires save/cancel navigation. Chrome
// (Sidebar/Topbar) is provided by the (app) route-group's AppShell (E3 W3a). Routes (new /
// [id]/edit) are thin Server Components that await params and render this.
//
// Navigation is CLIENT-SIDE (F7 SPA-ify) via the PLAIN next/navigation router on purpose:
// LayoutForm already runs the unsaved-changes confirm itself (guardedCancel before onCancel;
// post-save the blocker is disarmed) — the guarded router here would double-prompt.
//
// After a successful save, go to the RECORD'S DETAIL (create AND edit) — the same landing
// quick-create uses (F7 finding 7: the two create paths must not end in different places).
// "Lưu & tạo mới" is unaffected: it never calls onSaved (stays on the form for rapid entry).

import { useRouter } from "next/navigation";
import { LayoutForm } from "@/components/layout/LayoutForm";

export function LayoutFormScreen({ entity, recordId }: { entity: string; recordId?: string }) {
  const router = useRouter();

  const goDetail = (id: string) => router.push(`/${entity}/${id}`);
  // Cancel returns to where the user came from (list/detail) — history back, list fallback.
  const goBack = () => {
    if (window.history.length > 1) router.back();
    else router.push(`/${entity}`);
  };

  return (
    <LayoutForm
      entity={entity}
      recordId={recordId}
      onSaved={goDetail}
      onCancel={goBack}
    />
  );
}
