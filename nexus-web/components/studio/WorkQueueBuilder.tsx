"use client";

// WorkQueueBuilder — VIEC-HOM-NAY Studio screen (../specs/012-work-queues/spec.md §6): manage the role-based
// queues end-users see on the home screen. A queue is metadata: entity + filters (ExpressionBuilder,
// incl. Wave 3.6 relative tokens) + visibility role + a v1 action verb. Validation is server-side
// (validator-parity) — this screen surfaces the 422s verbatim next to the form.
//
// SLDS 2.264.1 (specs/038 T044, RESEARCH §5 `hang-doi`): the ObjectHomePage template
// (`page-headers/object-home` + `data-tables` with a row action menu), the editor in a right-docked
// `panels/base` beside the list, the filters as `expression`, the entity as a `combobox`
// (autocomplete — type to find it among every table), the other choices as `select`, and delete
// confirmed in `modals/base`.
import type { DirectApplyGate } from "@/components/studio/useDirectApplyGate";
import { useEffect, useId, useMemo, useState } from "react";
import Link from "next/link";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { ListTodo, Plus } from "lucide-react";
import {
  ApiError, ensureAuth, listEntities, listFields, listRoles, listWorkQueuesAdmin,
  createWorkQueue, updateWorkQueue, deleteWorkQueue,
  type WorkQueue, type WorkQueueInput,
} from "@/lib/api";
import { newFilterRow, type FilterField, type FilterRow } from "@/lib/filterModel";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { toast } from "@/components/ui/Toaster";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { Combobox } from "@/components/slds/Combobox";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { ExpressionBuilder } from "@/components/slds/ExpressionBuilder";
import { Input } from "@/components/slds/Input";
import { MenuItem } from "@/components/slds/Menu";
import { Panel } from "@/components/slds/Panel";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Toggle } from "@/components/slds/Toggle";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { InlineConfirm } from "./InlineConfirm";
import { OpenRowId, RowOpenButton } from "./RowOpenButton";
import { splitByScope } from "@/lib/appScope";

type Draft = WorkQueueInput & { id?: string };

const EMPTY: WorkQueueInput = {
  name: "", label: "", entityName: "", filters: [], actionVerb: "open", isActive: true, position: 0,
};

// Action allowlist (mirrors workQueueActions in the backend): the two deep-link actions + "claim"
// (specs/043 S5: counts the work items waiting in the pool, opens the "Chờ nhận" tab).
const VERBS = ["open", "approve", "claim"] as const;
const verbLabel = (v: string) => (VERBS as readonly string[]).includes(v) ? t(`page.workQueue.verb.${v}` as I18nKey) : v;

export interface WorkQueueBuilderProps {
  /** Tables in scope (the page's `?app=`); null/absent = every table of the project. */
  entityNames?: Set<string> | null;
  /** The page's `?bang=`: narrows the scope to this one table. */
  entity?: string;
  /** The page is still resolving `?app=`: list nothing yet. */
  scopeLoading?: boolean;
  /** Where "Xem tất cả" goes (the same page without `?app=`). */
  showAllHref?: string;
  /** Queue to open first (`hang-doi/<id>`); dropped when the scope hides it or it does not exist. */
  initialQueueId?: string | null;
  /** Called after the open queue changes (null = none open), so the page keeps the URL in step. */
  onSelect?: (id: string | null) => void;
  /** Delivered project: label + in-place question before a save that has no draft (useDirectApplyGate). */
  applyGate?: DirectApplyGate;
}

