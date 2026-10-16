"use client";

// RelationPanel (specs/075 T150): the right column of the relation map. Three modes, chosen by what is selected:
//   list  — every link, a keyboard path to a link (Tab + Enter), since edges are not Tab stops;
//   table — one table: "Mở bảng", its links, "Liên kết mới từ bảng này" arrives with the create slice);
//   link  — one link: sentence, read-only rows, "Đang dùng", how it lists on the parent record, "Gỡ liên kết…".
// Removal ALWAYS reads the impact first and never calls DELETE before the in-place confirmation. No dialog.
import { useState } from "react";
import Link from "next/link";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, deleteRelationshipById, getRelationshipImpact, updateField, type FieldMeta, type RelViewConfig } from "@/lib/api";
import { strictErrorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { impactDependents, edgeKindOf, type CanvasEdgeSpec } from "@/lib/relCanvas";
import { studioHref } from "@/lib/studioRoutes";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { useDirectApplyGate } from "@/components/studio/useDirectApplyGate";
import { RelatedListSettings } from "@/components/studio/screen/RelatedListSettings";
import { RelationForm, type CreatedLink } from "./RelationForm";
import { RelationImpactBlock } from "./RelationImpactBlock";

export interface PanelEntity { name: string; label: string; fields: FieldMeta[] }
export type RelationSelection = { kind: "node"; id: string } | { kind: "edge"; id: string } | { kind: "new"; parent?: string; child?: string } | null;

export interface RelationPanelProps {
  slug: string;
  selection: RelationSelection;
  entities: PanelEntity[];
  edges: CanvasEdgeSpec[];
  onSelect: (s: RelationSelection) => void;
  /** A link was removed (or found already gone): reload the map. */
  onChanged: () => void;
  onViewConfigSaved?: (relId: string, cfg: RelViewConfig | null) => void;
  /** A link was created: the map reloads and selects it. */
  onCreated?: (c: CreatedLink) => void;
}

export function RelationPanel({ slug, selection, entities, edges, onSelect, onChanged, onViewConfigSaved, onCreated }: RelationPanelProps) {
  const labelOf = (n: string) => entities.find((e) => e.name === n)?.label ?? n;
  const edge = selection?.kind === "edge" ? edges.find((e) => e.id === selection.id) : undefined;
  const node = selection?.kind === "node" ? entities.find((e) => e.name === selection.id) : undefined;

  if (selection?.kind === "new") {
    return (
      <RelationForm
        key={`${selection.parent ?? ""}>${selection.child ?? ""}`}
        entities={entities}
        initialParent={selection.parent}
        initialChild={selection.child}
        onCreated={(c) => onCreated?.(c)}
        onCancel={() => onSelect(selection.parent ? { kind: "node", id: selection.parent } : null)}
      />
    );
  }

  if (edge) {
    return <LinkView key={edge.id} slug={slug} edge={edge} entities={entities} labelOf={labelOf} onBack={() => onSelect(null)} onChanged={onChanged} onViewConfigSaved={onViewConfigSaved} />;
  }

  const linkList = (list: CanvasEdgeSpec[]) => (
    <ul>
      {list.map((e) => (
        <li key={e.id} className="slds-border_top">
          <button type="button" className="slds-button slds-button_reset slds-text-align_left slds-p-vertical_x-small" style={{ width: "100%", display: "flex", flexDirection: "column", alignItems: "flex-start" }} onClick={() => onSelect({ kind: "edge", id: e.id })}>
            <span className="slds-truncate" style={{ display: "block" }}>{labelOf(e.source)} → {labelOf(e.target)}</span>
            <span className="slds-text-body_small slds-text-color_weak" style={{ display: "block" }}>{t(edgeKindOf(e.relType).labelKey)}</span>
          </button>
        </li>
      ))}
    </ul>
  );

  if (node) {
    const mine = edges.filter((e) => e.source === node.name || e.target === node.name);
    return (
      <div className="flex flex-col gap-3">
        <Button variant="base" onClick={() => onSelect(null)}>{t("studio.build.relMap.panel.back")}</Button>
        <h2 className="slds-text-heading_small">{node.label}</h2>
        <Link className="slds-button slds-button_neutral" href={studioHref(slug, "dung/du-lieu/:bang", { bang: node.name })}>
          {t("studio.build.relMap.panel.openTable")}
        </Link>
        <section>
          <h3 className="slds-text-title_caps">{tf("studio.build.relMap.panel.tableLinks", { n: mine.length })}</h3>
          <Button variant="neutral" onClick={() => onSelect({ kind: "new", parent: node.name })}>{t("studio.build.relMap.panel.newFromTable")}</Button>
          {mine.length === 0 ? <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.relMap.state.tableNoRelations")}</p> : linkList(mine)}
        </section>
        <section>
          <h3 className="slds-text-title_caps">{tf("studio.build.relMap.panel.tableFields", { n: node.fields.length })}</h3>
          <ul style={{ maxHeight: "14rem", overflowY: "auto" }}>
            {node.fields.map((f) => <li key={f.name} className="slds-border_top slds-p-vertical_xx-small slds-truncate">{f.label ?? f.name}</li>)}
          </ul>
        </section>
      </div>
    );
  }

  return (
    <section className="flex flex-col gap-2">
      <h2 className="slds-text-title_caps">{tf("studio.build.relMap.panel.listHead", { n: edges.length })}</h2>
      <div><Button variant="neutral" onClick={() => onSelect({ kind: "new" })}>{t("studio.build.relMap.panel.newLink")}</Button></div>
      {edges.length === 0 ? <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.relMap.panel.listEmpty")}</p> : linkList(edges)}
    </section>
  );
}

function LinkView({ slug, edge, entities, labelOf, onBack, onChanged, onViewConfigSaved }: {
  slug: string; edge: CanvasEdgeSpec; entities: PanelEntity[]; labelOf: (n: string) => string;
  onBack: () => void; onChanged: () => void; onViewConfigSaved?: (relId: string, cfg: RelViewConfig | null) => void;
}) {
  const qc = useQueryClient();
  // Every link delete is applied at once (RelationLifecycleService.Delete runs under WithPublishApply, specs/063 FR-070): one label for all kinds.
  const applyNow = useDirectApplyGate(slug, "relationship");
  const rel = edge.rel;
  const relId = rel?.id ?? "";
  const key = ["studio", "relmap", "impact", relId];
  const impactQ = useQuery({ queryKey: key, queryFn: () => getRelationshipImpact(relId), enabled: !!rel, retry: false, gcTime: 0, staleTime: 0 });
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const parent = labelOf(edge.source);
  const child = labelOf(edge.target);
  const status = impactQ.isError ? "error" : impactQ.data ? "ok" : "loading";
  const blocked = impactQ.data ? !impactQ.data.deletable || impactQ.data.dependentRollups.length > 0 : true;
  const kind = edgeKindOf(edge.relType);
  const [labelDraft, setLabelDraft] = useState(edge.field?.label ?? edge.field?.name ?? "");
  const [savedLabel, setSavedLabel] = useState(edge.field?.label ?? edge.field?.name ?? "");
  const [labelBusy, setLabelBusy] = useState(false);
  // Only the link-field label is editable (PUT /admin/fields/:id). Target, kind and name are structural: PATCH refuses them.
  const saveLabel = async () => {
    const f = edge.field;
    if (!f?.id || !labelDraft.trim()) return;
    setLabelBusy(true);
    try {
      await updateField(f.id, { label: labelDraft.trim() });
      setSavedLabel(labelDraft.trim());
      toast.success(t("studio.build.relMap.panel.fieldLabelSaved"));
    } catch (e) {
      toast.error(tf("studio.build.relMap.panel.fieldLabelFailed", { msg: strictErrorMessage(e) }));
    } finally {
      setLabelBusy(false);
    }
  };
  const childEntity = entities.find((e) => e.name === edge.target);

  // Read the impact FIRST, every time — the confirm opens only on a fresh, deletable answer.
  const startRemove = async () => {
    setBusy(true);
    try {
      const fresh = await qc.fetchQuery({ queryKey: key, queryFn: () => getRelationshipImpact(relId), staleTime: 0 });
      if (fresh.deletable && fresh.dependentRollups.length === 0) setAsking(true);
    } catch (e) {
      toast.error(tf("studio.build.relMap.panel.removeFailed", { msg: strictErrorMessage(e) }));
    } finally {
      setBusy(false);
    }
  };

  const remove = async () => {
    if (!rel) return;
    setBusy(true);
    try {
      await deleteRelationshipById(rel.id);
      toast.success(tf("studio.build.relMap.panel.removed", { name: `${parent} → ${child}` }));
      setAsking(false);
      onChanged();
    } catch (e) {
      setAsking(false);
      if (e instanceof ApiError && e.status === 404) {
        toast.warning(t("studio.build.relMap.panel.alreadyGone"));
        onChanged();
      } else if (e instanceof ApiError && e.code === "RELATIONSHIP_HAS_DEPENDENTS") {
        toast.error(strictErrorMessage(e));
        void impactQ.refetch();
      } else {
        toast.error(tf("studio.build.relMap.panel.removeFailed", { msg: strictErrorMessage(e) }));
      }
    } finally {
      setBusy(false);
    }
  };

  const descKey = edge.relType === "N-N" ? "studio.build.relMap.panel.descManyMany" : edge.relType === "1-1" ? "studio.build.relMap.panel.descOneOne" : "studio.build.relMap.panel.descOneMany";
  const records = impactQ.data ? impactDependents(impactQ.data).records : 0;
  const rows: Array<[string, string]> = [
    [t("studio.build.relMap.panel.rowKind"), t(kind.labelKey)],
    [t("studio.build.relMap.panel.rowParent"), parent],
    [t("studio.build.relMap.panel.rowChild"), child],
    ...(rel ? ([[t("studio.build.relMap.panel.rowName"), rel.name]] as Array<[string, string]>) : []),
  ];

  return (
    <div className="flex flex-col gap-3">
      <Button variant="base" onClick={onBack}>{t("studio.build.relMap.panel.back")}</Button>
      <h2 className="slds-text-heading_small">{parent} → {child}</h2>
      <p className="slds-text-body_regular">{tf(descKey, { parent, child })}</p>
      <dl>
        {rows.map(([k, v]) => (
          <div key={k} className="slds-border_top slds-p-vertical_xx-small">
            <dt className="slds-text-body_small slds-text-color_weak">{k}</dt>
            <dd className="slds-truncate">{v}</dd>
          </div>
        ))}
      </dl>
      {edge.relType === "N-N" ? (
        <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.relMap.panel.manyManyNoLabel")}</p>
      ) : edge.field?.id ? (
        <div className="flex items-end gap-2">
          <div className="flex-1"><Input label={t("studio.build.relMap.panel.fieldLabel")} value={labelDraft} onChange={(e) => setLabelDraft(e.target.value)} /></div>
          <Button variant="neutral" disabled={labelBusy || !labelDraft.trim() || labelDraft.trim() === savedLabel} onClick={saveLabel}>{t("studio.build.relMap.panel.fieldLabelSave")}</Button>
        </div>
      ) : null}
      <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.relMap.panel.immutable")}</p>

      {rel ? (
        <>
          <RelationImpactBlock slug={slug} status={status} impact={impactQ.data} labelOf={labelOf} onRetry={() => void impactQ.refetch()} />
          <section className="slds-border_top slds-p-top_small">
            <RelatedListSettings
              slug={slug}
              relId={rel.id}
              relType={edge.relType}
              parentLabel={parent}
              childLabel={child}
              childFields={childEntity?.fields ?? []}
              initial={rel.viewConfig}
              onSaved={onViewConfigSaved}
            />
          </section>
          <section className="slds-border_top slds-p-top_small">
            {asking ? (
              <InlineConfirm
                tone="error"
                message={tf("studio.build.relMap.panel.removeAsk", { name: `${parent} → ${child}`, n: records })}
                confirmLabel={t("studio.build.relMap.panel.removeConfirm")}
                busy={busy}
                onConfirm={remove}
                onCancel={() => setAsking(false)}
              >
                {applyNow.notice && <p className="slds-text-body_small">{applyNow.notice} {t("studio.build.screens.draft.noReturn")}</p>}
              </InlineConfirm>
            ) : (
              <>
                {applyNow.notice && <p className="slds-text-body_small slds-m-bottom_x-small">{applyNow.notice}</p>}
                <Button variant="text-destructive" disabled={busy || status !== "ok" || blocked} onClick={startRemove}>
                  {t("studio.build.relMap.panel.remove")}
                </Button>
              </>
            )}
          </section>
        </>
      ) : (
        <>
          <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.relMap.panel.legacy")}</p>
          <Button variant="text-destructive" disabled>{t("studio.build.relMap.panel.remove")}</Button>
        </>
      )}
    </div>
  );
}
