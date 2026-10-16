"use client";

// Right column of the record-page editor when a related list is picked (`?lien-quan=<id>`): the display settings of
// that list. Reads the relationship from the same query the Quan hệ tab uses and the CHILD table's fields (columns and
// sort are validated against them). The settings form is keyed by id + order so a Lên/Xuống reorder, which rewrites the
// stored document, reloads it instead of letting a stale `order` be saved back.
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Spinner } from "@/components/slds/Spinner";
import { readForBuilder } from "@/lib/readForBuilder";
import { usePublishState } from "@/components/studio/usePublishState";
import { t } from "@/lib/i18n";
import { RelatedListSettings } from "./RelatedListSettings";
import { relListKey } from "./RelatedListsCanvas";

export function RelatedListPane({ slug, entity, relId, labelOf, tableLabel, onGone }: {
  slug: string;
  entity: string;
  relId: string;
  labelOf: (entityName: string) => string;
  tableLabel: string;
  onGone: () => void;
}) {
  const qc = useQueryClient();
  const delivered = usePublishState(slug).data?.delivered === true;
  const rels = useQuery({ queryKey: relListKey(entity), queryFn: () => readForBuilder.listAllRelationships(delivered, entity) });
  const rel = rels.data?.find((r) => r.id === relId && r.parentEntityName === entity);
  const child = useQuery({
    queryKey: ["studio", "screen", "fields", rel?.childEntityName, delivered],
    queryFn: () => readForBuilder.listFields(delivered, rel!.childEntityName),
    enabled: !!rel,
  });
  if (rels.isPending || (rel && child.isPending)) return <div className="slds-is-relative slds-p-around_large"><Spinner size="small" /></div>;
  if (!rel || !child.data) {
    return (
      <p role="alert" className="slds-text-color_error">
        {t("studio.build.relMap.related.gone")}{" "}
        <button type="button" className="slds-button" onClick={onGone}>{t("studio.build.screens.field.done")}</button>
      </p>
    );
  }
  return (
    <RelatedListSettings
      slug={slug}
      key={`${rel.id}:${rel.viewConfig?.order ?? ""}`}
      relId={rel.id}
      relType={rel.relationshipType as "1-N" | "N-N" | "1-1"}
      parentLabel={tableLabel}
      childLabel={labelOf(rel.childEntityName)}
      childFields={child.data}
      initial={rel.viewConfig}
      onSaved={() => void qc.invalidateQueries({ queryKey: relListKey(entity) })}
      onGone={() => { void qc.invalidateQueries({ queryKey: relListKey(entity) }); }}
    />
  );
}
