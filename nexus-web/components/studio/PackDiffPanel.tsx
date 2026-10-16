"use client";

// PackDiffPanel — "Đối chiếu" (specs/011-pack-provenance §4.4, specs/079 f7-nang-cap-hien-co).
// Read-only report: for every item the installed pack shipped, whether the project still matches it. Groups by item
// type under a plain heading; the four honest states are StatusText (never a Badge). Raw drift paths sit behind a
// per-row "Chi tiết" button; an untracked install gets one honest sentence.
import { useEffect, useState } from "react";
import { ApiError, diffPackInstall, type PackDiffItem, type PackDiffReport } from "@/lib/api";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Button } from "@/components/slds/Button";
import { fill } from "@/components/slds/DataTable";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { StatusText, type StatusTone } from "./StatusText";

// Shared with PackUpgradePanel (field / role_grant are the upgrade plan's expanded sub-items).
export const TYPE_LABELS: Record<string, I18nKey> = {
  field: "page.packDiff.type.field",
  role_grant: "page.packDiff.type.role_grant",
  field_acl: "page.packDiff.type.field_acl",
  entity: "page.packDiff.type.entity",
  deferred_field: "page.packDiff.type.deferred_field",
  layout: "page.packDiff.type.layout",
  role: "page.packDiff.type.role",
  nn_relationship: "page.packDiff.type.nn_relationship",
  workflow: "page.packDiff.type.workflow",
  report: "page.packDiff.type.report",
  dashboard: "page.packDiff.type.dashboard",
  team: "page.packDiff.type.team",
  work_queue: "page.packDiff.type.work_queue",
  approval: "page.packDiff.type.approval",
  inbound_hook: "page.packDiff.type.inbound_hook",
};

const STATUS_META: Record<PackDiffItem["status"], { label: I18nKey; tone: StatusTone }> = {
  unchanged: { label: "page.packDiff.status.unchanged", tone: "success" },
  modified: { label: "page.packDiff.status.modified", tone: "warn" },
  missing: { label: "page.packDiff.status.missing", tone: "error" },
  undiffable: { label: "page.packDiff.status.undiffable", tone: "weak" },
};

function DiffRow({ it }: { it: PackDiffItem }) {
  const [open, setOpen] = useState(false);
  const meta = STATUS_META[it.status] ?? STATUS_META.undiffable;
  const paths = it.status === "modified" ? (it.changedPaths ?? []) : [];
  const label = paths.length > 0 ? `${t(meta.label)} · ${fill(t("page.packDiff.deviates"), { n: paths.length })}` : t(meta.label);
  return (
    <li className="slds-item">
      <div className="flex flex-wrap items-baseline gap-3">
        <span className="min-w-0 break-all font-semibold">{it.itemKey}</span>
        <StatusText tone={meta.tone}>{label}</StatusText>
        {it.protected && <StatusText tone="weak">{t("page.packDiff.protected")}</StatusText>}
        {paths.length > 0 && (
          <Button variant="neutral" className="ml-auto" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
            {t(open ? "page.packDiff.detailHide" : "page.packDiff.detail")}
          </Button>
        )}
      </div>
      {open && (
        <p className="slds-text-body_small slds-text-color_weak slds-truncate" title={paths.join(", ")}>
          {fill(t("page.packDiff.changedAt"), { paths: paths.slice(0, 4).join(", ") + (paths.length > 4 ? "…" : "") })}
        </p>
      )}
    </li>
  );
}

export function PackDiffPanel({ slug }: { slug: string }) {
  const [report, setReport] = useState<PackDiffReport | null>(null);
  const [err, setErr] = useState("");
  useEffect(() => {
    let alive = true;
    diffPackInstall(slug)
      .then((r) => { if (alive) { setReport(r); setErr(""); } })
      .catch((e) => { if (alive) setErr(e instanceof ApiError ? e.message : t("page.packDiff.failed")); });
    return () => { alive = false; };
  }, [slug]);

  if (err) return <ScopedNotification theme="error">{err}</ScopedNotification>;
  if (!report) {
    return (
      <div className="flex items-center gap-2">
        <Spinner size="x-small" inline />
        <span className="slds-text-color_weak">{t("page.packDiff.loading")}</span>
      </div>
    );
  }
  if (report.untracked) return <p className="slds-text-color_weak">{t("page.packDiff.untracked")}</p>;

  const groups = new Map<string, PackDiffItem[]>();
  for (const it of report.items) {
    const g = groups.get(it.itemType) ?? [];
    g.push(it);
    groups.set(it.itemType, g);
  }
  const same = report.items.filter((i) => i.status === "unchanged").length;
  const versions = report.versions.join(", v");

  return (
    <div className="flex flex-col gap-3">
      <p className="slds-text-body_small slds-text-color_weak">
        {same === report.items.length
          ? fill(t("page.packDiff.allSame"), { n: report.items.length, versions })
          : fill(t("page.packDiff.someChanged"), { same, n: report.items.length, versions })}
      </p>
      {[...groups.entries()].map(([type, items]) => (
        <div key={type}>
          <h3 className="font-semibold">{TYPE_LABELS[type] ? t(TYPE_LABELS[type]) : type}</h3>
          <ul className="slds-has-dividers_bottom">
            {items.map((it) => <DiffRow key={type + "/" + it.itemKey} it={it} />)}
          </ul>
        </div>
      ))}
    </div>
  );
}
