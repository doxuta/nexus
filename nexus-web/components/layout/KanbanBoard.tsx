"use client";

// KanbanBoard — W9c. The dynamic record board view: groups the loaded records by ONE picklist
// field (default: the first picklist; metadata-driven, no per-entity hard-coding — see
// lib/kanbanModel.pickKanbanField) into columns whose ORDER matches the picklist declaration order.
// Drag a card between columns → optimistic move → PUT /:entity/:id { <field>: newValue } (partial,
// one field) → commit on 200, or roll the card back + toast on 403/409/422.
//
// SLDS 2.264.1 has no kanban blueprint (RESEARCH §5.2), so the board is composed from the ones that
// exist (specs/038 T022): each lane is a `cards` card with its border back (nested-with-boundary,
// it sits inside the object-home card), the cards are `tiles` board tiles
// (ul.slds-has-dividers_around-space > li.slds-item > article.slds-tile.slds-tile_board), the lanes
// scroll sideways in `slds-scrollable_x`, and the lane under a dragged card shows the `drop-zone`
// "Drag Over" state. Behaviour is @dnd-kit.
//
//   • Columns = picklist values (+ a trailing "Chưa đặt" bucket for empty/stale values). The empty
//     bucket is NOT a drop target when the field is required (dropping there would 422 → pointless).
//   • Cards show recordLabel() + up to 3 short scalar fields (from the list columns, skipping the
//     label + grouping fields). The card title links to the detail.
//   • Drag uses a grip handle so the card title stays a plain link — no click/drag conflict.
//     PointerSensor has a 5px activation threshold so a click never starts a stray drag. Keyboard:
//     Space/Enter on the grip picks the card up, Left/Right jump it one lane, Space drops it
//     (lib/dndKeyboard: a keyboard drag has no pointer, so the drop target is the nearest lane).
//   • ACL: dragging is disabled entirely when the user lacks `edit` on the entity (usePermissions);
//     field-level read-only that isn't locally detectable relies on the 403 rollback path.
//   • KANBAN-FULL-FETCH (Wave 4.3): the board fetches PER COLUMN server-side (one filtered query
//     per lane, `field[eq]=value` / `[isnull]=1`, up to 100 cards each) instead of grouping the
//     parent's single loaded page. Lane headers show the SERVER total; a lane holding more than its
//     fetched cap says "100/123". The parent's page is only the instant seed while the lanes load.
//
// The optimistic-move + rollback state machine lives in lib/kanbanModel (moveReducer) so it is
// unit-tested pure; this component only wires it to dnd-kit + the network + toasts.

import { useCallback, useEffect, useMemo, useReducer, useState, type MouseEvent, type ReactNode } from "react";
import clsx from "clsx";
import { GripVertical, Lock, Plus, SquareKanban } from "lucide-react";
import {
  DndContext,
  DragOverlay,
  PointerSensor,
  KeyboardSensor,
  useSensor,
  useSensors,
  useDraggable,
  useDroppable,
  type DragStartEvent,
  type DragEndEvent,
} from "@dnd-kit/core";
import { useQueries, useQueryClient } from "@tanstack/react-query";
import {
  ensureAuth,
  updateRecord,
  listRecordsPage,
  ApiError,
  type ApiRecord,
  type FieldMeta,
  type LayoutListColumn,
  type ListPage,
} from "@/lib/api";
import { t } from "@/lib/i18n";
import { buildLaneQuery } from "@/lib/kanbanLane";
import { recordLabel } from "@/lib/recordLabel";
import { dndAccessibility, jumpToDroppable, pointerOrClosest } from "@/lib/dndKeyboard";
import { FieldDisplay } from "@/components/fields/display";
import { metaToSpec } from "@/lib/fieldSpec";
import { toast } from "@/components/ui/Toaster";
import { usePermissions } from "@/lib/metadata/permissions";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Card } from "@/components/slds/Card";
import { EmptyState } from "@/components/slds/EmptyState";
import { Icon } from "@/components/slds/Icon";
import {
  EMPTY_BUCKET,
  columnsForField,
  columnOf,
  fieldValues,
  groupRecords,
  moveRecordValue,
  moveReducer,
  pickKanbanField,
  type KanbanColumn,
  type MoveState,
} from "@/lib/kanbanModel";

