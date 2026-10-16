"use client";

// InboundHookScreen — CONG-TIN-HIEU Studio screen (../specs/015-inbound-hooks/spec.md §7): manage the
// per-tenant signed inbound webhooks. A hook is metadata (entity + payload→field mapping +
// optional unique upsert key); the partner posts JSON to the shown URL with an HMAC header.
// Validation is server-side (HOOK_INVALID) and surfaced verbatim.
//
// SLDS 2.264.1 (specs/038 T044, RESEARCH §5 `webhook`): the ObjectHomePage template
// (`page-headers/object-home` + `data-tables` with a row action menu); the editor and the recent
// deliveries share one right-docked `panels/base` beside the list. The editor is `form-element`s —
// the hook URL as a readonly `input` with a copy button, `select`s, a checkbox — and the server's
// 422 / the HMAC hint are `scoped-notifications`. Rotate and delete confirm in `modals/base`.
import { useEffect, useId, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { Copy, KeyRound, Plus, Radio, X } from "lucide-react";
import {
  ApiError, ensureAuth, listEntities, listFields, listInboundHooks, listHookDeliveries,
  createInboundHook, updateInboundHook, deleteInboundHook, rotateInboundHookSecret,
  inboundHookUrl, type InboundHook, type InboundHookInput, type HookMapping,
  type HookPaymentMatch,
} from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Checkbox } from "@/components/slds/Checkbox";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { MenuItem } from "@/components/slds/Menu";
import { Panel } from "@/components/slds/Panel";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { Spinner } from "@/components/slds/Spinner";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { InlineConfirm } from "./InlineConfirm";
import { OpenRowId, RowOpenButton } from "./RowOpenButton";

type Draft = InboundHookInput & { id?: string };
type EntityOption = { name: string; label: string };

const EMPTY: InboundHookInput = { name: "", entityName: "", mapping: [{ source: "", field: "" }], isActive: true };

// The clipboard API throws (or is absent) outside a secure context — say so instead of a false "copied".
async function copyText(text: string, done: string) {
  try {
    await navigator.clipboard.writeText(text);
    toast.success(done);
  } catch {
    toast.error(t("page.inboundHook.copyFailed"));
  }
}
const copyUrl = (url: string) => copyText(url, t("page.inboundHook.copied"));

