"use client";

// LayoutDetailScreen — pure content: mounts LayoutDetail + wires edit / clone / after-delete
// navigation ("back to list" is the shell's up button + breadcrumb). Chrome is provided by the
// (app) route-group's AppShell (E3 W3a). The route ([entity]/[id]/page.tsx) is a thin Server Component that awaits params and
// renders this. Navigation is CLIENT-SIDE (F7 SPA-ify) via the guarded router (lib/
// navigation-blocker, same primitive as Topbar) — no full document reload per hop.

import { useGuardedRouter } from "@/lib/navigation-blocker";
import { LayoutDetail } from "@/components/layout/LayoutDetail";

export function LayoutDetailScreen({ entity, recordId }: { entity: string; recordId: string }) {
  const router = useGuardedRouter();

  const goList = () => router.push(`/${entity}`);
  const goEdit = () => router.push(`/${entity}/${recordId}/edit`);
  // §7.1 clone: open a NEW form prefilled from this record.
  const goClone = () => router.push(`/${entity}/new?cloneFrom=${encodeURIComponent(recordId)}`);

  return (
    <LayoutDetail
      entity={entity}
      recordId={recordId}
      onEdit={goEdit}
      onClone={goClone}
      onDeleted={goList}
    />
  );
}
