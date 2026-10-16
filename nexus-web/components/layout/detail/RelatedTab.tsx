"use client";

// "Quan hệ" tab — every list of records tied to this one, as SLDS related-list cards (blueprint
// cards: related-list-table when the admin configured columns, related-list-tiles otherwise).
// Cards sit inside the tab container, so they carry slds-card_boundary (cards "nested-with-boundary").
//   • N-N relationships (GET /:entity/:id/relationships/:relName?expand=true) — link / unlink.
//   • This entity's own relation fields answering on that same route (a forward 1-N 404s there and is
//     already shown inline in Tổng quan, so it is skipped).
//   • "Danh sách liên quan": the PARENT side of 1-N rows from the relationship registry (ENG-REL2 #4)
//     — records of a child entity whose relation field points here, with "+ Tạo" (quick-create, FK
//     prefilled) and "Xem tất cả" deep-linking into the child list pre-filtered.
//   • "Gắn với bản ghi này": the reverse polymorphic timeline (ACTIVITY-CORE #3a) — rows whose
//     polymorphic_relation field points at this record.
// ENG-RELVIEW: a registry row may carry `viewConfig` ({label, order, columns, sort, allowCreate,
// allowLink}); absent config → every branch falls back to the pre-RELVIEW rendering.
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { ArrowUpRight, FileText, Link2, List, Plus, X } from "lucide-react";
import {
  ensureAuth,
  listFields,
  listRelationships,
  listAllRelationships,
  addRelationship,
  removeRelationship,
  listRecordsPage,
  listEntities,
  listChildRecords,
  listPolymorphicRecords,
  ApiError,
  type AdminRelationship,
  type ApiRecord,
  type EntityMeta,
  type FieldMeta,
} from "@/lib/api";
import { recordLabel } from "@/lib/recordLabel";
import { findPolymorphicSources, type PolymorphicSource } from "@/lib/polymorphicTimeline";
import { metaToSpec } from "@/lib/fieldSpec";
import { t } from "@/lib/i18n";
import { FieldDisplay } from "@/components/fields/display";
import { QuickCreateModal } from "@/components/layout/QuickCreateModal";
import { Badge } from "@/components/slds/Badge";
import { Button, buttonClassName } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { EmptyState } from "@/components/slds/EmptyState";
import { Icon } from "@/components/slds/Icon";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";

// Panel ordering: viewConfig.order ascending; rows without an order keep their API position
// (name ASC) AFTER the ordered ones. Stable sort ⇒ zero config = byte-identical order.
function byViewOrder(a: AdminRelationship, b: AdminRelationship): number {
  return (a.viewConfig?.order ?? Number.MAX_SAFE_INTEGER) - (b.viewConfig?.order ?? Number.MAX_SAFE_INTEGER);
}

// One discovered polymorphic source, enriched with its entity label and field metadata.
interface PolyTimelineSource extends PolymorphicSource {
  entityLabel: string;
  fields: FieldMeta[];
}
// Preview rows per list — one visual rhythm for every related card.
const PREVIEW_LIMIT = 5;

// cards/related-list-tiles body: one tile per record, title linking to it, optional trailing action.
function RecordTiles({ items }: { items: { id: string; label: string; href: string; action?: ReactNode }[] }) {
  return (
    <ul className="slds-grid slds-wrap slds-grid_pull-padded">
      {items.map((it) => (
        <li key={it.id} className="slds-p-horizontal_small slds-size_1-of-1 slds-medium-size_1-of-3">
          <article className="slds-tile slds-media slds-card__tile slds-hint-parent">
            <div className="slds-media__figure">
              <Icon icon={FileText} category="standard" name="record" size="small" />
            </div>
            <div className="slds-media__body">
              <div className="slds-grid slds-grid_align-spread slds-has-flexi-truncate">
                <h3 className="slds-tile__title slds-truncate" title={it.label}>
                  <a href={it.href}>{it.label}</a>
                </h3>
                {it.action && <div className="slds-shrink-none">{it.action}</div>}
              </div>
            </div>
          </article>
        </li>
      ))}
    </ul>
  );
}