/** `initialHookId` opens that hook's editor once the list is in; `onSelect` reports the open hook (null = none) so the page can mirror it in the address. */
export function InboundHookScreen({ initialHookId, onSelect, gate }: { initialHookId?: string; onSelect?: (id: string | null) => void; gate?: (save: () => void) => void } = {}) {
  const qc = useQueryClient();
  const [authed, setAuthed] = useState(false);
  useEffect(() => { ensureAuth().then(() => setAuthed(true)).catch(() => {}); }, []);

  const { data: hooks = [], isLoading, isSuccess } = useQuery({
    queryKey: ["inbound-hooks"], queryFn: listInboundHooks, enabled: authed,
  });
  const { data: entities = [] } = useQuery({ queryKey: ["entities"], queryFn: listEntities, enabled: authed });

  // The docked panel shows either the editor or one hook's recent deliveries — never both.
  const [editing, setEditing] = useState<Draft | null>(null);
  const [inspecting, setInspecting] = useState<InboundHook | null>(null);
  const [err, setErr] = useState("");
  const [deleting, setDeleting] = useState<InboundHook | null>(null);
  const [rotating, setRotating] = useState<InboundHook | null>(null);
  // panels docs: the buttons that open the docked panel name it (aria-controls) and say whether it
  // shows their content (aria-expanded).
  const panelId = useId();
  const editingId = editing?.id;
  const creating = !!editing && !editing.id;

  // A key is shown from the moment it is made until the maker hides it or leaves the page — it lives in state only.
  const [shownKey, setShownKey] = useState<{ name: string; secret: string; rotated: boolean } | null>(null);

  // The memoised columns call openEdit once; the ref keeps them on the current onSelect.
  const onSelectRef = useRef(onSelect);
  useEffect(() => { onSelectRef.current = onSelect; });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["inbound-hooks"] });

  const save = useMutation({
    mutationFn: async (input: Draft) => {
      const { id, ...body } = input;
      return id ? updateInboundHook(id, body) : createInboundHook(body);
    },
    onSuccess: (hook, vars) => {
      if (vars.id) toast.success(t("page.inboundHook.saved"));
      else setShownKey({ name: hook.name, secret: hook.secret, rotated: false });
      setEditing(null);
      onSelectRef.current?.(null);
      setErr("");
      invalidate();
    },
    onError: (e) => setErr(e instanceof ApiError ? e.message : t("page.inboundHook.saveFailed")),
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteInboundHook(id),
    onSuccess: () => { toast.success(t("page.inboundHook.deleted")); setDeleting(null); invalidate(); },
    onError: () => toast.error(t("page.inboundHook.deleteFailed")),
  });

  const rotate = useMutation({
    mutationFn: (id: string) => rotateInboundHookSecret(id),
    onSuccess: (hook) => { setShownKey({ name: hook.name, secret: hook.secret, rotated: true }); setRotating(null); invalidate(); },
    onError: () => toast.error(t("page.inboundHook.rotateFailed")),
  });

  const openEdit = (h: InboundHook) => {
    onSelectRef.current?.(h.id);
    setInspecting(null);
    setEditing({
      id: h.id, name: h.name, entityName: h.entityName,
      mapping: h.mapping.length ? h.mapping : [{ source: "", field: "" }],
      matchField: h.matchField, paymentMatch: h.paymentMatch, isActive: h.isActive,
    });
    setErr("");
  };
  const openCreate = () => {
    setInspecting(null);
    setEditing({ ...EMPTY, mapping: [{ source: "", field: "" }] });
    setErr("");
  };
  const openDeliveries = (h: InboundHook) => { setEditing(null); setErr(""); setInspecting(h); };
  const closePanel = () => { setEditing(null); setInspecting(null); setErr(""); onSelectRef.current?.(null); };

  // Deep link: open the hook named in the address once, when the list arrives (a stranger's id opens nothing).
  const deepLinked = useRef(false);
  useEffect(() => {
    if (deepLinked.current || !initialHookId || !isSuccess) return;
    deepLinked.current = true;
    const h = hooks.find((x) => x.id === initialHookId);
    // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot sync from the address into the panel
    if (h) openEdit(h);
  }, [initialHookId, isSuccess, hooks]);

  const columns = useMemo<ColumnDef<InboundHook>[]>(() => [
    {
      id: "name",
      accessorKey: "name",
      header: t("page.inboundHook.col.name"),
      cell: ({ row }) => (
        <RowOpenButton rowId={row.original.id} label={row.original.name} panelId={panelId} onOpen={() => openEdit(row.original)} />
      ),
    },
    {
      id: "url",
      header: t("page.inboundHook.col.url"),
      accessorFn: (h) => inboundHookUrl(h.name),
      cell: ({ row }) => {
        const url = inboundHookUrl(row.original.name);
        return (
          <div className="flex min-w-0 items-center gap-1">
            <span className="slds-truncate slds-text-font_monospace" title={url}>{url}</span>
            <ButtonIcon icon={Copy} size="small" label={tf("page.inboundHook.copyUrlFor", { name: row.original.name })}
              onClick={() => void copyUrl(url)} />
          </div>
        );
      },
    },
    { id: "entityName", accessorKey: "entityName", header: t("page.inboundHook.col.entity") },
    {
      id: "matchField",
      header: t("page.inboundHook.col.match"),
      accessorFn: (h) => h.matchField || t("page.inboundHook.alwaysCreate"),
    },
    {
      id: "isActive",
      header: t("page.inboundHook.col.status"),
      accessorFn: (h) => (h.isActive ? t("page.inboundHook.active") : t("page.inboundHook.inactive")),
      cell: ({ row }) =>
        row.original.isActive
          ? <Badge theme="success">{t("page.inboundHook.active")}</Badge>
          : <Badge variant="lightest">{t("page.inboundHook.inactive")}</Badge>,
    },
  // openEdit only calls state setters and panelId (useId) is stable: the columns never rebuild.
  ], [panelId]);

  const savedName = editing?.id ? hooks.find((h) => h.id === editing.id)?.name : undefined;
  const panelTitle = editing
    ? (editing.id ? t("page.inboundHook.editTitle") : t("page.inboundHook.createTitle"))
    : inspecting ? tf("page.inboundHook.deliveriesTitle", { name: inspecting.name }) : "";

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden">
      <div className="min-w-0 flex-1 overflow-y-auto p-4">
        <ObjectHomePage
          icon={{ glyph: Radio, name: "data-streams" }}
          label={t("page.inboundHook.label")}
          title={t("page.inboundHook.title")}
          meta={isLoading ? t("page.inboundHook.intro") : `${tf("page.inboundHook.meta", { n: hooks.length })} · ${t("page.inboundHook.intro")}`}
          actions={[{ label: t("page.inboundHook.create"), onSelect: openCreate, controls: panelId, expanded: creating }]}
          empty={!isLoading && hooks.length === 0 && (
            <EmptyState
              className="slds-p-bottom_medium"
              icon={Radio}
              title={t("page.inboundHook.emptyTitle")}
              description={t("page.inboundHook.emptyDesc")}
              action={
                <Button variant="brand" iconLeft={Plus} aria-controls={panelId} aria-expanded={creating} onClick={openCreate}>
                  {t("page.inboundHook.create")}
                </Button>
              }
            />
          )}
        >
          {shownKey && <KeyOnce value={shownKey} onDone={() => setShownKey(null)} />}
          {rotating && (
            <div className="slds-m-bottom_small">
              <InlineConfirm tone="warning" message={tf("studio.build.automation.hook.rotateAsk", { name: rotating.name })}
                confirmLabel={t("studio.build.automation.hook.rotateGo")} busy={rotate.isPending}
                onConfirm={() => rotate.mutate(rotating.id)} onCancel={() => setRotating(null)} />
            </div>
          )}
          {deleting && (
            <div className="slds-m-bottom_small">
              <InlineConfirm tone="error" message={tf("studio.build.automation.hook.deleteAsk", { name: deleting.name })}
                confirmLabel={t("studio.build.automation.hook.deleteGo")} busy={remove.isPending}
                onConfirm={() => remove.mutate(deleting.id)} onCancel={() => setDeleting(null)} />
            </div>
          )}
          <OpenRowId.Provider value={editingId}>
          <DataTable
            label={t("page.inboundHook.grid")}
            columns={columns}
            data={hooks}
            getRowId={(h) => h.id}
            sortable
            loading={isLoading}
            rowActions={(h) => (
              <>
                <MenuItem label={t("page.inboundHook.deliveries")} onSelect={() => openDeliveries(h)} />
                <MenuItem label={t("page.inboundHook.rotate")} onSelect={() => setRotating(h)} />
                <MenuItem label={t("page.inboundHook.edit")} onSelect={() => openEdit(h)} />
                <MenuItem label={t("page.inboundHook.delete")} onSelect={() => setDeleting(h)} />
              </>
            )}
          />
          </OpenRowId.Provider>
        </ObjectHomePage>
      </div>

      <Panel id={panelId} open={!!editing || !!inspecting} position="right" size="large" title={panelTitle} onClose={closePanel}>
        {editing && (
          <HookEditor
            key={editing.id ?? "new"}
            value={editing}
            url={savedName ? inboundHookUrl(savedName) : undefined}
            entities={entities.map((e) => ({ name: e.name, label: e.label }))}
            err={err}
            saving={save.isPending}
            onChange={setEditing}
            onSave={() => (gate ?? ((go: () => void) => go()))(() => save.mutate(editing))}
            onCancel={closePanel}
          />
        )}
        {!editing && inspecting && <Deliveries key={inspecting.id} hook={inspecting} />}
      </Panel>

    </div>
  );
}

