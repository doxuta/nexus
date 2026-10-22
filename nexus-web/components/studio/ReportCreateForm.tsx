"use client";

// ReportCreateForm (specs/074 FR-006, lát f2-report-list-view): "+ Báo cáo mới" in place — a shaded strip, never a
// dialog. Split out of ReportsScreen's create dialog and kept in step with it: name + table, type tabular with at
// least one column (BE REPORT_CONFIG_INVALID otherwise), so the report draws something the moment it exists.
import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { createReport, listFields, type ReportSummary } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { entityOptionText, useEntityOptions } from "@/lib/entityOptions";
import { errorMessage } from "@/lib/errorMessages";
import { t } from "@/lib/i18n";
import { useEntities } from "@/lib/metadata/hooks";
import { qk } from "@/lib/query/keys";
import { buildReportConfig } from "@/lib/reportConfig";
import { scopeEntityOptions } from "@/lib/appScope";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";

export function ReportCreateForm({
  entity, entityNames, onCreated, onCancel,
}: {
  /** Default table (?bang=). */
  entity?: string;
  /** Tables of the app in scope; null = all. A new report can only be born on one of them. */
  entityNames?: Set<string> | null;
  onCreated: (created: ReportSummary) => void;
  onCancel: () => void;
}) {
  const qc = useQueryClient();
  const { data: all = [] } = useEntities();
  const entities = scopeEntityOptions(all, entityNames ?? null, entity);
  const choices = useEntityOptions(entities).map((o) => ({ value: o.value, label: entityOptionText(o) }));
  const [name, setName] = useState("");
  const [picked, setPicked] = useState("");
  const chosen = picked || entity || entities[0]?.name || "";
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const can = Boolean(name.trim() && chosen && !busy);

  async function submit() {
    if (!can) return;
    setBusy(true);
    setErr(null);
    try {
      const flds = await listFields(chosen);
      const created = await createReport({
        name: name.trim(),
        entityName: chosen,
        type: "tabular",
        config: buildReportConfig({ type: "tabular", columns: [], metric: "count", fields: flds, fallbackColumn: flds[0]?.name ?? "id" }),
        isActive: true,
      });
      emitDraftsChanged("report"); // a report draft on a delivered project: the header recounts
      await Promise.all([
        qc.invalidateQueries({ queryKey: qk.adminReports() }),
        qc.invalidateQueries({ queryKey: qk.reports() }),
      ]);
      onCreated(created);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form
      className="slds-box slds-theme_shade slds-m-bottom_small"
      aria-label={t("studio.build.reports.createTitle")}
      onSubmit={(e) => { e.preventDefault(); void submit(); }}
    >
      <div className="slds-form flex flex-col gap-3">
        <Input
          autoFocus
          label={t("page.reports.createName")}
          value={name}
          placeholder={t("page.reports.createNamePlaceholder")}
          onChange={(e) => setName(e.target.value)}
        />
        <Select
          label={t("page.reports.createEntity")}
          value={chosen}
          onChange={(e) => setPicked(e.target.value)}
          placeholder={entities.length === 0 ? t("page.reports.createNoEntity") : false}
          options={choices}
        />
        {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
        <div className="flex gap-2">
          <Button variant="brand" type="submit" disabled={!can}>
            {busy ? t("page.reports.creating") : t("studio.build.reports.createSubmit")}
          </Button>
          <Button type="button" onClick={onCancel} disabled={busy}>{t("action.cancel")}</Button>
        </div>
      </div>
    </form>
  );
}