// KANBAN-FULL-FETCH: cards fetched per lane (server cap is 100/request).
const LANE_LIMIT = 100;

const fill = (key: Parameters<typeof t>[0], values: Record<string, string | number>) =>
  t(key).replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));

export interface KanbanBoardProps {
  entity: string;
  records: ApiRecord[];
  fields: FieldMeta[];
  /** Visible list columns → source for the 1-3 scalar fields shown on each card. */
  columns?: LayoutListColumn[];
  loading?: boolean;
  /** True when the list is paginated (server has more than the loaded page) → counts are partial. */
  hasMore?: boolean;
  onOpen?: (id: string) => void;
  onCreate?: () => void;
  /** Sync the moved value back to the parent's records so the table view stays consistent. */
  onRecordsChange?: (records: ApiRecord[]) => void;
  /** Picklist the layout DECLARED to group by (layout.views.list.kanbanField, DAILY-QUEUE #5).
   *  Undefined, or a name that is not a picklist here, falls back to the first picklist. */
  laneFieldName?: string;
  /** The list's active filters (`field[op]=value&…`) and search text — every lane query carries them (#279). */
  filterQuery?: string;
  searchQuery?: string;
}

function usePrefersReducedMotion(): boolean {
  const [reduced, setReduced] = useState(
    () => typeof window !== "undefined" && !!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches,
  );
  useEffect(() => {
    if (typeof window === "undefined" || !window.matchMedia) return;
    const m = window.matchMedia("(prefers-reduced-motion: reduce)");
    const on = () => setReduced(m.matches);
    m.addEventListener("change", on);
    return () => m.removeEventListener("change", on);
  }, []);
  return reduced;
}

function moveErrorMessage(err: unknown, fieldLabel: string): string {
  if (err instanceof ApiError) {
    if (err.status === 403 || err.code === "FORBIDDEN") return fill("page.list.kanban.forbidden", { field: fieldLabel });
    if (err.status === 409) return t("page.list.kanban.conflict");
    if (err.status === 422) return fill("page.list.kanban.invalid", { msg: err.message });
    return fill("page.list.kanban.failedMsg", { msg: err.message || t("page.list.kanban.failed") });
  }
  return t("page.list.kanban.failedRollback");
}

