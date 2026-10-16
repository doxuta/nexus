"use client";

// Tab "Quan hệ" of the record page stage (specs/075 US4, FR-050..FR-052): the related lists of this table as a ruled
// list (not bordered cards). Only relationships where THIS table is the parent are listed — a link that exists only
// on the child side shows nothing here (the parent record has no list for it). Lên/Xuống swap the `order` of two
// neighbours; PUT …/view-config replaces the WHOLE document, so each write resends the row's current config with
// the new `order` (no other key is lost). Picking a row opens "Cách hiện danh sách liên quan" in the right column.
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { EmptyState } from "@/components/slds/EmptyState";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { setRelationshipViewConfig, type AdminRelationship } from "@/lib/api";
import { readForBuilder } from "@/lib/readForBuilder";
import { usePublishState } from "@/components/studio/usePublishState";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";

export const relListKey = (entity: string) => ["studio", "screen", entity, "related-lists"] as const;

export interface RelatedListsCanvasProps {
  slug: string;
  entity: string;
  /** Business label of a table (falls back to its name). */
  labelOf: (entityName: string) => string;
  selectedId: string | null;
  onSelect: (id: string | null) => void;
}

const byOrder = (a: AdminRelationship, b: AdminRelationship) =>
  (a.viewConfig?.order ?? Number.MAX_SAFE_INTEGER) - (b.viewConfig?.order ?? Number.MAX_SAFE_INTEGER);

/** Order values after swapping rows i and j: rows with no (or duplicate) order first get their position. */
export function swappedOrders(rows: AdminRelationship[], i: number, j: number): number[] {
  const given = rows.map((r) => r.viewConfig?.order);
  const explicit = given.every((o) => o != null) && new Set(given).size === given.length;
  const next = rows.map((r, k) => (explicit ? (r.viewConfig!.order as number) : k));
  [next[i], next[j]] = [next[j], next[i]];
  return next;
}

export function RelatedListsCanvas({ slug, entity, labelOf, selectedId, onSelect }: RelatedListsCanvasProps) {
  const qc = useQueryClient();
  const delivered = usePublishState(slug).data?.delivered === true;
  const q = useQuery({ queryKey: relListKey(entity), queryFn: () => readForBuilder.listAllRelationships(delivered, entity), staleTime: 0 });
  const [busy, setBusy] = useState(false);
  const [partial, setPartial] = useState<string | null>(null);

  if (q.isPending) return <div className="slds-is-relative slds-p-around_large"><Spinner size="small" /></div>;
  if (q.isError) {
    return (
      <ScopedNotification theme="error">
        {errorMessage(q.error)} <Button variant="base" onClick={() => void q.refetch()}>{t("action.retry")}</Button>
      </ScopedNotification>
    );
  }
  const rows = q.data.filter((r) => r.parentEntityName === entity).sort(byOrder);
  if (rows.length === 0) {
    return (
      <EmptyState
        plain
        title={t("studio.build.screens.record.related.empty")}
        description={t("studio.build.screens.record.related.emptyDesc")}
        action={<a className="slds-button slds-button_neutral" href={studioHref(slug, "dung/du-lieu/so-do", {}, { bang: entity })}>{t("studio.build.screens.record.related.create")}</a>}
      />
    );
  }

  const labelFor = (r: AdminRelationship) => r.viewConfig?.label || labelOf(r.childEntityName);
  async function move(i: number, j: number) {
    const orders = swappedOrders(rows, i, j);
    const writes = rows.flatMap((r, k) => (orders[k] === r.viewConfig?.order ? [] : [{ r, order: orders[k] }]));
    setBusy(true);
    setPartial(null);
    const results = await Promise.allSettled(writes.map((w) => setRelationshipViewConfig(w.r.id, { ...(w.r.viewConfig ?? {}), order: w.order })));
    const failed = results.flatMap((res) => (res.status === "rejected" ? [errorMessage(res.reason)] : []));
    if (failed.length > 0) setPartial(tf("studio.build.screens.record.related.partial", { msg: failed[0] }));
    await qc.invalidateQueries({ queryKey: relListKey(entity) });
    setBusy(false);
  }

  return (
    <div>
      <h3 className="slds-text-heading_small slds-m-bottom_x-small">{t("studio.build.screens.record.related.title")}</h3>
      {partial && <ScopedNotification theme="warning" className="slds-m-bottom_small"><span role="alert">{partial}</span></ScopedNotification>}
      <ul className="slds-border_top">
        {rows.map((r, i) => (
          <li key={r.id} className={`slds-grid slds-grid_vertical-align-center slds-border_bottom slds-p-vertical_x-small${selectedId === r.id ? " slds-theme_shade" : ""}`}>
            <button
              type="button"
              aria-pressed={selectedId === r.id}
              className="slds-button slds-col slds-text-align_left"
              onClick={() => onSelect(selectedId === r.id ? null : r.id)}
            >
              <span className="slds-text-color_weak slds-m-right_x-small">{i + 1}</span>
              {labelFor(r)}
              <span className="slds-text-color_weak slds-m-left_x-small">{t(`studio.build.screens.record.related.kind.${r.relationshipType === "N-N" ? "nn" : r.relationshipType === "1-1" ? "one" : "many"}`)}</span>
            </button>
            <Button variant="base" className="slds-m-right_small" disabled={busy || i === 0} aria-label={tf("studio.build.screens.record.related.upOf", { name: labelFor(r) })} onClick={() => void move(i, i - 1)}>
              {t("studio.build.screens.field.up")}
            </Button>
            <Button variant="base" disabled={busy || i === rows.length - 1} aria-label={tf("studio.build.screens.record.related.downOf", { name: labelFor(r) })} onClick={() => void move(i, i + 1)}>
              {t("studio.build.screens.field.down")}
            </Button>
          </li>
        ))}
      </ul>
    </div>
  );
}
