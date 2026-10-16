"use client";

// "Xuất thành tệp gói mẫu" (specs/079 FR-045, f7-xuat-goi): pick tables (or an app's menu tables), download the JSON pack.
// In place: no card, no dialog. The app scope travels in the call itself (appSlug/appName/appIcon), never from
// useStudioScope. With "Kèm bản ghi" on in a customer project the click first asks in place (InlineConfirm) because
// the file then carries the customer's real rows. All state lives under the page's key={S}, so a project switch
// resets it. The Blob is revoked right after the click and never kept in a cache or storage.
import { useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { DataTable, fill } from "@/components/slds/DataTable";
import { FormElement } from "@/components/slds/FormElement";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { adminApp, adminApps, exportVertical, listEntities, type EntityMeta } from "@/lib/api";
import { applySelection, packFileName, preselectEntities } from "@/lib/handoverWizard";
import { t } from "@/lib/i18n";
import { maintainErrorText } from "@/lib/maintainErrors";
import { homeSlug } from "@/lib/studioContext";
import { InlineConfirm } from "../InlineConfirm";
import { QueryState } from "../QueryState";
import { useProjectQuery } from "../useProjectQuery";

// key={project}: state (includeData, the open confirm, the picked tables) never crosses to another customer.
export function PackExportForm({ project }: { project: string }) {
  return <PackExportFormInner key={project} project={project} />;
}

function PackExportFormInner({ project }: { project: string }) {
  const entities = useProjectQuery(project, ["export", "entities"], listEntities);
  const apps = useProjectQuery(project, ["export", "apps"], adminApps);
  const [appId, setAppId] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [includeData, setIncludeData] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const [warnings, setWarnings] = useState<string[]>([]);
  const runBtn = useRef<HTMLButtonElement>(null);
  const refocus = useRef(false); // FR-011a: after the confirm closes or a run ends, focus returns to "Xuất", never to body
  const pick = useRef(0); // a late app answer must not overwrite a newer choice or a hand edit
  const list = entities.data ?? [];
  const app = (apps.data ?? []).find((a) => a.id === appId);
  const columns = useMemo(() => [
    { id: "label", header: t("studio.maintain.export.colLabel"), accessorFn: (e: EntityMeta) => e.label ?? e.name },
    { id: "name", header: t("studio.maintain.export.colName"), accessorKey: "name" as const },
  ], []);

  useEffect(() => {
    if (refocus.current && !confirming && !busy) { refocus.current = false; runBtn.current?.focus(); }
  }, [confirming, busy]);

  const reset = () => { setDone(false); setProblem(null); setWarnings([]); setConfirming(false); };

  async function chooseApp(id: string) {
    const mine = ++pick.current;
    setAppId(id);
    reset();
    if (!id) return;
    try {
      // The app's menu rows name tables by id; the export call wants table names.
      const nav = (await adminApp(id)).items.filter((i) => i.itemType === "entity").map((i) => i.refId);
      if (mine !== pick.current) return;
      const names = list.filter((e) => nav.includes(e.id ?? "") || nav.includes(e.name)).map((e) => e.name);
      setSelected(preselectEntities(names, list));
    } catch (e) {
      if (mine !== pick.current) return;
      setAppId(""); // identity must match the tables: no app, no app identity in the file
      setProblem(maintainErrorText(e));
    }
  }

  async function run() {
    refocus.current = true;
    setConfirming(false); setBusy(true); setDone(false); setProblem(null); setWarnings([]);
    try {
      const identity = app ? { slug: app.name, name: app.label, icon: app.icon ?? undefined } : undefined;
      const { pack, warnings: w } = await exportVertical(selected, includeData, identity);
      setWarnings(w);
      const url = URL.createObjectURL(new Blob([JSON.stringify(pack, null, 2)], { type: "application/json" }));
      const a = document.createElement("a");
      a.href = url;
      const file = packFileName(identity?.slug, false);
      a.download = includeData ? file.replace(/\.json$/, "-co-du-lieu.json") : file;
      a.click();
      URL.revokeObjectURL(url);
      setDone(true);
    } catch (e) {
      setProblem(maintainErrorText(e));
    } finally {
      setBusy(false);
    }
  }

  // Real rows of a customer's project are the only ones that need the extra question; the workshop's own are Nexus's.
  const onExport = () => (includeData && project !== homeSlug() ? setConfirming(true) : void run());

  return (
    <section className="slds-m-top_x-large" aria-labelledby="pack-export-h">
      <h2 id="pack-export-h" className="slds-text-heading_small slds-border_bottom slds-p-bottom_x-small">{t("studio.maintain.export.title")}</h2>
      <p className="nx-lede slds-m-top_x-small">{t("studio.maintain.export.intro")}</p>
      <div className="slds-theme_shade slds-p-around_medium slds-m-top_small flex flex-col gap-3">
        <Select
          label={t("studio.maintain.export.app")}
          placeholder={t("studio.maintain.export.appNone")}
          value={appId}
          disabled={!entities.data}
          onChange={(e) => void chooseApp(e.target.value)}
          options={(apps.data ?? []).map((a) => ({ value: a.id, label: a.label }))}
        />
        {apps.isError && <ScopedNotification theme="error" role="alert">{maintainErrorText(apps.error)}</ScopedNotification>}
        {app && <p className="slds-text-body_small slds-text-color_weak">{fill(t("studio.maintain.export.appNote"), { label: app.label })}</p>}
        <FormElement label={t("studio.maintain.export.tables")}>
          <QueryState query={entities} empty={<p className="slds-text-color_weak">{t("studio.maintain.export.noTables")}</p>}>
            <DataTable
              label={t("studio.maintain.export.tables")}
              columns={columns}
              data={list}
              getRowId={(e) => e.name}
              selectable
              rowSelection={Object.fromEntries(selected.map((n) => [n, true]))}
              onRowSelectionChange={(sel) => { pick.current++; setSelected((prev) => applySelection(prev, sel)); reset(); }}
              maxHeight="15rem"
              bordered
            />
          </QueryState>
        </FormElement>
        <Checkbox
          label={t("studio.maintain.export.includeData")}
          checked={includeData}
          onChange={(e) => { setIncludeData(e.target.checked); reset(); }}
        />
        <div aria-live="polite">{includeData && <ScopedNotification theme="warning">{t("studio.maintain.export.dataWarn")}</ScopedNotification>}</div>
        {confirming ? (
          <InlineConfirm
            tone="warning"
            message={fill(t("studio.maintain.export.confirm"), { name: project })}
            confirmLabel={t("studio.maintain.export.confirmRun")}
            onConfirm={() => void run()}
            onCancel={() => { refocus.current = true; setConfirming(false); }}
          />
        ) : (
          <div>
            <Button ref={runBtn} variant="brand" disabled={busy || selected.length === 0} onClick={onExport}>
              {busy ? t("studio.maintain.export.running") : t("studio.maintain.export.run")}
            </Button>
          </div>
        )}
        <p role="status">{done ? t("studio.maintain.export.done") : ""}</p>
        {problem && <ScopedNotification theme="error" role="alert">{problem}</ScopedNotification>}
        {done && warnings.length > 0 && (
          <div>
            <ScopedNotification theme="warning"><strong>{fill(t("studio.maintain.export.excluded"), { n: warnings.length })}</strong></ScopedNotification>
            <ul className="slds-list_dotted slds-m-top_x-small">{warnings.map((w, i) => <li key={i}>{w}</li>)}</ul>
          </div>
        )}
      </div>
    </section>
  );
}