export function WorkQueueBuilder({ entityNames, entity, scopeLoading = false, showAllHref, initialQueueId = null, onSelect, applyGate }: WorkQueueBuilderProps = {}) {
  const qc = useQueryClient();
  const [authed, setAuthed] = useState(false);
  useEffect(() => { ensureAuth().then(() => setAuthed(true)).catch(() => {}); }, []);

  const { data: allQueues = [], isLoading: loadingQueues } = useQuery({
    queryKey: ["admin-work-queues"], queryFn: listWorkQueuesAdmin, enabled: authed,
  });
  // Soft filter (INV-2): a queue of another app's table is hidden, so it can neither be opened nor deleted from here.
  const scope = useMemo(() => (entity ? new Set([entity]) : entityNames ?? null), [entity, entityNames]);
  const queues = useMemo(() => splitByScope(allQueues, scope, (q) => q.entityName).visible, [allQueues, scope]);
  const hiddenQueues = allQueues.length - queues.length;
  const isLoading = loadingQueues || scopeLoading;
  const { data: entities = [] } = useQuery({ queryKey: ["entities"], queryFn: listEntities, enabled: authed });
  const { data: roles = [] } = useQuery({ queryKey: ["roles"], queryFn: listRoles, enabled: authed });

  // Editor state: null = closed; {} with id = edit; without id = create.
  const [editing, setEditing] = useState<Draft | null>(null);
  const [err, setErr] = useState("");
  const [deleting, setDeleting] = useState<WorkQueue | null>(null);
  const [wanted, setWanted] = useState(initialQueueId);
  // panels docs: the buttons that open the docked editor name it (aria-controls) and say whether it
  // shows their content (aria-expanded).
  const panelId = useId();
  const editingId = editing?.id;
  const creating = !!editing && !editing.id;

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["admin-work-queues"] });
    qc.invalidateQueries({ queryKey: ["work-queues"] });
  };

  const save = useMutation({
    mutationFn: async (input: Draft) => {
      const { id, ...body } = input;
      return id ? updateWorkQueue(id, body) : createWorkQueue(body);
    },
    onSuccess: () => {
      toast.success(t("page.workQueue.saved"));
      setEditing(null);
      setErr("");
      invalidate();
    },
    onError: (e) => setErr(e instanceof ApiError ? e.message : t("page.workQueue.saveFailed")),
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteWorkQueue(id),
    onSuccess: () => { toast.success(t("page.workQueue.deleted")); setDeleting(null); invalidate(); },
    onError: (e) => { toast.error(e instanceof ApiError ? e.message : t("page.workQueue.deleteFailed")); setDeleting(null); },
  });

  const openEdit = (q: WorkQueue) => {
    setEditing({
      id: q.id, name: q.name, label: q.label, entityName: q.entityName,
      roleId: q.roleId, filters: q.filters ?? [], sortField: q.sortField,
      sortOrder: q.sortOrder, actionVerb: q.actionVerb, position: q.position,
      isActive: q.isActive,
    });
    setErr("");
  };
  const openCreate = () => { setEditing({ ...EMPTY }); setErr(""); };

  // `hang-doi/<id>`: open that queue once the list is in; an id the scope hides (or that does not exist) opens nothing.
  if (wanted && !isLoading && authed) {
    const q = queues.find((x) => x.id === wanted);
    setWanted(null);
    if (q) openEdit(q);
  }
  const openId = editing?.id ?? null;
  useEffect(() => {
    if (authed && !isLoading && !wanted) onSelect?.(openId);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- onSelect is the page's callback, not a trigger
  }, [openId, authed, isLoading, wanted]);

  const roleLabel = (id?: string) => {
    if (!id) return t("page.workQueue.everyone");
    const r = roles.find((x) => x.id === id);
    return r?.label ?? r?.name ?? t("page.workQueue.oneRole");
  };

  const columns = useMemo<ColumnDef<WorkQueue>[]>(() => [
    {
      id: "label",
      accessorKey: "label",
      header: t("page.workQueue.col.queue"),
      cell: ({ row }) => (
        <RowOpenButton rowId={row.original.id} label={row.original.label} panelId={panelId} onOpen={() => openEdit(row.original)} />
      ),
    },
    { id: "name", accessorKey: "name", header: t("page.workQueue.col.code") },
    { id: "entityName", accessorKey: "entityName", header: t("page.workQueue.col.entity") },
    {
      id: "role",
      header: t("page.workQueue.col.role"),
      accessorFn: (q) => roleLabel(q.roleId),
    },
    { id: "actionVerb", accessorKey: "actionVerb", header: t("page.workQueue.col.action") },
    {
      id: "isActive",
      header: t("page.workQueue.col.status"),
      accessorFn: (q) => (q.isActive ? t("page.workQueue.active") : t("page.workQueue.inactive")),
      cell: ({ row }) =>
        row.original.isActive
          ? <Badge theme="success">{t("page.workQueue.active")}</Badge>
          : <Badge variant="lightest">{t("page.workQueue.inactive")}</Badge>,
    },
  // openEdit/roleLabel read state setters + the roles list; rebuilding with roles is enough.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  ], [roles]);

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden">
      <div className="min-w-0 flex-1 overflow-y-auto p-4">
        {deleting && (
          <div className="slds-m-bottom_small">
            <InlineConfirm
              tone="error"
              message={tf("page.workQueue.deleteBody", { label: deleting.label })}
              confirmLabel={t("page.workQueue.delete")}
              busy={remove.isPending}
              onConfirm={() => remove.mutate(deleting.id)}
              onCancel={() => setDeleting(null)}
            />
          </div>
        )}
        <ObjectHomePage
          icon={{ glyph: ListTodo, name: "work-queue" }}
          label={t("page.workQueue.label")}
          title={t("page.workQueue.title")}
          meta={isLoading ? t("page.workQueue.intro") : (
            <>
              {`${tf("page.workQueue.meta", { n: queues.length })} · ${t("page.workQueue.intro")}`}
              {hiddenQueues > 0 && showAllHref && (
                <>{` · ${tf("studio.build.automation.queue.hidden", { n: hiddenQueues })} · `}<Link href={showAllHref}>{t("studio.build.data.showAll")}</Link></>
              )}
            </>
          )}
          actions={[{ label: t("page.workQueue.create"), onSelect: openCreate, controls: panelId, expanded: creating }]}
          empty={!isLoading && queues.length === 0 && (
            <EmptyState
              className="slds-p-bottom_medium"
              icon={ListTodo}
              title={t("page.workQueue.emptyTitle")}
              description={t("page.workQueue.emptyDesc")}
              action={
                <Button variant="brand" iconLeft={Plus} aria-controls={panelId} aria-expanded={creating} onClick={openCreate}>
                  {t("page.workQueue.create")}
                </Button>
              }
            />
          )}
        >
          <OpenRowId.Provider value={editingId}>
          <DataTable
            label={t("page.workQueue.grid")}
            columns={columns}
            data={queues}
            getRowId={(q) => q.id}
            sortable
            loading={isLoading}
            rowActions={(q) => (
              <>
                <MenuItem label={t("page.workQueue.edit")} onSelect={() => openEdit(q)} />
                <MenuItem label={t("page.workQueue.delete")} onSelect={() => setDeleting(q)} />
              </>
            )}
          />
          </OpenRowId.Provider>
        </ObjectHomePage>
      </div>

      <Panel
        id={panelId}
        open={!!editing}
        position="right"
        size="large"
        title={editing ? (editing.id ? t("page.workQueue.editTitle") : t("page.workQueue.createTitle")) : ""}
        onClose={() => { setEditing(null); setErr(""); }}
      >
        {editing && applyGate?.notice && <p className="slds-m-bottom_small">{applyGate.notice}</p>}
        {editing && applyGate?.confirm && <div className="slds-m-bottom_small">{applyGate.confirm}</div>}
        {editing && (
          <QueueEditor
            key={editing.id ?? "new"}
            value={editing}
            entities={entities.map((e) => ({ name: e.name, label: e.label }))}
            roles={roles.map((r) => ({ id: r.id, label: r.label ?? r.name }))}
            error={err}
            saving={save.isPending}
            onCancel={() => { setEditing(null); setErr(""); }}
            onSave={(v) => (applyGate ? applyGate.ask(() => save.mutate(v)) : save.mutate(v))}
          />
        )}
      </Panel>

    </div>
  );
}