export function KanbanBoard({
  entity,
  records,
  fields,
  columns,
  loading,
  hasMore,
  onOpen,
  onCreate,
  onRecordsChange,
  laneFieldName,
  filterQuery = "",
  searchQuery = "",
}: KanbanBoardProps) {
  const { can } = usePermissions();
  // F3: compute canEdit in render — NEVER put `can` in an effect dependency (identity churns).
  const canEdit = can("edit", entity);
  const reduced = usePrefersReducedMotion();

  const laneField = useMemo(() => pickKanbanField(fields, laneFieldName), [fields, laneFieldName]);
  const boardColumns = useMemo(() => columnsForField(laneField), [laneField]);
  const validValues = useMemo(() => fieldValues(laneField), [laneField]);
  const qc = useQueryClient();

  // KANBAN-FULL-FETCH: one filtered server query per lane. The empty bucket queries [isnull]=1
  // (a rogue value outside the picklist still lands there client-side via columnOf, but can't
  // be queried server-side — acceptable residue, such rows are data-repair cases).
  // DP-4.1: expand relation fields so cards can show relation LABELS.
  const relFieldNames = useMemo(
    () => fields.filter((f) => f.fieldType === "relation").map((f) => f.name),
    [fields],
  );
  const laneQueries = useQueries({
    queries: boardColumns.map((col) => ({
      queryKey: ["kanban", entity, laneField?.name ?? "", col.key, relFieldNames.join(","), filterQuery, searchQuery],
      queryFn: async () => {
        await ensureAuth();
        const q = buildLaneQuery({
          laneField: laneField!.name, col, limit: LANE_LIMIT, expand: relFieldNames, filterQuery, q: searchQuery,
        });
        if (q === null) return { records: [], pagination: { page: 1, limit: LANE_LIMIT, total: 0, totalPages: 1 } } as ListPage;
        return listRecordsPage(entity, q);
      },
      enabled: !!laneField,
      staleTime: 15_000,
      retry: false,
    })),
  });
  const lanesLoaded = laneQueries.some((q) => q.data !== undefined);
  // laneQueries is a new array every render; the memos below must follow the lanes' DATA only (a
  // background refetch flipping isFetching must not reseed the optimistic board mid-drag), so they
  // key on each lane's dataUpdatedAt.
  const laneDataKey = laneQueries.map((q) => q.dataUpdatedAt).join(",");
  // Server totals + fetched counts per lane key (undefined while that lane still loads).
  const laneTotals = useMemo(() => {
    const m: Record<string, { total: number; fetched: number }> = {};
    boardColumns.forEach((col, i) => {
      const d = laneQueries[i]?.data;
      if (d) m[col.key] = { total: d.pagination.total, fetched: d.records.length };
    });
    return m;
    // eslint-disable-next-line react-hooks/exhaustive-deps -- laneQueries identity churns per render; laneDataKey is the real dep
  }, [boardColumns, laneDataKey]);

  // Board dataset: the per-lane fetches once ANY lane answered; the parent's page is only the
  // instant seed before that (no flash of empty board). Once the lanes answer, the board follows
  // their data ALONE: the parent taking a moved card back (onRecordsChange) must not reseed the
  // board from lanes that have not refetched yet — the card jumped back to its old lane until the
  // refetch landed, and a keyboard user's focus went with the unmounted card.
  const laneRecords = useMemo<ApiRecord[]>(() => {
    const seen = new Set<string>();
    const out: ApiRecord[] = [];
    for (const q of laneQueries) {
      for (const r of q.data?.records ?? []) {
        if (!seen.has(r.id)) {
          seen.add(r.id);
          out.push(r);
        }
      }
    }
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps -- see laneTotals note
  }, [laneDataKey]);
  const boardRecords = lanesLoaded ? laneRecords : records;

  // Optimistic state seeded from the board dataset; reseeded (during render, the "adjust state
  // on prop change" pattern) when it refetches.
  const [state, dispatch] = useReducer(moveReducer, { records: boardRecords, pending: null } as MoveState);
  const [seed, setSeed] = useState(boardRecords);
  if (seed !== boardRecords) {
    setSeed(boardRecords);
    dispatch({ type: "reset", records: boardRecords });
  }

  const grouped = useMemo(
    () => groupRecords(state.records, laneField?.name ?? "", boardColumns),
    [state.records, laneField, boardColumns],
  );

  const [activeId, setActiveId] = useState<string | null>(null);
  const moving = state.pending !== null;

  // Up to 3 scalar fields for the card face: sourced from the list columns (then the remaining
  // fields in metadata order). Excludes the grouping field and label-like fields (recordLabel
  // already shows one of these).
  const cardFields = useMemo<FieldMeta[]>(() => {
    // DP-4.1: relations included (lanes fetch with ?expand → {id,label}); picklist included.
    const DISPLAY_TYPES = new Set([
      "currency", "number", "date", "datetime", "email", "boolean", "text", "rating",
      "relation", "picklist", "auto_number",
    ]);
    // Mirrors recordLabel's tier-1 preferred-name fields — kept in sync so the card title (from
    // recordLabel) is never duplicated as a scalar below it.
    const LABEL_FIELDS = new Set([
      "name", "title", "label", "full_name", "display_name", "subject", "code", "email",
    ]);
    const colNames = (columns ?? []).map((c) => c.field);
    const source = [...colNames, ...fields.map((f) => f.name).filter((n) => !colNames.includes(n))];
    const picked: FieldMeta[] = [];
    for (const name of source) {
      if (picked.length >= 3) break;
      if (name === laneField?.name || LABEL_FIELDS.has(name.toLowerCase())) continue;
      const f = fields.find((x) => x.name === name);
      if (!f || !DISPLAY_TYPES.has(f.fieldType)) continue;
      picked.push(f);
    }
    return picked;
  }, [columns, fields, laneField]);

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 5 } }),
    useSensor(KeyboardSensor, { coordinateGetter: jumpToDroppable }),
  );
  const accessibility = dndAccessibility(
    (id) => {
      const card = state.records.find((r) => r.id === String(id));
      return card ? recordLabel(card, fields) : String(id);
    },
    (id) => boardColumns.find((c) => c.key === String(id))?.label ?? String(id),
  );

  const onDragStart = useCallback((e: DragStartEvent) => setActiveId(String(e.active.id)), []);

  const onDragEnd = useCallback(
    (e: DragEndEvent) => {
      setActiveId(null);
      const { active, over } = e;
      if (!over || !laneField) return;
      const cardId = String(active.id);
      const targetCol = String(over.id); // droppable id === column key
      const card = state.records.find((r) => r.id === cardId);
      if (!card) return;
      const currentCol = columnOf(card, laneField.name, validValues);
      if (currentCol === targetCol) return; // dropped back on its own column → no-op
      const isEmptyTarget = targetCol === EMPTY_BUCKET;
      if (isEmptyTarget && laneField.isRequired) return; // guarded: can't clear a required field
      const newValue: string | null = isEmptyTarget ? null : targetCol;

      const snapshot = state.records;
      dispatch({ type: "begin", cardId, fieldName: laneField.name, newValue });
      (async () => {
        try {
          await ensureAuth();
          // Partial PUT — one picklist field. Version is intentionally OMITTED: updateRecord returns
          // no fresh version, so a follow-up drag (e.g. drag-back) would 409 on a stale version;
          // a single-field stage change is inherently low-conflict. 409 is still handled below.
          await updateRecord(entity, cardId, { [laneField.name]: newValue });
          // Write the move into both lanes' cached pages before refetching: the lanes answer one at
          // a time, and each answer reseeds the board from every lane's cache — a stale source lane
          // put the card back where it came from (then nowhere) until its own refetch landed, and a
          // keyboard user's focus went with it. Newest first, like the lanes' updated_at sort.
          const lanePage = (key: string, edit: (p: ListPage) => ListPage) =>
            qc.setQueryData<ListPage>(["kanban", entity, laneField.name, key, relFieldNames.join(","), filterQuery, searchQuery], (old) => old && edit(old));
          lanePage(currentCol, (p) => ({
            ...p,
            records: p.records.filter((r) => r.id !== cardId),
            pagination: { ...p.pagination, total: Math.max(0, p.pagination.total - 1) },
          }));
          lanePage(targetCol, (p) => ({
            ...p,
            records: [{ ...card, data: { ...card.data, [laneField.name]: newValue } }, ...p.records.filter((r) => r.id !== cardId)],
            pagination: { ...p.pagination, total: p.pagination.total + 1 },
          }));
          dispatch({ type: "commit" });
          onRecordsChange?.(moveRecordValue(snapshot, cardId, laneField.name, newValue));
          const colLabel = boardColumns.find((c) => c.key === targetCol)?.label ?? targetCol;
          toast.success(fill("page.list.kanban.moved", { record: recordLabel(card, fields), lane: colLabel }));
          // Refresh the two affected lanes' server counts (KANBAN-FULL-FETCH).
          qc.invalidateQueries({ queryKey: ["kanban", entity, laneField.name] });
        } catch (err) {
          dispatch({ type: "rollback" });
          toast.error(moveErrorMessage(err, laneField.label ?? laneField.name));
        }
      })();
    },
    [entity, laneField, validValues, state.records, boardColumns, fields, onRecordsChange, qc, relFieldNames, filterQuery, searchQuery],
  );

  const activeCard = activeId ? state.records.find((r) => r.id === activeId) ?? null : null;

  // ── No grouping field: honest empty state ──
  if (!laneField) {
    return (
      <div className="slds-border_top slds-p-around_medium">
        <EmptyState icon={SquareKanban} title={t("page.list.kanban.noField")} description={t("page.list.kanban.noFieldHint")} />
      </div>
    );
  }

  // Each lane card sits in its own sized box: SLDS stacks adjacent cards (`.slds-card + .slds-card`
  // gets a top margin), which would drop every lane after the first below it.
  const board = (lanes: ReactNode) => (
    <div className="slds-scrollable_x slds-border_top">
      <div className="flex gap-3 slds-p-around_small">{lanes}</div>
    </div>
  );

  // ── Loading: the lanes themselves, each in the card's loading state ──
  if (loading && state.records.length === 0) {
    return board(
      boardColumns.slice(0, 5).map((c) => (
        <div key={c.key} className="w-72 shrink-0">
          <Card title={c.label} boundary loading />
        </div>
      )),
    );
  }

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={pointerOrClosest}
      accessibility={accessibility}
      onDragStart={onDragStart}
      onDragEnd={onDragEnd}
      onDragCancel={() => setActiveId(null)}
    >
      {/* Honesty notes — read-only mode and/or a seed page still waiting for lane queries */}
      {(!canEdit || (!lanesLoaded && hasMore)) && (
        <div className="slds-grid slds-grid_vertical-align-center slds-p-horizontal_small slds-p-vertical_x-small slds-border_top slds-text-body_small slds-text-color_weak">
          {!canEdit && (
            <span className="slds-grid slds-grid_vertical-align-center slds-m-right_medium">
              <Icon icon={Lock} size="x-small" className="slds-m-right_xx-small" />
              {t("page.list.kanban.readOnly")}
            </span>
          )}
          {!lanesLoaded && hasMore && <span>{t("page.list.kanban.loadingLanes")}</span>}
        </div>
      )}

      {board(
        boardColumns.map((col) => (
          <KanbanLane
            key={col.key}
            column={col}
            records={grouped[col.key] ?? []}
            serverTotal={laneTotals[col.key]?.total}
            fields={fields}
            cardFields={cardFields}
            canEdit={canEdit}
            canDrag={canEdit && !moving}
            dropDisabled={!canEdit || (col.isEmpty && !!laneField.isRequired)}
            activeId={activeId}
            entity={entity}
            onOpen={onOpen}
            onCreate={onCreate}
          />
        )),
      )}

      <DragOverlay dropAnimation={reduced ? null : undefined}>
        {activeCard ? (
          <article className="slds-card slds-card_boundary w-64">
            <div className="slds-p-around_small">
              <CardFace record={activeCard} fields={fields} cardFields={cardFields} />
            </div>
          </article>
        ) : null}
      </DragOverlay>
    </DndContext>
  );
}

