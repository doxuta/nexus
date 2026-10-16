"use client";

// Vận hành › Dữ liệu nhận vào (specs/078 lát u7-cong-nhan-du-lieu-vao, US7 AC6, FR-076).
// One flat table of what the inbound gates received. The received body (`payloadHead`) is NEVER drawn.
import type { ColumnDef } from "@tanstack/react-table";
import Link from "next/link";
import { useMemo } from "react";
import { DataTable, fill } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { ExpandableSection } from "@/components/slds/ExpandableSection";
import { QueryState } from "@/components/studio/QueryState";
import { StatusText } from "@/components/studio/StatusText";
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { listHookDeliveries, listInboundHooks, type HookDelivery } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";

// Measured 2026-10-14 (T130, inbound_hook_service.go): `error` is not only fixed sentences. The payment-match reason
// quotes the received memo (`memo %q không chứa mã khớp`) and record validation errors can echo a value, so the
// server text may contain received data. Re-measure (send a bad body, read `error`) before turning this on.
export const SHOW_RAW_DELIVERY_ERROR = false;

const MAX_HOOKS = 10; // ponytail: 10 first gates, GET /admin/inbound-hooks/deliveries gộp nếu >10 (spec N10)

type Row = HookDelivery & { hookName: string };

function reason(d: HookDelivery): string {
  if (d.status === "error") return t("studio.operate.inbound.reason.error");
  if (d.matchStatus === "unmatched") return t("studio.operate.inbound.reason.unmatched");
  return "";
}

export function InboundDeliveriesPage({ slug, cong, showRaw = SHOW_RAW_DELIVERY_ERROR }: { slug: string; cong?: string; showRaw?: boolean }) {
  const q = useProjectQuery(slug, ["operate", "inbound", cong ?? "all"], async () => {
    const hooks = await listInboundHooks();
    const picked = cong ? hooks.filter((h) => h.id === cong) : hooks.slice(0, MAX_HOOKS);
    const lists = await Promise.all(picked.map((h) => listHookDeliveries(h.id)));
    const rows: Row[] = picked
      .flatMap((h, i) => lists[i].map((d) => ({ ...d, hookName: h.name })))
      .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
    return { hookCount: hooks.length, picked: picked.length, rows };
  });

  const columns = useMemo<ColumnDef<Row>[]>(
    () => [
      { id: "when", header: t("studio.operate.auto.col.when"), cell: ({ row }) => <span className="tabular-nums">{formatDateTime(row.original.createdAt)}</span> },
      { id: "hook", header: t("studio.operate.inbound.col.hook"), cell: ({ row }) => row.original.hookName },
      {
        id: "result",
        header: t("studio.operate.auto.col.result"),
        cell: ({ row: { original: d } }) =>
          d.status === "error" ? (
            <StatusText tone="error">{t("studio.operate.inbound.status.error")}</StatusText>
          ) : (
            <StatusText tone="success">{t(d.status === "created" ? "studio.operate.inbound.status.created" : "studio.operate.inbound.status.updated")}</StatusText>
          ),
      },
      {
        id: "reason",
        header: t("studio.operate.inbound.col.reason"),
        cell: ({ row: { original: d } }) => (
          <>
            {reason(d)}
            {showRaw && d.error && (
              <ExpandableSection title={t("studio.operate.inbound.detail")} defaultOpen={false}>
                <span style={{ whiteSpace: "pre-wrap" }}>{d.error}</span>
              </ExpandableSection>
            )}
          </>
        ),
      },
    ],
    [showRaw],
  );

  const manage = studioHref(slug, "dung/tu-dong/cong-nhan");
  return (
    <div className="slds-p-around_medium">
      <h1 className="slds-text-heading_medium">{t("studio.route.operInbound")}</h1>
      <QueryState query={q}>
        {q.data && q.data.hookCount === 0 ? (
          <EmptyState plain title={t("studio.operate.inbound.none")} action={<Link href={manage}>{t("studio.operate.inbound.create")}</Link>} />
        ) : (
          q.data && (
            <>
              {!cong && q.data.hookCount > MAX_HOOKS && <p className="slds-text-color_weak slds-m-vertical_small">{t("studio.operate.inbound.capped")}</p>}
              {q.data.rows.length === 0 ? (
                <p className="slds-text-color_weak slds-m-top_small">{t("studio.operate.inbound.never")}</p>
              ) : (
                <div className="slds-m-top_small">
                  <DataTable label={fill(t("studio.operate.inbound.label"), {})} columns={columns} data={q.data.rows} />
                </div>
              )}
              <p className="slds-m-top_small">
                <Link href={manage}>{t("studio.operate.inbound.manage")}</Link>
              </p>
            </>
          )
        )}
      </QueryState>
    </div>
  );
}
