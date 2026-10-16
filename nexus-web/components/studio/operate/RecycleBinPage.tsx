"use client";

// Vận hành › Dữ liệu › <bảng> › Thùng rác (specs/078 lát u7-du-lieu-thung-rac-xoa-han, FR-058).
// The customer's own recycle bin on the work surface. This page is the only place that asks /auth/me whether to offer
// "Xoá vĩnh viễn", so the customer-face screen stays free of Studio hooks.
// ponytail: hiding the action by canPurge is UX only; the real gate is the server (RequireAdmin + RequireBuilderTier on the
// purge group, router.go), which answers 403 to anyone else.
import { RecycleBinScreen } from "@/components/screens/RecycleBinScreen";
import { canPurge } from "@/lib/operateData";
import { useStudioIdentity } from "@/lib/studioContext";
import { useScopeGate } from "./useScopeGate";

export function OperateRecycleBin({ bang }: { slug: string; bang: string }) {
  const identity = useStudioIdentity();
  const guard = useScopeGate(true);
  const allowed = canPurge({ status: identity.status === "ready" ? "ok" : identity.status, user: identity.me, workspace: identity.me?.workspace });
  return <RecycleBinScreen entity={bang} purge={{ allowed, assertScope: () => guard.assertScope?.() ?? false }} />;
}