// ── Lane ────────────────────────────────────────────────────────────────────────────
function KanbanLane({
  column,
  records,
  serverTotal,
  fields,
  cardFields,
  canEdit,
  canDrag,
  dropDisabled,
  activeId,
  entity,
  onOpen,
  onCreate,
}: {
  column: KanbanColumn;
  records: ApiRecord[];
  /** Server-side count for this lane (KANBAN-FULL-FETCH); undefined while its query loads. */
  serverTotal?: number;
  fields: FieldMeta[];
  cardFields: FieldMeta[];
  canEdit: boolean;
  /** False while a move is saving (one pending move at a time). */
  canDrag: boolean;
  dropDisabled: boolean;
  activeId: string | null;
  entity: string;
  onOpen?: (id: string) => void;
  onCreate?: () => void;
}) {
  const { setNodeRef, isOver } = useDroppable({ id: column.key, disabled: dropDisabled });
  const capped = serverTotal !== undefined && serverTotal > records.length;
  const count = capped ? `${records.length}/${serverTotal}` : String(serverTotal ?? records.length);

  return (
    <div className="w-72 shrink-0">
      <Card
        title={`${column.label} (${count})`}
        boundary
        footer={
          onCreate ? (
            <Button variant="base" iconLeft={Plus} onClick={onCreate}>
              {t("page.list.kanban.add")}
            </Button>
          ) : undefined
        }
      >
        {capped && (
          <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_x-small">
            {fill("page.list.kanban.capped", { shown: records.length, total: serverTotal })}
          </p>
        )}
        {/* Droppable body — wraps the list AND the empty state, with a min-height so short/empty
            lanes still accept a drop; the lane under the pointer shows drop-zone "Drag Over". */}
        <div
          ref={setNodeRef}
          className={clsx("slds-scrollable_y min-h-[7.5rem] max-h-[calc(100vh-24rem)]", isOver && !dropDisabled && "slds-drop-zone slds-drop-zone_drag")}
        >
          {records.length === 0 ? (
            <p className="slds-text-body_small slds-text-color_weak slds-text-align_center slds-p-vertical_large">{t("page.list.kanban.empty")}</p>
          ) : (
            <ul className="slds-has-dividers_around-space">
              {records.map((r) => (
                <KanbanCard
                  key={r.id}
                  record={r}
                  fields={fields}
                  cardFields={cardFields}
                  grip={canEdit}
                  disabled={!canDrag}
                  lifted={activeId === r.id}
                  href={`/${entity}/${r.id}`}
                  onOpen={onOpen}
                />
              ))}
            </ul>
          )}
        </div>
      </Card>
    </div>
  );
}

