"use client";

// RelatedListSettings (specs/075 T151): "Cách hiện danh sách liên quan" — how a child list shows on the parent record's
// Quan hệ tab. One copy, used by the relation map (and later by the screen editor). Saves through
// PUT /admin/relationships/:id/view-config (setRelationshipViewConfig); absent keys mean the default rendering, so
// only opt-outs are written (allowCreate:false / allowLink:false), never `true`.
import { useState } from "react";
import { useDirectApplyGate } from "@/components/studio/useDirectApplyGate";
import { ApiError, setRelationshipViewConfig, type FieldMeta, type RelViewConfig } from "@/lib/api";
import { errorMessage, strictErrorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { Input } from "@/components/slds/Input";
import { Select } from "@/components/slds/Select";

export interface RelatedListSettingsProps {
  slug: string;
  relId: string;
  relType: "1-N" | "N-N" | "1-1";
  parentLabel: string;
  childLabel: string;
  childFields: FieldMeta[];
  initial?: RelViewConfig;
  onSaved?: (relId: string, cfg: RelViewConfig | null) => void;
  /** The relationship no longer exists (PUT 404): the caller closes the group. */
  onGone?: () => void;
}

export function RelatedListSettings({ slug, relId, relType, parentLabel, childLabel, childFields, initial, onSaved, onGone }: RelatedListSettingsProps) {
  const [label, setLabel] = useState(initial?.label ?? "");
  const [order, setOrder] = useState(initial?.order != null ? String(initial.order) : "");
  const [columns, setColumns] = useState<string[]>(initial?.columns ?? []);
  const [sortField, setSortField] = useState(initial?.sort?.field ?? "");
  const [sortDir, setSortDir] = useState<"asc" | "desc">(initial?.sort?.dir === "desc" ? "desc" : "asc");
  // Absent means ALLOWED on the reading side (LayoutDetail: `vc?.allowCreate ?? true`).
  const [allowCreate, setAllowCreate] = useState(initial?.allowCreate !== false);
  const [allowLink, setAllowLink] = useState(initial?.allowLink !== false);
  const [busy, setBusy] = useState(false);
  // server refusals land under the field they are about (RELVIEW_*), anything else in a toast
  const [errs, setErrs] = useState<Partial<Record<"label" | "columns" | "sort", string>>>({});
  const [gone, setGone] = useState(false);
  // a relationship is never drafted: on a delivered project the save applies at once, with no history (specs/075 T196)
  const gate = useDirectApplyGate(slug, "relationship", {
    message: tf("studio.build.screens.draft.applyNow", { what: t("studio.build.screens.draft.relDisplay") }),
    confirm: t("studio.build.screens.draft.applyNowGo"),
  });

  // Mirror of the server's sortable allowlist, so a save cannot hit RELVIEW_SORT_NOT_SORTABLE.
  const sortable = childFields.filter((f) => f.isUnique || f.fieldType === "relation" || f.fieldType === "user" || f.options?.indexed === true);
  const sortOptions = [
    ...sortable.map((f) => ({ value: f.name, label: f.label ?? f.name })),
    { value: "created_at", label: t("studio.build.relMap.display.sortCreated") },
    { value: "updated_at", label: t("studio.build.relMap.display.sortUpdated") },
    { value: "id", label: t("studio.build.relMap.display.sortId") },
  ];

  const toggleColumn = (name: string) => setColumns((cs) => (cs.includes(name) ? cs.filter((c) => c !== name) : [...cs, name]));

  const build = (): RelViewConfig => {
    const cfg: RelViewConfig = {};
    if (label.trim()) cfg.label = label.trim();
    if (order.trim() !== "" && Number.isInteger(Number(order))) cfg.order = Number(order);
    if (columns.length > 0) cfg.columns = columns;
    if (sortField) cfg.sort = { field: sortField, dir: sortDir };
    if (relType !== "N-N" && !allowCreate) cfg.allowCreate = false;
    if (relType === "N-N" && !allowLink) cfg.allowLink = false;
    return cfg;
  };

  const save = async (reset: boolean) => {
    setBusy(true);
    setErrs({});
    try {
      const cfg = reset ? null : build();
      const stored = await setRelationshipViewConfig(relId, cfg && Object.keys(cfg).length > 0 ? cfg : null);
      onSaved?.(relId, stored);
      toast.success(t(reset || !stored ? "studio.build.relMap.display.resetDone" : "studio.build.relMap.display.saved"));
      if (reset) {
        setLabel(""); setOrder(""); setColumns([]); setSortField(""); setSortDir("asc"); setAllowCreate(true); setAllowLink(true);
      }
    } catch (e) {
      const code = e instanceof ApiError ? e.code : undefined;
      if (e instanceof ApiError && e.status === 404) {
        setGone(true);
        onGone?.();
      } else if (code === "RELVIEW_INVALID") setErrs({ label: strictErrorMessage(e) });
      else if (code === "RELVIEW_COLUMN_UNKNOWN") setErrs({ columns: strictErrorMessage(e) });
      else if (code === "RELVIEW_SORT_NOT_SORTABLE") setErrs({ sort: strictErrorMessage(e) });
      else toast.error(tf("studio.build.relMap.display.failed", { msg: errorMessage(e) }));
    } finally {
      setBusy(false);
    }
  };

  if (gone) return <p role="alert" className="slds-text-color_error">{t("studio.build.relMap.related.gone")}</p>;
  return (
    <section>
      <h3 className="slds-text-title_caps slds-m-bottom_x-small">{t("studio.build.relMap.display.title")}</h3>
      <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_x-small">
        {tf("studio.build.relMap.display.help", { parent: parentLabel, child: childLabel })}
      </p>
      <div className="flex flex-col gap-2">
        <Input label={t("studio.build.relMap.display.label")} maxLength={100} error={errs.label} value={label} placeholder={childLabel} onChange={(e) => setLabel(e.target.value)} />
        <Input label={t("studio.build.relMap.display.order")} type="number" step={1} value={order} onChange={(e) => setOrder(e.target.value)} />
        <div>
          <span className="slds-form-element__label">
            {tf("studio.build.relMap.display.columns", { n: columns.length > 0 ? columns.length : t("studio.build.relMap.display.columnsDefault") })}
          </span>
          <ul className="slds-border_top slds-border_bottom" style={{ maxHeight: "10rem", overflowY: "auto" }}>
            {childFields.map((f) => (
              <li key={f.name} className="slds-p-vertical_xx-small">
                <Checkbox label={f.label ?? f.name} checked={columns.includes(f.name)} onChange={() => toggleColumn(f.name)} />
              </li>
            ))}
          </ul>
          {errs.columns && <p role="alert" className="slds-form-element__help slds-text-color_error">{errs.columns}</p>}
        </div>
        <div className="flex items-end gap-2">
          <div className="min-w-0 flex-1">
            <Select
              label={t("studio.build.relMap.display.sort")}
              value={sortField}
              placeholder={t("studio.build.relMap.display.sortDefault")}
              error={errs.sort}
              options={sortOptions}
              onChange={(e) => setSortField(e.target.value)}
            />
          </div>
          <Select
            label={t("studio.build.relMap.display.sortDir")}
            labelHidden
            placeholder={false}
            disabled={!sortField}
            value={sortDir}
            options={[
              { value: "asc", label: t("studio.build.relMap.display.asc") },
              { value: "desc", label: t("studio.build.relMap.display.desc") },
            ]}
            onChange={(e) => setSortDir(e.target.value === "desc" ? "desc" : "asc")}
          />
        </div>
        {relType === "N-N" ? (
          <Checkbox label={t("studio.build.relMap.display.allowLink")} checked={allowLink} onChange={(e) => setAllowLink(e.target.checked)} />
        ) : (
          <Checkbox label={t("studio.build.relMap.display.allowCreate")} checked={allowCreate} onChange={(e) => setAllowCreate(e.target.checked)} />
        )}
        {gate.notice && <p className="slds-text-body_small">{gate.notice}</p>}
        {gate.confirm}
        <div className="flex gap-2">
          <Button variant="brand" disabled={busy} onClick={() => gate.ask(() => void save(false))}>{t("studio.build.relMap.display.save")}</Button>
          <Button variant="neutral" disabled={busy} onClick={() => gate.ask(() => void save(true))}>{t("studio.build.relMap.display.reset")}</Button>
        </div>
      </div>
    </section>
  );
}