// ── Editor (docked panel body) ───────────────────────────────────────────────────────────────

function QueueEditor({
  value, entities, roles, error, saving, onCancel, onSave,
}: {
  value: Draft;
  entities: { name: string; label: string }[];
  roles: { id: string; label: string }[];
  error: string;
  saving: boolean;
  onCancel: () => void;
  onSave: (v: Draft) => void;
}) {
  const [form, setForm] = useState(value);
  const isClaim = form.actionVerb === "claim";
  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setForm((f) => ({ ...f, [k]: v }));
  // Condition rows keep their own ids (ExpressionBuilder focus + keys); queue filters carry none —
  // they are mapped back on save.
  const [rows, setRows] = useState<FilterRow[]>(() =>
    (value.filters ?? []).map((f) => ({ ...newFilterRow(f.field, f.op), value: f.value ?? "" })));

  // Load the chosen entity's fields for the ExpressionBuilder (same shape the list screen feeds it).
  const { data: fields = [] } = useQuery({
    queryKey: ["fields", form.entityName],
    queryFn: () => listFields(form.entityName),
    enabled: !!form.entityName,
  });
  const filterFields: FilterField[] = useMemo(
    () => fields.map((f) => ({ name: f.name, label: f.label, fieldType: f.fieldType, options: f.options })),
    [fields],
  );

  return (
    <div className="flex flex-col gap-3">
      <div className="grid grid-cols-2 gap-3">
        <Input label={t("page.workQueue.f.label")} value={form.label} onChange={(e) => set("label", e.target.value)}
          placeholder={t("page.workQueue.f.labelPh")} />
        <Input label={t("page.workQueue.f.name")} value={form.name} onChange={(e) => set("name", e.target.value)}
          placeholder={t("page.workQueue.f.namePh")} disabled={!!form.id} />
      </div>

      <Combobox
        variant="autocomplete"
        label={t("page.workQueue.f.entity")}
        placeholder={t("page.workQueue.f.entityPh")}
        options={entities.map((e) => ({ value: e.name, label: `${e.label} (${e.name})` }))}
        value={form.entityName || null}
        onChange={(v) => {
          setForm((f) => ({ ...f, entityName: v ?? "", filters: [], sortField: undefined }));
          setRows([]);
        }}
      />

      {form.entityName && !isClaim && (
        // form-element fieldset: the builder's own h2 title is page-sized, too loud in a panel.
        <fieldset className="slds-form-element">
          <legend className="slds-form-element__legend slds-form-element__label">{t("page.workQueue.f.filters")}</legend>
          <div className="slds-form-element__control">
            <ExpressionBuilder title={null} fields={filterFields} value={rows} onChange={setRows} />
          </div>
        </fieldset>
      )}

      <div className="grid grid-cols-2 gap-3">
        <Select label={t("page.workQueue.f.role")} placeholder={t("page.workQueue.everyone")}
          value={form.roleId ?? ""} onChange={(e) => set("roleId", e.target.value || undefined)}
          options={roles.map((r) => ({ value: r.id, label: r.label }))} />
        <Select label={t("page.workQueue.f.verb")} placeholder={false}
          value={form.actionVerb ?? "open"}
          onChange={(e) => {
            // The server refuses a filter or a sort on a claim queue (WORKQUEUE_CLAIM_NO_FILTER): drop them here.
            if (e.target.value === "claim") { setRows([]); setForm((f) => ({ ...f, actionVerb: "claim", filters: [], sortField: undefined, sortOrder: "asc" })); }
            else set("actionVerb", e.target.value);
          }}
          options={VERBS.map((v) => ({ value: v, label: verbLabel(v) }))} />
      </div>

      {isClaim && <p className="slds-text-color_weak">{t("page.workQueue.claimNote")}</p>}
      {!isClaim && <div className="grid grid-cols-2 gap-3">
        <Select label={t("page.workQueue.f.sortField")} placeholder={t("page.workQueue.sortDefault")}
          value={form.sortField ?? ""} onChange={(e) => set("sortField", e.target.value || undefined)}
          // RM-09: server chỉ nhận field sortable — lỗi trả 422 rõ ràng nếu chọn sai.
          options={[
            { value: "created_at", label: t("page.workQueue.sort.created") },
            { value: "updated_at", label: t("page.workQueue.sort.updated") },
            ...filterFields
              .filter((f) => ["date", "datetime", "number", "currency", "rating"].includes(f.fieldType))
              .map((f) => ({ value: f.name, label: f.label ?? f.name })),
          ]} />
        <Select label={t("page.workQueue.f.sortOrder")} placeholder={false}
          value={form.sortOrder ?? "asc"} onChange={(e) => set("sortOrder", e.target.value)}
          options={[
            { value: "asc", label: t("page.workQueue.order.asc") },
            { value: "desc", label: t("page.workQueue.order.desc") },
          ]} />
      </div>}

      <div className="grid grid-cols-2 gap-3">
        <Input type="number" label={t("page.workQueue.f.position")} value={form.position ?? 0}
          onChange={(e) => set("position", Number(e.target.value) || 0)} />
        <Toggle label={t("page.workQueue.f.status")} onText={t("page.workQueue.active")} offText={t("page.workQueue.inactive")}
          checked={form.isActive !== false} onChange={(e) => set("isActive", e.target.checked)} />
      </div>

      {/* Footer pinned to the bottom of the scrolling panel (same bar as the field inspector). The
          server's 422 sits in it, right above the buttons: in view wherever the form was scrolled to. */}
      <div className="slds-theme_default slds-border_top sticky bottom-0 flex flex-col gap-2 py-3">
        {error && <ScopedNotification theme="error">{error}</ScopedNotification>}
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button onClick={onCancel}>{t("action.cancel")}</Button>
          <Button variant="brand" disabled={saving || !form.label || !form.name || !form.entityName}
            onClick={() => onSave({ ...form, filters: isClaim ? [] : rows.map((r) => ({ field: r.field, op: r.op, value: r.value })) })}>
            {t("page.workQueue.save")}
          </Button>
        </div>
      </div>
    </div>
  );
}