// ── the one-time key, shown in the page (pattern "Kết quả sau một việc") ───────────────────────────

function KeyOnce({ value, onDone }: { value: { name: string; secret: string; rotated: boolean }; onDone: () => void }) {
  return (
    <div className="slds-m-bottom_small" role="status">
      <ScopedNotification theme="success" icon={KeyRound}>
        {tf(value.rotated ? "studio.build.automation.hook.keyTitleRotated" : "studio.build.automation.hook.keyTitleNew", { name: value.name })}{" "}
        {t("studio.build.automation.hook.keyOnce")}
      </ScopedNotification>
      <div className="slds-m-top_small flex items-end gap-2">
        <div className="min-w-0 flex-1">
          <Input label={t("studio.build.automation.hook.keyLabel")} readOnly value={value.secret} className="slds-text-font_monospace"
            onFocus={(e) => e.currentTarget.select()} />
        </div>
        <ButtonIcon variant="border-filled" icon={Copy} label={t("studio.build.automation.hook.keyCopy")}
          onClick={() => void copyText(value.secret, t("studio.build.automation.hook.keyCopied"))} />
        <Button variant="brand" onClick={onDone}>{t("studio.build.automation.hook.keyDone")}</Button>
      </div>
    </div>
  );
}

// ── Editor (docked panel body) ───────────────────────────────────────────────────────────────