// ── Card (board tile) ───────────────────────────────────────────────────────────────
function KanbanCard({
  record,
  fields,
  cardFields,
  grip,
  disabled,
  lifted,
  href,
  onOpen,
}: {
  record: ApiRecord;
  fields: FieldMeta[];
  cardFields: FieldMeta[];
  /** The drag handle shows for editors. It stays (aria-disabled) while a move saves, so the focus
   *  dnd-kit returns after a keyboard drop lands on it and not on the title link. */
  grip: boolean;
  disabled: boolean;
  lifted: boolean;
  href: string;
  onOpen?: (id: string) => void;
}) {
  const { attributes, listeners, setNodeRef, setActivatorNodeRef, isDragging } = useDraggable({ id: record.id, disabled });
  const label = recordLabel(record, fields);
  const open = (e: MouseEvent<HTMLAnchorElement>) => {
    if (!onOpen || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    onOpen(record.id);
  };

  return (
    // While lifted, the card keeps its slot but hides (the drag overlay is the moving copy).
    <li ref={setNodeRef} className={clsx("slds-item", (lifted || isDragging) && "slds-hidden")}>
      <article className="slds-tile slds-tile_board slds-hint-parent">
        <div className="slds-grid slds-grid_align-spread slds-has-flexi-truncate">
          <h3 className="slds-tile__title slds-truncate" title={label}>
            <a href={href} onClick={open}>
              {label}
            </a>
          </h3>
          {grip && (
            <div className="slds-shrink-none">
              <ButtonIcon
                ref={setActivatorNodeRef}
                {...attributes}
                {...listeners}
                icon={GripVertical}
                size="x-small"
                hint
                label={t("page.list.kanban.drag")}
                className="cursor-grab touch-none"
              />
            </div>
          )}
        </div>
        <CardDetail record={record} cardFields={cardFields} />
      </article>
    </li>
  );
}

// Card face for the drag overlay — the same tile without the link and grip.
function CardFace({ record, fields, cardFields }: { record: ApiRecord; fields: FieldMeta[]; cardFields: FieldMeta[] }) {
  const label = recordLabel(record, fields);
  return (
    <article className="slds-tile slds-tile_board">
      <h3 className="slds-tile__title slds-truncate" title={label}>
        {label}
      </h3>
      <CardDetail record={record} cardFields={cardFields} />
    </article>
  );
}

function CardDetail({ record, cardFields }: { record: ApiRecord; cardFields: FieldMeta[] }) {
  const shown = cardFields.filter((f) => record.data[f.name] != null && record.data[f.name] !== "");
  if (shown.length === 0) return null;
  return (
    <div className="slds-tile__detail">
      {shown.map((f) => (
        <p key={f.name} className="slds-truncate" title={f.label ?? f.name}>
          <FieldDisplay field={metaToSpec(f)} value={record.data[f.name]} />
        </p>
      ))}
    </div>
  );
}
