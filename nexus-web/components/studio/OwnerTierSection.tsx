"use client";

// Project-wide customer owner tier (specs/078 u7-chu-khach-cai-dat, 065 FR-033/FR-042). One group inside the customer
// project settings. `tenantId` is the ledger row's id handed in by ProjectSettings, never read from the URL or a field.
// The two counts come from ONE GET /admin/tenants (only mounted on this page, no N+1); that route is operator-only, so
// loading / error (403) / no row for the id ⇒ the group draws nothing. The Nexus workshop never reaches this component
// (ProjectSettings mounts it only on the customer branch; the engine also refuses with TENANT_SELF_OWNER_TIER).
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { toast } from "@/components/ui/Toaster";
import { ApiError, listTenants, revertOwnerTier, setOwnerTier } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { InlineConfirm } from "./InlineConfirm";

const TENANTS_KEY = ["studio", "tenants"];

function failure(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.code === "TENANT_NOT_FOUND") return t("studio.operate.ownerTier.notFound");
    if (e.status === 403) return t("studio.operate.ownerTier.forbidden");
    return errorMessage(e);
  }
  return t("studio.operate.ownerTier.failed");
}

export function OwnerTierSection({ tenantId, name }: { tenantId: string; name: string }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: TENANTS_KEY, queryFn: listTenants, staleTime: 30_000, retry: false });
  const [asking, setAsking] = useState<"on" | "off" | null>(null);
  // Give focus back to the button row when a confirm closes, so keyboard users are not dropped to <body>.
  const row = useRef<HTMLDivElement>(null);
  const lastAsked = useRef<"on" | "off" | null>(null);
  useEffect(() => {
    if (asking !== null) { lastAsked.current = asking; return; }
    if (lastAsked.current === null) return;
    // Prefer the button that opened the confirm; fall back to the row itself when that button is gone after the refetch.
    const r = row.current;
    (r?.querySelector<HTMLElement>(`[data-tier="${lastAsked.current}"]`) ?? r)?.focus();
    lastAsked.current = null;
  }, [asking]);
  // Close the confirm only after the counts refetched, so stale buttons never reappear (mutation stays pending until then).
  const done = async () => {
    await Promise.all([
      qc.invalidateQueries({ queryKey: TENANTS_KEY }),
      qc.invalidateQueries({ queryKey: ["studio", "projects"] }),
    ]);
    setAsking(null);
  };
  const on = useMutation({
    mutationFn: () => setOwnerTier(tenantId),
    onSuccess: (r) => {
      toast.success(tf("studio.operate.ownerTier.onDone", { n: r.converted, m: r.skippedWithRole }));
      if (r.revokeFailed > 0) toast.error(tf("studio.operate.ownerTier.revokeFailed", { n: r.revokeFailed }));
      return done();
    },
  });
  const off = useMutation({
    mutationFn: () => revertOwnerTier(tenantId),
    onSuccess: (r) => {
      toast.success(tf("studio.operate.ownerTier.offDone", { n: r.reverted }));
      if (r.revokeFailed > 0) toast.error(tf("studio.operate.ownerTier.revokeFailed", { n: r.revokeFailed }));
      return done();
    },
  });

  const tn = q.data?.find((x) => x.id === tenantId);
  if (!tn) return null;
  const err = on.error ?? off.error;
  return (
    <section className="slds-m-top_large">
      <h3 className="slds-text-title_caps slds-border_bottom slds-p-bottom_x-small" style={{ fontSize: "13px", fontWeight: 600, textTransform: "none" }}>
        {t("studio.operate.ownerTier.title")}
      </h3>
      <div className="slds-m-top_small">
        <p aria-live="polite">{tf("studio.operate.ownerTier.counts", { usable: tn.usableOwnerCount, convertible: tn.convertibleAdminCount })}</p>
        {asking === "on" ? (
          <div className="slds-m-top_small">
            <InlineConfirm
              tone="warning"
              message={tf("studio.operate.ownerTier.onMessage", { n: tn.convertibleAdminCount, name })}
              confirmLabel={t("studio.operate.ownerTier.onConfirm")}
              busy={on.isPending}
              onConfirm={() => on.mutate()}
              onCancel={() => setAsking(null)}
            />
          </div>
        ) : asking === "off" ? (
          <div className="slds-m-top_small">
            <InlineConfirm
              tone="warning"
              message={tf("studio.operate.ownerTier.offMessage", { n: tn.usableOwnerCount, name })}
              confirmLabel={t("studio.operate.ownerTier.offConfirm")}
              busy={off.isPending}
              onConfirm={() => off.mutate()}
              onCancel={() => setAsking(null)}
            />
          </div>
        ) : (
          <div ref={row} tabIndex={-1} className="slds-m-top_small flex gap-2">
            {tn.convertibleAdminCount > 0 && (
              <Button variant="neutral" data-tier="on" onClick={() => { on.reset(); off.reset(); setAsking("on"); }}>{t("studio.operate.ownerTier.on")}</Button>
            )}
            {tn.usableOwnerCount > 0 && (
              <Button variant="neutral" data-tier="off" onClick={() => { on.reset(); off.reset(); setAsking("off"); }}>{t("studio.operate.ownerTier.off")}</Button>
            )}
          </div>
        )}
        {err ? <div className="slds-m-top_small"><ScopedNotification theme="error">{failure(err)}</ScopedNotification></div> : null}
      </div>
    </section>
  );
}