function HookEditor({ value, url, entities, err, saving, onChange, onSave, onCancel }: {
  value: Draft;
  /** Receive URL of the SAVED hook (its stored name) — absent while creating. */
  url?: string;
  entities: EntityOption[];
  err: string;
  saving: boolean;
  onChange: (v: Draft) => void;
  onSave: () => void;
  onCancel: () => void;
}) {
  const { data: fields = [] } = useQuery({
    queryKey: ["fields", value.entityName],
    queryFn: () => listFields(value.entityName),
    enabled: !!value.entityName,
  });
  const uniqueFields = fields.filter((f) => f.isUnique);
  const entityOptions = entities.map((e) => ({ value: e.name, label: `${e.label} (${e.name})` }));

  const setMapping = (i: number, patch: Partial<HookMapping>) => {
    const mapping = value.mapping.map((m, j) => (j === i ? { ...m, ...patch } : m));
    onChange({ ...value, mapping });
  };

  return (
    <div className="flex flex-col gap-3">
      {url && (
        <div className="flex items-end gap-2">
          <div className="min-w-0 flex-1">
            <Input label={t("page.inboundHook.f.url")} readOnly value={url} className="slds-text-font_monospace"
              onFocus={(e) => e.currentTarget.select()} />
          </div>
          <ButtonIcon variant="border-filled" icon={Copy} label={t("page.inboundHook.copyUrl")} onClick={() => void copyUrl(url)} />
        </div>
      )}

      <Input label={t("page.inboundHook.f.name")} value={value.name} placeholder="bank-in" className="slds-text-font_monospace"
        onChange={(e) => onChange({ ...value, name: e.target.value })} />

      <Select label={t("page.inboundHook.f.entity")} placeholder={t("page.inboundHook.entityPh")}
        value={value.entityName} options={entityOptions}
        onChange={(e) => onChange({ ...value, entityName: e.target.value, matchField: undefined })} />

      <fieldset className="slds-form-element">
        <legend className="slds-form-element__legend slds-form-element__label">{t("page.inboundHook.f.mapping")}</legend>
        {/* __control clears the floated legend (fieldset form-element blueprint). */}
        <div className="slds-form-element__control flex flex-col gap-2">
          {value.mapping.map((m, i) => (
            <div key={i} className="flex items-center gap-2">
              <div className="min-w-0 flex-1">
                <Input labelHidden label={tf("page.inboundHook.f.source", { n: i + 1 })} value={m.source} placeholder="data.amount"
                  className="slds-text-font_monospace" onChange={(e) => setMapping(i, { source: e.target.value })} />
              </div>
              <span aria-hidden="true" className="slds-text-color_weak">→</span>
              <div className="w-40 shrink-0">
                <Select labelHidden label={tf("page.inboundHook.f.field", { n: i + 1 })} placeholder={t("page.inboundHook.fieldPh")}
                  value={m.field} options={fields.map((f) => ({ value: f.name, label: f.name }))}
                  onChange={(e) => setMapping(i, { field: e.target.value })} />
              </div>
              <ButtonIcon icon={X} variant="border-filled" label={tf("page.inboundHook.removeRow", { n: i + 1 })}
                onClick={() => onChange({ ...value, mapping: value.mapping.filter((_, j) => j !== i) })} />
            </div>
          ))}
          <div>
            <Button variant="base" iconLeft={Plus}
              onClick={() => onChange({ ...value, mapping: [...value.mapping, { source: "", field: "" }] })}>
              {t("page.inboundHook.addRow")}
            </Button>
          </div>
        </div>
      </fieldset>

      <Select label={t("page.inboundHook.f.match")} placeholder={t("page.inboundHook.matchPh")}
        value={value.matchField ?? ""} options={uniqueFields.map((f) => ({ value: f.name, label: f.name }))}
        help={t("page.inboundHook.matchHelp")}
        onChange={(e) => onChange({ ...value, matchField: e.target.value || undefined })} />

      <PaymentMatchSection
        entities={entities}
        value={value.paymentMatch}
        onChange={(pm) => onChange({ ...value, paymentMatch: pm })}
      />

      <Checkbox label={t("page.inboundHook.f.active")} checked={value.isActive !== false}
        onChange={(e) => onChange({ ...value, isActive: e.target.checked })} />

      {!value.id && <ScopedNotification icon={KeyRound}>{t("page.inboundHook.secretHint")}</ScopedNotification>}

      {/* Footer pinned to the bottom of the scrolling panel (same bar as the work-queue editor). The
          server's 422 sits in it, right above the buttons: in view wherever the form was scrolled to. */}
      <div className="slds-theme_default slds-border_top sticky bottom-0 flex flex-col gap-2 py-3">
        {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button onClick={onCancel}>{t("action.cancel")}</Button>
          <Button variant="brand" disabled={saving} onClick={onSave}>
            {saving ? t("action.saving") : t("page.inboundHook.save")}
          </Button>
        </div>
      </div>
    </div>
  );
}

// ── payment-match section (DONG-TIEN p2, ../specs/016-payment-flow/spec.md §8) ─────────────────────────────────

const EMPTY_PM: HookPaymentMatch = {
  targetEntity: "", refField: "", refPattern: "", memoSource: "",
  statusField: "", statusValue: "",
};

function PaymentMatchSection({ entities, value, onChange }: {
  entities: EntityOption[];
  value?: HookPaymentMatch;
  onChange: (pm: HookPaymentMatch | undefined) => void;
}) {
  const enabled = value !== undefined;
  const pm = value ?? EMPTY_PM;
  const { data: targetFields = [] } = useQuery({
    queryKey: ["fields", pm.targetEntity],
    queryFn: () => listFields(pm.targetEntity),
    enabled: !!pm.targetEntity,
  });
  const uniqueFields = targetFields.filter((f) => f.isUnique);
  const set = (patch: Partial<HookPaymentMatch>) => onChange({ ...pm, ...patch });
  const choose = t("page.inboundHook.pm.choose");
  const names = (fs: typeof targetFields) => fs.map((f) => ({ value: f.name, label: f.name }));

  return (
    <div className="slds-box slds-box_small">
      <Checkbox label={t("page.inboundHook.pm.enable")} checked={enabled}
        onChange={(e) => onChange(e.target.checked ? { ...EMPTY_PM } : undefined)} />
      {enabled && (
        <div className="slds-m-top_small flex flex-col gap-3">
          <div className="grid grid-cols-2 gap-3">
            <Select label={t("page.inboundHook.pm.target")} placeholder={choose} value={pm.targetEntity}
              options={entities.map((e) => ({ value: e.name, label: e.name }))}
              onChange={(e) => set({ targetEntity: e.target.value, refField: "", statusField: "", amountSource: undefined, amountField: undefined })} />
            <Select label={t("page.inboundHook.pm.refField")} placeholder={choose} value={pm.refField}
              options={names(uniqueFields)} onChange={(e) => set({ refField: e.target.value })} />
            <Input label={t("page.inboundHook.pm.refPattern")} value={pm.refPattern} placeholder="INV-\d+"
              className="slds-text-font_monospace" onChange={(e) => set({ refPattern: e.target.value })} />
            <Input label={t("page.inboundHook.pm.memoSource")} value={pm.memoSource} placeholder="data.description"
              className="slds-text-font_monospace" onChange={(e) => set({ memoSource: e.target.value })} />
            <Select label={t("page.inboundHook.pm.statusField")} placeholder={choose} value={pm.statusField}
              options={names(targetFields)} onChange={(e) => set({ statusField: e.target.value })} />
            <Input label={t("page.inboundHook.pm.statusValue")} value={pm.statusValue} placeholder={t("page.inboundHook.pm.statusValuePh")}
              onChange={(e) => set({ statusValue: e.target.value })} />
            <Input label={t("page.inboundHook.pm.amountSource")} value={pm.amountSource ?? ""} placeholder="data.amount"
              className="slds-text-font_monospace" onChange={(e) => set({ amountSource: e.target.value || undefined })} />
            <Select label={t("page.inboundHook.pm.amountField")} placeholder={t("page.inboundHook.pm.noAmount")} value={pm.amountField ?? ""}
              options={names(targetFields.filter((f) => f.fieldType === "number" || f.fieldType === "currency"))}
              onChange={(e) => set({ amountField: e.target.value || undefined })} />
          </div>
          <p className="slds-text-body_small slds-text-color_weak">{t("page.inboundHook.pm.help")}</p>
        </div>
      )}
    </div>
  );
}

// ── deliveries (docked panel body) ───────────────────────────────────────────────────────────

function Deliveries({ hook }: { hook: InboundHook }) {
  const { data: rows = [], isLoading } = useQuery({
    queryKey: ["hook-deliveries", hook.id],
    queryFn: () => listHookDeliveries(hook.id),
  });
  if (isLoading) {
    return (
      <div className="relative h-24">
        <Spinner size="small" label={t("action.loading")} />
      </div>
    );
  }
  if (rows.length === 0) return <p className="slds-text-color_weak">{t("page.inboundHook.deliveriesEmpty")}</p>;
  return (
    <ul className="slds-has-dividers_bottom-space" aria-label={t("page.inboundHook.deliveriesList")}>
      {rows.map((d) => (
        <li key={d.id} className="slds-item flex flex-col gap-2">
          <div className="flex items-center justify-between gap-2">
            <span className="flex items-center gap-1">
              <Badge theme={d.status === "created" ? "success" : d.status === "error" ? "error" : undefined}>{d.status}</Badge>
              {d.matchStatus && (
                <Badge theme={d.matchStatus === "matched" ? "success" : "warning"}>
                  {d.matchStatus === "matched" ? t("page.inboundHook.matched") : t("page.inboundHook.unmatched")}
                </Badge>
              )}
            </span>
            <span className="slds-text-body_small slds-text-color_weak tabular-nums">{new Date(d.createdAt).toLocaleString("vi-VN")}</span>
          </div>
          {d.error && <ScopedNotification theme="error">{d.error}</ScopedNotification>}
          {d.matchStatus === "matched" && d.matchRecordId && (
            <p className="slds-text-body_small slds-text-color_weak slds-text-font_monospace">
              {tf("page.inboundHook.matchRecord", { id: d.matchRecordId })}
            </p>
          )}
          {d.payloadHead && (
            <pre className="slds-box slds-box_xx-small slds-theme_shade slds-text-font_monospace slds-text-body_small max-h-24 overflow-auto">{d.payloadHead}</pre>
          )}
        </li>
      ))}
    </ul>
  );
}