function Loading({ label }: { label: string }) {
  return (
    <div className="relative h-12">
      <Spinner size="small" label={label} />
    </div>
  );
}

export function RelatedTab({
  entity,
  recordId,
  allFields,
}: {
  entity: string;
  recordId: string;
  allFields: FieldMeta[];
}) {
  const relationFields = allFields.filter((f) => f.fieldType === "relation");
  const [relData, setRelData] = useState<Record<string, ApiRecord[]>>({});
  // #272: the target table's fields per relation field, so a tile is named by the table's own
  // label field (isLabel) instead of a UUID.
  const [relTargetFields, setRelTargetFields] = useState<Record<string, FieldMeta[]>>({});
  const [relErrors, setRelErrors] = useState<Record<string, string>>({});
  const [relLoading, setRelLoading] = useState<Record<string, boolean>>({});

  // Every relationship registry row involving this entity (any type), loaded ONCE and sliced
  // client-side — a single GET /metadata/relationships?entity= call.
  const [allRels, setAllRels] = useState<AdminRelationship[]>([]);
  useEffect(() => {
    let alive = true;
    listAllRelationships(entity)
      .then((rs) => { if (alive) setAllRels(rs); })
      .catch(() => { if (alive) setAllRels([]); });
    return () => { alive = false; };
  }, [entity]);

  // N-N relationships — this tab's link/unlink API only implements N-N semantics.
  const nnRels = useMemo(
    () => allRels.filter((r) => r.relationshipType === "N-N").sort(byViewOrder),
    [allRels],
  );
  // PARENT-side 1-N rows: some OTHER entity has a relation field pointing back at this one.
  const parentRels = useMemo(
    () => allRels
      .filter((r) => r.relationshipType === "1-N" && r.parentEntityName === entity)
      .sort(byViewOrder),
    [allRels, entity],
  );

  // Entity labels for card titles — the customer reads "Liên hệ", never "crm_contacts". Loaded only
  // when some card needs one; until then the title falls back to the machine name.
  const [entityLabels, setEntityLabels] = useState<Record<string, string>>({});
  const needLabels = parentRels.length + nnRels.length;
  useEffect(() => {
    if (needLabels === 0) return;
    let alive = true;
    listEntities()
      .then((es) => { if (alive) setEntityLabels(Object.fromEntries(es.map((e: EntityMeta) => [e.name, e.label]))); })
      .catch(() => { /* label falls back to raw entity name */ });
    return () => { alive = false; };
  }, [needLabels]);

  // ── Reverse polymorphic timeline (ACTIVITY-CORE #3a) ──────────────────────────────────────
  // Which OTHER entities carry a `polymorphic_relation` field whose allowlist contains THIS entity?
  // Invisible to the relationship registry, so discovery reads the candidate entities' metadata.
  // ponytail: N listFields calls, cached per session. An app has ~10-20 entities; add a BE
  // discovery route only if that grows past ~30. This tab only mounts when the user opens it.
  const { data: polySources, isPending: sourcesPending } = useQuery({
    queryKey: ["poly-sources", entity],
    queryFn: async () => {
      await ensureAuth();
      const es = await listEntities();
      // allSettled: one entity whose metadata read fails drops THAT entity, not the whole timeline.
      const settled = await Promise.allSettled(es.map((e) => listFields(e.name)));
      const candidates = es.map((e, i) => {
        const r = settled[i];
        return { entity: e.name, fields: r.status === "fulfilled" ? r.value : [] };
      });
      const fieldsOf = new Map(candidates.map((c) => [c.entity, c.fields]));
      const labelOf = new Map(es.map((e) => [e.name, e.label]));
      return findPolymorphicSources(entity, candidates).map((s): PolyTimelineSource => ({
        ...s,
        entityLabel: labelOf.get(s.entity) || s.entity,
        fields: fieldsOf.get(s.entity) ?? [],
      }));
    },
    staleTime: 5 * 60 * 1000,
    retry: false,
  });

  const sources = useMemo(() => polySources ?? [], [polySources]);
  // Rows for every discovered source, fetched TOGETHER so the section knows its own emptiness
  // before it renders — a per-section fetch would flash a heading that then vanishes.
  const { data: polyGroups, isPending: groupsPending } = useQuery({
    queryKey: ["poly-timeline", entity, recordId, sources.map((s) => `${s.entity}.${s.fieldName}`).join(",")],
    queryFn: async () => {
      const settled = await Promise.allSettled(
        sources.map((s) => listPolymorphicRecords(s.entity, s.fieldName, entity, recordId, PREVIEW_LIMIT)),
      );
      const groups: { source: PolyTimelineSource; records: ApiRecord[]; total: number }[] = [];
      // "Refused" (403/404: this role may not see that column — stay quiet) is not "broken"
      // (network/5xx: we DO NOT KNOW). Count the second kind so the user is told.
      let unreadable = 0;
      sources.forEach((s, i) => {
        const r = settled[i];
        if (r.status === "rejected") {
          const status = r.reason instanceof ApiError ? r.reason.status : 0;
          if (!(status === 403 || status === 404)) unreadable++;
          return;
        }
        if (r.value.records.length === 0) return; // nothing attached → no card at all
        groups.push({ source: s, records: r.value.records, total: r.value.total });
      });
      return { groups, unreadable };
    },
    enabled: sources.length > 0,
    staleTime: 60 * 1000,
    retry: false,
  });

  // Load all relation fields on mount.
  useEffect(() => {
    if (relationFields.length === 0) return;
    let cancelled = false;
    (async () => {
      await ensureAuth();
      for (const f of relationFields) {
        if (cancelled) return;
        setRelLoading((prev) => ({ ...prev, [f.name]: true }));
        try {
          const records = await listRelationships(entity, recordId, f.name, true);
          const target = (f.options as { relatedEntity?: string } | undefined)?.relatedEntity;
          // Target fields land BEFORE the rows, so a tile never flashes a guessed name and then
          // swaps to the table's label field.
          if (target && records.length > 0) {
            const tf = await listFields(target).catch(() => [] as FieldMeta[]); // label falls back to the heuristic
            if (!cancelled) setRelTargetFields((prev) => ({ ...prev, [f.name]: tf }));
          }
          if (!cancelled) setRelData((prev) => ({ ...prev, [f.name]: records }));
        } catch (e) {
          if (!cancelled) {
            const msg = e instanceof ApiError
              ? (e.status === 404 ? t("page.record.related.nnMissing") : e.message)
              : String(e);
            setRelErrors((prev) => ({ ...prev, [f.name]: msg }));
          }
        } finally {
          if (!cancelled) setRelLoading((prev) => ({ ...prev, [f.name]: false }));
        }
      }
    })();
    return () => { cancelled = true; };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [entity, recordId]);

  // polyPending holds the "nothing related" verdict while discovery is in flight (read from
  // isPending, not `data === undefined`: a FAILED discovery must fall back to the empty state).
  const polyPending = sourcesPending || (sources.length > 0 && groupsPending);
  if (relationFields.length === 0 && nnRels.length === 0 && parentRels.length === 0
      && !polyGroups?.groups.length && !polyGroups?.unreadable && !polyPending) {
    return (
      <EmptyState icon={Link2} title={t("page.record.related.emptyTitle")} description={t("page.record.related.emptyText")} />
    );
  }

  return (
    <>
      {nnRels.map((rel) => {
        const other = rel.parentEntityName === entity ? rel.childEntityName : rel.parentEntityName;
        return (
          <NNRelationSection
            key={rel.name}
            entity={entity}
            recordId={recordId}
            rel={rel}
            otherLabel={entityLabels[other] ?? other}
          />
        );
      })}
      {relationFields.map((f) => {
        const records = relData[f.name] ?? [];
        const err = relErrors[f.name];
        const loading = relLoading[f.name] ?? false;
        const opts = (f.options ?? {}) as { relatedEntity?: string; labelField?: string };
        // A forward 1-N relation field 404s on the N-N route; its single FK is already inline in
        // Tổng quan, so skip it instead of a scary "not registered" card.
        if (err && records.length === 0) return null;
        return (
          <Card
            key={f.name}
            boundary
            className="slds-m-bottom_small"
            title={f.label ?? f.name}
            icon={{ glyph: Link2, name: "link" }}
            count={loading ? undefined : records.length}
          >
            {loading && <Loading label={t("page.record.loading")} />}
            {!loading && err && <p className="slds-text-color_error">{err}</p>}
            {!loading && !err && records.length === 0 && (
              <p className="slds-text-color_weak">{t("page.record.related.noneLinked")}</p>
            )}
            {!loading && !err && records.length > 0 && (
              <RecordTiles
                items={records.map((r) => ({
                  id: r.id,
                  label: recordLabel(r, relTargetFields[f.name] ?? [], { labelField: opts.labelField }),
                  href: `/${opts.relatedEntity ?? entity}/${r.id}`,
                }))}
              />
            )}
          </Card>
        );
      })}

      {/* Parent-side related lists — "who lists me?" (ENG-REL2 #4 inverse registry). */}
      {parentRels.length > 0 && (
        <>
          <h2 className="slds-text-title_caps slds-m-top_medium slds-m-bottom_x-small">{t("page.record.related.parentGroup")}</h2>
          {parentRels.map((rel) => (
            <ParentRelatedSection
              key={rel.name}
              rel={rel}
              parentId={recordId}
              entityLabel={entityLabels[rel.childEntityName] ?? rel.childEntityName}
            />
          ))}
        </>
      )}

      {/* Reverse polymorphic timeline — a source with nothing attached renders NOTHING, so the whole
          block disappears when nothing points here. */}
      {polyGroups && (polyGroups.groups.length > 0 || polyGroups.unreadable > 0) && (
        <>
          <h2 className="slds-text-title_caps slds-m-top_medium slds-m-bottom_x-small">{t("page.record.related.polyGroup")}</h2>
          {polyGroups.groups.map((g) => (
            <Card
              key={`${g.source.entity}.${g.source.fieldName}`}
              boundary
              className="slds-m-bottom_small"
              title={g.source.entityLabel}
              icon={{ glyph: List, name: "related-list" }}
              count={g.total}
              // Which field points here — two polymorphic fields on one entity are two lists.
              actions={<Badge>{g.source.fieldLabel}</Badge>}
              // The count is the TRUE total but only PREVIEW_LIMIT rows are fetched — say which these are.
              footer={g.total > g.records.length ? (
                <span>
                  {t("page.record.related.polyShowing").replace("{n}", String(g.records.length)).replace("{total}", String(g.total))}{" "}
                  <a href={`/${g.source.entity}`}>{g.source.entityLabel}</a>
                </span>
              ) : undefined}
            >
              <RecordTiles
                items={g.records.map((r) => ({
                  id: r.id,
                  label: recordLabel(r, g.source.fields),
                  href: `/${g.source.entity}/${r.id}`,
                }))}
              />
            </Card>
          ))}
          {/* A read that FAILED is not a read that found nothing. */}
          {polyGroups.unreadable > 0 && (
            <ScopedNotification theme="warning">
              {t("page.record.related.polyUnreadable").replace("{n}", String(polyGroups.unreadable))}
            </ScopedNotification>
          )}
        </>
      )}
    </>
  );
}

// ── Parent-side related list — records of a CHILD entity whose relation field points at THIS
// record. fkField comes from rel.childFkColumn ("<field>_id") with the "_id" stripped back to the
// field NAME the list filter dialect expects.
function ParentRelatedSection({
  rel,
  parentId,
  entityLabel,
}: {
  rel: AdminRelationship;
  parentId: string;
  entityLabel: string;
}) {
  const fkField = (rel.childFkColumn ?? "").replace(/_id$/, "");
  const vc = rel.viewConfig;
  const cols = vc?.columns ?? [];
  const sortField = vc?.sort?.field;
  const sortDir = vc?.sort?.dir;

  const [fetchState, setState] = useState<"loading" | "ok" | "error">("loading");
  const [records, setRecords] = useState<ApiRecord[]>([]);
  const [total, setTotal] = useState(0);
  const [fetchErr, setErr] = useState("");
  // "+ Tạo" opens the quick-create modal (FK prefilled) instead of leaving the parent; reloadKey
  // re-runs the fetch after a save. The full-page href stays on the anchor as fallback.
  const [quickOpen, setQuickOpen] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);

  // Child field metas — configured-column headers/cells AND the default row label.
  const [childFields, setChildFields] = useState<FieldMeta[]>([]);
  useEffect(() => {
    let alive = true;
    listFields(rel.childEntityName)
      .then((fs) => { if (alive) setChildFields(fs); })
      .catch(() => { /* headers/labels fall back to raw names / #id */ });
    return () => { alive = false; };
  }, [rel.childEntityName]);
  const childFieldsByName = useMemo(
    () => Object.fromEntries(childFields.map((f) => [f.name, f])),
    [childFields],
  );

  useEffect(() => {
    // A malformed/legacy registry row without childFkColumn can't be filtered (said at render).
    if (!fkField) return;
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        // DP-2.1: expand the child's relation fields (minus the FK back to THIS parent) so cells
        // and row labels show names, not UUIDs.
        const childRels = childFields
          .filter((f) => f.fieldType === "relation" && f.name !== fkField)
          .map((f) => f.name);
        const page = await listChildRecords(
          rel.childEntityName, fkField, parentId, PREVIEW_LIMIT,
          sortField ? { field: sortField, dir: sortDir } : undefined,
          childRels,
        );
        if (!alive) return;
        setRecords(page.records);
        setTotal(page.total);
        setState("ok");
      } catch (e) {
        if (!alive) return;
        setErr(e instanceof ApiError ? e.message : t("page.record.related.loadError"));
        setState("error");
      }
    })();
    return () => { alive = false; };
  }, [rel.childEntityName, fkField, parentId, sortField, sortDir, reloadKey, childFields]);

  // ?field[op]=value bracket syntax — the list screen's deep-link parser only reads "field[op]" keys.
  const viewAllHref = fkField
    ? `/${rel.childEntityName}?${new URLSearchParams({ [`${fkField}[eq]`]: parentId }).toString()}`
    : `/${rel.childEntityName}`;
  const panelTitle = vc?.label || entityLabel;
  const state: "loading" | "ok" | "error" = fkField ? fetchState : "error";
  const err = fkField ? fetchErr : t("page.record.related.missingFk");
  const createHref = fkField
    ? `/${rel.childEntityName}/new?${new URLSearchParams({ [`pf_${fkField}`]: parentId }).toString()}`
    : `/${rel.childEntityName}/new`;
  const batchHref = fkField
    ? `/${rel.childEntityName}/batch?${new URLSearchParams({ [`pf_${fkField}`]: parentId }).toString()}`
    : `/${rel.childEntityName}/batch`;
  const table = state === "ok" && records.length > 0 && cols.length > 0;

  return (
    <>
      <Card
        boundary
        className="slds-m-bottom_small"
        title={panelTitle}
        icon={{ glyph: List, name: "related-list" }}
        count={state === "ok" ? total : undefined}
        loading={state === "loading"}
        padded={!table}
        actions={(vc?.allowCreate ?? true) && (
          // Default-on: a 1-N related list lets you add a child born linked to the parent; an admin
          // opts out per relationship with viewConfig.allowCreate = false.
          <>
          {fkField && (
            // queue #304: many children at once (điểm danh cả lớp) — the grid opens with every row
            // already linked to this parent.
            <a
              href={batchHref}
              className={buttonClassName("neutral")}
              title={t("page.record.related.createManyTitle").replace("{label}", panelTitle)}
            >
              {t("page.record.related.createMany")}
            </a>
          )}
          <a
            href={createHref}
            className={buttonClassName("neutral")}
            title={t("page.record.related.createTitle").replace("{label}", panelTitle)}
            onClick={(e) => {
              // Plain left-click → quick-create modal; modified clicks and the no-FK edge keep the
              // full-page navigation.
              if (!fkField || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
              e.preventDefault();
              setQuickOpen(true);
            }}
          >
            <Plus aria-hidden="true" className="slds-button__icon slds-button__icon_left nx-lucide" />
            {t("page.record.related.create")}
          </a>
          </>
        )}
        footer={state === "ok" && total > 0 ? (
          <a className="slds-card__footer-action" href={viewAllHref}>
            {t("page.record.related.viewAll").replace("{n}", String(total))}
          </a>
        ) : undefined}
      >
        {state === "error" && <p className="slds-text-color_error">{err}</p>}
        {state === "ok" && records.length === 0 && (
          <p className="slds-text-color_weak">
            {t("page.record.related.noneOfKind").replace("{label}", panelTitle.toLowerCase())}
          </p>
        )}
        {state === "ok" && records.length > 0 && cols.length === 0 && (
          <RecordTiles
            items={records.map((r) => ({ id: r.id, label: recordLabel(r, childFields), href: `/${rel.childEntityName}/${r.id}` }))}
          />
        )}
        {/* ENG-RELVIEW: configured columns → cards/related-list-table (type-aware cells through the
            shared FieldDisplay); headers fall back to raw names until metas load. */}
        {table && (
          <table
            className="slds-table slds-table_cell-buffer slds-no-row-hover slds-table_bordered slds-table_fixed-layout"
            aria-label={panelTitle}
          >
            <thead>
              <tr className="slds-line-height_reset">
                {cols.map((col) => {
                  const label = childFieldsByName[col]?.label ?? col;
                  return (
                    <th key={col} scope="col">
                      <div className="slds-truncate" title={label}>{label}</div>
                    </th>
                  );
                })}
                <th scope="col" className="w-12">
                  <span className="slds-assistive-text">{t("page.record.related.openRecord")}</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {records.map((r) => (
                <tr key={r.id} className="slds-hint-parent">
                  {cols.map((col) => {
                    const meta = childFieldsByName[col];
                    const value = r.data?.[col];
                    return (
                      <td key={col}>
                        <div className="slds-truncate">
                          {meta ? <FieldDisplay field={metaToSpec(meta)} value={value} /> : (value == null ? "—" : String(value))}
                        </div>
                      </td>
                    );
                  })}
                  <td>
                    <a
                      href={`/${rel.childEntityName}/${r.id}`}
                      className="slds-button slds-button_icon"
                      title={t("page.record.related.openRecord")}
                    >
                      <ArrowUpRight aria-hidden="true" className="slds-button__icon nx-lucide" />
                      <span className="slds-assistive-text">
                        {t("page.record.related.openNamed").replace("{label}", recordLabel(r, childFields))}
                      </span>
                    </a>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Card>

      {fkField && (
        <QuickCreateModal
          entity={rel.childEntityName}
          open={quickOpen}
          onClose={() => setQuickOpen(false)}
          onSaved={() => setReloadKey((k) => k + 1)}
          prefill={{ [fkField]: parentId }}
        />
      )}
    </>
  );
}

// ── N-N relationship card — list + link + unlink records of the other entity ──────────────────
function NNRelationSection({
  entity,
  recordId,
  rel,
  otherLabel,
}: {
  entity: string;
  recordId: string;
  rel: AdminRelationship;
  otherLabel: string;
}) {
  const otherEntity = rel.parentEntityName === entity ? rel.childEntityName : rel.parentEntityName;
  // ENG-RELVIEW: label overrides the title; allowLink === false hides the picker (read-only card).
  const vc = rel.viewConfig;
  const panelTitle = vc?.label || otherLabel;
  const canLink = vc?.allowLink !== false;
  const [linked, setLinked] = useState<ApiRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState("");
  const [picking, setPicking] = useState(false);
  const [candidates, setCandidates] = useState<ApiRecord[]>([]);
  const [candErr, setCandErr] = useState(""); // distinct from "no candidates" — see openPicker
  const [candLoading, setCandLoading] = useState(false);
  const [busyId, setBusyId] = useState("");
  // recordLabel needs the other entity's metas or a non-"name" label field falls back to "#<id>".
  const [otherFields, setOtherFields] = useState<FieldMeta[]>([]);
  useEffect(() => {
    let alive = true;
    listFields(otherEntity)
      .then((fs) => { if (alive) setOtherFields(fs); })
      .catch(() => { /* labels fall back to #id */ });
    return () => { alive = false; };
  }, [otherEntity]);

  // Bumped after a link/unlink so the effect re-reads the linked list.
  const [version, setVersion] = useState(0);
  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const rows = await listRelationships(entity, recordId, rel.name, true);
        if (!alive) return;
        setLinked(rows);
        setErr("");
      } catch (e) {
        if (alive) setErr(e instanceof ApiError ? e.message : t("page.record.related.loadError"));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => { alive = false; };
  }, [entity, recordId, rel.name, version]);
  const reload = () => {
    setLoading(true);
    setVersion((v) => v + 1);
  };

  const openPicker = async () => {
    setPicking(true);
    setCandLoading(true);
    setCandErr("");
    try {
      await ensureAuth();
      const page = await listRecordsPage(otherEntity, "limit=100");
      setCandidates(page.records);
    } catch (e) {
      // A refused or failed read is not "nothing left to link" — say what happened.
      setCandidates([]);
      setCandErr(e instanceof ApiError ? e.message : t("page.record.related.candidatesError"));
    } finally { setCandLoading(false); }
  };

  const link = async (id: string) => {
    setBusyId(id);
    try { await ensureAuth(); await addRelationship(entity, recordId, rel.name, id); reload(); }
    catch (e) { setErr(e instanceof ApiError ? e.message : t("page.record.related.linkError")); }
    finally { setBusyId(""); }
  };
  const unlink = async (id: string) => {
    setBusyId(id);
    try { await ensureAuth(); await removeRelationship(entity, recordId, rel.name, id); reload(); }
    catch (e) { setErr(e instanceof ApiError ? e.message : t("page.record.related.unlinkError")); }
    finally { setBusyId(""); }
  };

  const linkedIds = new Set(linked.map((r) => r.id));
  const unlinkedCandidates = candidates.filter((c) => !linkedIds.has(c.id));

  return (
    <Card
      boundary
      className="slds-m-bottom_small"
      title={panelTitle}
      icon={{ glyph: Link2, name: "linked" }}
      count={loading ? undefined : linked.length}
      actions={canLink && (
        <Button iconLeft={picking ? X : Plus} onClick={() => (picking ? setPicking(false) : openPicker())}>
          {picking ? t("page.record.related.closePicker") : t("page.record.related.link")}
        </Button>
      )}
    >
      {canLink && picking && (
        <div className="slds-box slds-theme_shade slds-m-bottom_small">
          <p className="slds-text-title slds-m-bottom_x-small">
            {t("page.record.related.pickPrompt").replace("{label}", panelTitle)}
          </p>
          {candLoading ? (
            <Loading label={t("page.record.loading")} />
          ) : candErr ? (
            <p className="slds-text-color_error">{candErr}</p>
          ) : unlinkedCandidates.length === 0 ? (
            <p className="slds-text-color_weak">{t("page.record.related.noCandidates")}</p>
          ) : (
            <ul className="slds-has-dividers_bottom-space max-h-56 overflow-y-auto">
              {unlinkedCandidates.map((c) => (
                <li key={c.id} className="slds-item">
                  <Button variant="base" iconLeft={Plus} disabled={busyId === c.id} onClick={() => link(c.id)}>
                    {recordLabel(c, otherFields)}
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {loading && <Loading label={t("page.record.loading")} />}
      {!loading && err && <p className="slds-text-color_error">{err}</p>}
      {!loading && !err && linked.length === 0 && (
        <p className="slds-text-color_weak">
          {canLink ? t("page.record.related.noneLinkedHint") : t("page.record.related.noneLinked")}
        </p>
      )}
      {!loading && !err && linked.length > 0 && (
        <RecordTiles
          items={linked.map((r) => ({
            id: r.id,
            label: recordLabel(r, otherFields),
            href: `/${otherEntity}/${r.id}`,
            action: (
              <ButtonIcon
                icon={X}
                label={t("page.record.related.unlink")}
                variant="border-filled"
                size="x-small"
                hint
                disabled={busyId === r.id}
                onClick={() => unlink(r.id)}
              />
            ),
          }))}
        />
      )}
    </Card>
  );
}
