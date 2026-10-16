"use client";

// Field widgets — the WRITE-mode renderer for every field type, on the SLDS kit (components/slds:
// Input, Combobox, DatePicker/TimePicker, Checkbox, Textarea, FormElement — SLDS 2.264.1). One
// registry for every write surface: record form (CellRenderer), batch create, mass edit, workflow
// builder.
//
// FieldRow is the labelled frame. It no longer draws a label row of its own: it hands the error to
// the FieldInput inside it through context, and the SLDS control draws ONE form element with a real
// <label for>, required mark, field-level help and help/error line (blueprint
// form-element/record-detail "edit-stacked"). Outside a FieldRow — a batch-table cell — the label is
// kept for assistive tech only (`a11yLabel`, e.g. "Tên công ty – dòng 2").
//
// Values keep their widget-native shapes (lib/recordPayload): the model is what buildPayload sends.

import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { FileText, Info, Mail, Minus, Plus, Star, UserRound } from "lucide-react";
import type { FieldSpec } from "@/lib/metadata";
import { ensureAuth, getRecord, listFields, listRecords, listUserDirectory, ApiError, type ApiRecord, type FieldMeta, type DirectoryUser } from "@/lib/api";
import { dependentChoice, relationFilterChoice } from "@/lib/dependentPicklist";
import { toWireDateTime, VN_OFFSET } from "@/lib/recordPayload";
import { t, tf } from "@/lib/i18n";
import { useEntities } from "@/lib/metadata/hooks";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Checkbox } from "@/components/slds/Checkbox";
import { Combobox, ComboboxField, type ComboboxOption } from "@/components/slds/Combobox";
import { DateField, DatePicker } from "@/components/slds/DatePicker";
import { FormElement, useFieldIds, type FieldProps } from "@/components/slds/FormElement";
import { Input } from "@/components/slds/Input";
import { Textarea } from "@/components/slds/Textarea";
import { TimePicker } from "@/components/slds/TimePicker";
import { Tooltip } from "@/components/slds/Tooltip";

// ── Frame: FieldRow → FieldInput ────────────────────────────────────────────────────────────────
const RowContext = createContext<{ error?: string } | null>(null);

/** Labelled frame for one field. `field` is the same spec the FieldInput inside it renders. */
export function FieldRow({ error, children }: { field: FieldSpec; error?: string; children: ReactNode }) {
  return <RowContext.Provider value={{ error }}>{children}</RowContext.Provider>;
}

/** SLDS field-level help (blueprint form-element "help-text-icon"): info button + tooltip. */
export function FieldHelp({ text, label }: { text: string; label: string }) {
  return <Tooltip trigger={<ButtonIcon icon={Info} label={`${t("page.recordForm.help")}: ${label}`} />}>{text}</Tooltip>;
}

type Frame = FieldProps & { required?: boolean };

// The props every SLDS control takes to draw its form element.
function useFrame(field: FieldSpec, invalid?: boolean | string, a11yLabel?: string): Frame {
  const row = useContext(RowContext);
  if (!row) {
    // A bare cell: the column header is the visible label; the error is the red state, plus its line
    // under the control when the caller has the reason (a failed batch row).
    return { label: a11yLabel ?? field.label, labelHidden: true, required: field.required, error: invalid || undefined };
  }
  return {
    label: field.label,
    required: field.required,
    help: field.helpText,
    error: row.error || (invalid || undefined),
    // W5: the per-FIELD description is a hover "?" beside the label, not body text; the per-VIEW
    // helpText stays the line under the control (contract §2.3 keeps the two surfaces distinct).
    fieldLevelHelp: field.description ? <FieldHelp text={field.description} label={field.label} /> : undefined,
    layout: "stacked",
    editing: true,
  };
}

// Pickers built on DateField/fieldsets take no `layout`: give them the same stacked classes.
const stackedClass = (f: Frame) => (f.layout === "stacked" ? "slds-form-element_stacked slds-is-editing" : undefined);

// A compound fieldset draws the only error line; every control inside is invalid and described by
// it (blueprint datetime-picker error, like the kit DateTimePicker's errorId). Without it a screen
// reader hears nothing on the control the user has to fix.
function useCompoundError(frame: Frame) {
  const { helpId } = useFieldIds();
  return { helpId, errorId: frame.error ? helpId : undefined };
}

/** The rest of the record the widget sits in: a dependent picklist lists by its controlling field. */
export interface Siblings {
  /** Current values keyed by field name — form state, a batch row. */
  values: Record<string, unknown>;
  fieldsByName: Record<string, FieldMeta>;
}

// ── Dispatcher: render the widget for the field type ─────────────────────────────────────────────
export function FieldInput({
  field,
  value,
  onChange,
  error,
  a11yLabel,
  siblings,
}: {
  field: FieldSpec;
  value: unknown;
  onChange: (v: unknown) => void;
  /** Invalid state; a string is also the message (outside a FieldRow, where no frame carries it). */
  error?: boolean | string;
  /** Assistive label when rendered outside a FieldRow (default: the field label). */
  a11yLabel?: string;
  /** Sibling fields of the same record (specs/052 dependent picklist); omitted = no filtering. */
  siblings?: Siblings;
}) {
  const frame = useFrame(field, error, a11yLabel);
  const o = field.options ?? {};
  switch (field.fieldType) {
    case "email":
      return (
        <Input {...frame} type="email" iconLeft={Mail} value={str(value)} onChange={(e) => onChange(e.target.value)}
          placeholder={t("page.recordForm.emailPlaceholder")} />
      );
    case "boolean":
      return <Checkbox {...frame} variant="standalone" checked={!!value} onChange={(e) => onChange(e.target.checked)} />;
    case "picklist": {
      // specs/052: a dependent picklist lists only the children its controlling field's value
      // allows, and is locked while that field is empty (unless it already holds a value — a pair
      // from before the config — which must stay clearable).
      const dep = dependentChoice(o, siblings?.values);
      if (dep.kind === "flat") {
        return <Picklist frame={frame} values={o.values ?? []} labels={o.valueLabels} value={str(value)} onChange={onChange} />;
      }
      const parent = dep.kind === "blocked" ? (siblings?.fieldsByName[dep.controllingField]?.label ?? dep.controllingField) : "";
      return (
        <Picklist frame={frame} values={dep.kind === "filtered" ? dep.values : []} labels={o.valueLabels} value={str(value)} onChange={onChange}
          disabled={dep.kind === "blocked" && !str(value)}
          placeholder={dep.kind === "blocked" ? tf("page.recordForm.pickParentFirst", { field: parent })
            : dep.values.length ? undefined : t("page.recordForm.noDependentValues")} />
      );
    }
    case "multi_select":
      return <MultiSelect frame={frame} values={o.values ?? []} labels={o.valueLabels} value={arr(value)} onChange={onChange} />;
    case "rating":
      return <Rating frame={frame} max={o.max ?? 5} value={num(value)} onChange={onChange} />;
    case "currency":
      return <Currency frame={frame} code={o.currencyCode ?? "VNĐ"} value={str(value)} onChange={onChange} />;
    case "number":
      return <NumberCounter frame={frame} value={value} step={o.step ?? 1} min={o.min} max={o.max} onChange={onChange} />;
    case "date":
      return <DateInput frame={frame} value={str(value)} onChange={onChange} />;
    case "datetime":
      return <DateTimeInput frame={frame} value={str(value)} onChange={onChange} />;
    case "relation": {
      // queue #304: options.filterBy narrows the picker by a sibling relation on the same form.
      const rf = relationFilterChoice(o, siblings?.values);
      const parent = rf.kind === "blocked" ? (siblings?.fieldsByName[rf.parentField]?.label ?? rf.parentField) : "";
      return (
        <RecordPicker frame={frame} relatedEntity={o.relatedEntity ?? ""} labelFieldOpt={o.labelField}
          value={str(value)} onChange={onChange} filter={rf.kind === "filtered" ? { field: rf.field, value: rf.value } : undefined}
          blockedBy={rf.kind === "blocked" ? parent : undefined} />
      );
    }
    case "polymorphic_relation":
      return <PolymorphicPicker frame={frame} field={field} value={value} onChange={onChange} />;
    case "user":
      // Stores a bare userId — what the server validates and what ?expand= turns into a name.
      // userIdOf is the cheap guard for a caller that hands one over expanded: the shared str()
      // would silently coerce {id,label,email} to "[object Object]" and write that.
      return <UserPicker frame={frame} value={userIdOf(value)} onChange={onChange} />;
    case "rich_text":
      return (
        <Textarea {...frame} rows={3} value={str(value)} onChange={(e) => onChange(e.target.value)}
          placeholder={t("page.recordForm.richPlaceholder")} />
      );
    case "composite":
      return <Composite frame={frame} subFields={o.subFields ?? []} value={obj(value)} onChange={onChange} />;
    case "file":
      return <AttachmentNote frame={frame} kind="file" />;
    case "image":
      return <AttachmentNote frame={frame} kind="image" value={value} />;
    case "formula":
      // Formula fields are always read-only — show the computed value with an ƒ mark.
      return <ReadonlyComputed frame={frame} glyph="ƒ" value={str(value)} hint="—" />;
    case "lookup":
      // Server-mirrored from a to-one related record on read — never user-entered (else 422
      // RECORD_LOOKUP_READONLY).
      return <ReadonlyComputed frame={frame} glyph="↦" value={str(value)} hint={t("page.recordForm.hintLookup")} />;
    case "rollup":
      // Server-aggregated over child records on read — never user-entered (RECORD_ROLLUP_READONLY).
      return <ReadonlyComputed frame={frame} glyph="Σ" value={str(value)} hint={t("page.recordForm.hintRollup")} />;
    case "auto_number":
      // Server-ASSIGNED sequence at Create (RECORD_AUTONUMBER_READONLY on any submitted value).
      return <ReadonlyComputed frame={frame} glyph="#" value={str(value)} hint={t("page.recordForm.hintAutoNumber")} />;
    case "text":
    default:
      return (
        <Input {...frame} value={str(value)} onChange={(e) => onChange(e.target.value)}
          placeholder={field.helpText ? "" : t("page.recordForm.placeholder")} />
      );
  }
}

// ── Widgets ─────────────────────────────────────────────────────────────────────────────────────
// Server-computed / server-assigned types (formula/lookup/rollup/auto_number) are never submitted
// (buildPayload strips them): static text, the computed value when it exists (edit) or a weak hint
// of where it comes from (create). Form-element "static" state — no input to type over.
function ReadonlyComputed({ frame, glyph, value, hint }: { frame: Frame; glyph: string; value: string; hint: string }) {
  return (
    <FormElement {...frame} readOnly="static" title={t("page.recordForm.readonlyComputed")}>
      <span className="slds-text-color_weak slds-m-right_x-small" aria-hidden="true">{glyph}</span>
      {value ? <span className="tabular-nums">{value}</span> : <span className="slds-text-color_weak">{hint}</span>}
    </FormElement>
  );
}

// `labels` (options.valueLabels): what a HUMAN reads per code. onChange always emits the CODE —
// that invariant is the whole design; the stored value and every workflow/report comparing against
// it never see the Vietnamese text.
function choiceOptions(values: string[], labels: Record<string, string> | undefined, keep: string[]): ComboboxOption[] {
  // A stored code the picklist no longer lists (renamed/removed value) stays visible, never blanked.
  const all = [...values, ...keep.filter((v) => v && !values.includes(v))];
  return all.map((v) => ({ value: v, label: labels?.[v] ?? v }));
}

function Picklist({ frame, values, labels, value, onChange, disabled, placeholder }: {
  frame: Frame; values: string[]; labels?: Record<string, string>; value: string; onChange: (v: string) => void;
  disabled?: boolean; placeholder?: string;
}) {
  return (
    <Combobox {...frame} options={choiceOptions(values, labels, [value])} value={value || null}
      onChange={(v) => onChange(v ?? "")} disabled={disabled}
      placeholder={placeholder ?? (values.length ? t("page.recordForm.pick") : t("page.recordForm.noValues"))} />
  );
}

function MultiSelect({ frame, values, labels, value, onChange }: { frame: Frame; values: string[]; labels?: Record<string, string>; value: string[]; onChange: (v: string[]) => void }) {
  return (
    <Combobox {...frame} multiple options={choiceOptions(values, labels, value)} value={value} onChange={onChange}
      placeholder={values.length ? t("page.recordForm.addValue") : t("page.recordForm.noValues")} />
  );
}

// No SLDS rating blueprint: icon buttons (buttons/icon) in a labelled group, lit with the warning
// icon tone. aria-pressed tells which stars are on.
function Rating({ frame, max, value, onChange }: { frame: Frame; max: number; value: number; onChange: (v: number) => void }) {
  const { helpId } = useFieldIds();
  const message = frame.error ?? frame.help;
  // Buttons take no aria-invalid: each star reads the line (error or help), like the counter's input.
  const describedById = message != null && message !== "" && message !== true ? helpId : undefined;
  return (
    <FormElement {...frame} as="fieldset" helpId={helpId}>
      {Array.from({ length: max }, (_, i) => i + 1).map((n) => (
        <button key={n} type="button" className="slds-button slds-button_icon" aria-pressed={n <= value}
          aria-describedby={describedById}
          title={tf("page.recordForm.ratingStar", { n, max })} onClick={() => onChange(n)}>
          <Star aria-hidden="true"
            className={`slds-button__icon slds-button__icon_large nx-lucide ${n <= value ? "slds-icon-text-warning" : "slds-icon-text-light"}`} />
          <span className="slds-assistive-text">{tf("page.recordForm.ratingStar", { n, max })}</span>
        </button>
      ))}
    </FormElement>
  );
}

// Decimal string (widget-native currency shape); the code is the input's fixed-text addon.
function Currency({ frame, code, value, onChange }: { frame: Frame; code: string; value: string; onChange: (v: string) => void }) {
  return (
    <Input {...frame} inputMode="decimal" numeric fixedTextRight={code} value={value} placeholder="0"
      onChange={(e) => {
        let v = e.target.value.replace(/[^\d.]/g, "");
        const i = v.indexOf(".");
        if (i >= 0) v = v.slice(0, i + 1) + v.slice(i + 1).replace(/\./g, "");
        onChange(v);
      }} />
  );
}

// Blueprint `counter`: the input with − / + buttons inside it. An untouched field ("") stays empty
// on screen — it is omitted from the payload, so showing "0" would promise a value it won't save.
function NumberCounter({ frame, value, step, onChange, min, max }: { frame: Frame; value: unknown; step: number; onChange: (v: number | "") => void; min?: number; max?: number }) {
  const { controlId, labelId, helpId } = useFieldIds();
  const n = num(value);
  // #276: typing is free (even empty or a lone "-"); min/max are enforced on blur and by the form
  // schema on submit — clamping per keystroke turned "30" into "50" when min was 5.
  const [draft, setDraft] = useState<string | null>(null);
  const clamp = (x: number) => Math.min(max ?? Infinity, Math.max(min ?? -Infinity, x));
  const atMin = min !== undefined && n <= min;
  const message = frame.error ?? frame.help;
  return (
    <FormElement {...frame} htmlFor={controlId} labelId={labelId} helpId={helpId}>
      <button type="button" className="slds-button slds-button_icon slds-button_icon-small slds-input__button_decrement"
        title={t("page.recordForm.decrement")} disabled={atMin} onClick={() => onChange(clamp(n - step))}>
        <Minus aria-hidden="true" className="slds-button__icon nx-lucide" />
        <span className="slds-assistive-text">{t("page.recordForm.decrement")}</span>
      </button>
      <input id={controlId} className="slds-input slds-input_counter tabular-nums" inputMode="numeric"
        value={draft ?? (value === "" || value == null ? "" : String(n))} required={frame.required}
        aria-invalid={frame.error ? true : undefined}
        aria-describedby={message != null && message !== "" && message !== true ? helpId : undefined}
        onChange={(e) => {
          const txt = e.target.value.replace(/[^\d-]/g, "");
          setDraft(txt);
          if (txt === "") onChange("");
          else if (txt !== "-" && !Number.isNaN(Number(txt))) onChange(Number(txt));
        }}
        onBlur={() => {
          if (draft !== null && value !== "" && value != null) onChange(clamp(n));
          setDraft(null);
        }} />
      <button type="button" className="slds-button slds-button_icon slds-button_icon-small slds-input__button_increment"
        title={t("page.recordForm.increment")} onClick={() => onChange(clamp(n + step))}>
        <Plus aria-hidden="true" className="slds-button__icon nx-lucide" />
        <span className="slds-assistive-text">{t("page.recordForm.increment")}</span>
      </button>
    </FormElement>
  );
}

// The MODEL value of a date/datetime field is the WIRE format the backend accepts and echoes
// (record_service.go checkType): date `YYYY-MM-DD`; datetime naive-with-seconds
// `YYYY-MM-DDTHH:mm:ss` (RFC3339 offsets and the server's space-form echo `YYYY-MM-DD HH:mm:ss`
// are parsed too). An old DateInput wrote its DISPLAY string ("01/08/2026") into the model and
// buildPayload passed it through verbatim → every calendar-picked date 422'd RECORD_INVALID_DATE
// (dogfood 2026-07-15). parseWire keeps accepting the legacy DMY form so an in-progress draft
// typed under the old build still displays.
export function parseWire(v: string): { d: Date; time: string } | null {
  const iso = v.match(/^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::\d{2})?)?/);
  if (iso) {
    const d = new Date(Number(iso[1]), Number(iso[2]) - 1, Number(iso[3]));
    if (isNaN(d.getTime())) return null;
    return { d, time: iso[4] ? `${iso[4]}:${iso[5]}` : "" };
  }
  const dmy = v.match(/^(\d{1,2})\/(\d{1,2})\/(\d{4})(?:\s*·\s*(\d{2}:\d{2}))?/);
  if (!dmy) return null;
  const d = new Date(Number(dmy[3]), Number(dmy[2]) - 1, Number(dmy[1]));
  return isNaN(d.getTime()) ? null : { d, time: dmy[4] ?? "" };
}
function fmtWireDate(d: Date) {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
}

function DateInput({ frame, value, onChange }: { frame: Frame; value: string; onChange: (v: string) => void }) {
  const parsed = parseWire(value);
  return (
    <DatePicker {...frame} className={stackedClass(frame)} value={parsed ? fmtWireDate(parsed.d) : null}
      onChange={(iso) => onChange(iso ?? "")} />
  );
}

// Blueprint datetime-picker (compound fieldset: date field + time combobox). Not the kit
// DateTimePicker: that one reports null until BOTH halves are set and emits an offset ISO string;
// this field has always saved on the date alone (time defaults to 09:00) in the naive wire format.
//
// #278: the store is UTC but the picker shows — and takes — Vietnam wall-clock time (D6). A value
// that names its zone (Z / +07:00 / the server's space-form echo, which is UTC) is shifted to the
// Vietnam clock for display; the pick leaves with an explicit +07:00, so what was typed is what
// comes back. A zoneless legacy value is already a Vietnam wall clock.
export function vnWallClock(v: string): string {
  const w = toWireDateTime(v);
  if (!/(Z|[+-]\d{2}:\d{2})$/.test(w)) return w;
  const ms = Date.parse(w);
  return isNaN(ms) ? w : new Date(ms + 7 * 3600_000).toISOString().slice(0, 16);
}

function DateTimeInput({ frame, value, onChange }: { frame: Frame; value: string; onChange: (v: string) => void }) {
  const parsed = parseWire(vnWallClock(value));
  const date = parsed?.d;
  const time = parsed?.time || null;
  const emit = (d: Date | undefined, tm: string | null) => onChange(d ? `${fmtWireDate(d)}T${tm ?? "09:00"}:00${VN_OFFSET}` : "");
  const { helpId, errorId } = useCompoundError(frame);
  const inner = { labelHidden: frame.labelHidden, required: frame.required, requiredMark: false, errorId };
  return (
    <FormElement {...frame} as="fieldset" helpId={helpId}
      className={["slds-form-element_compound", stackedClass(frame)].filter(Boolean).join(" ")}>
      <div className="slds-form-element__group">
        <div className="slds-form-element__row">
          <DateField {...inner} label={t("slds.datetimepicker.date")} placeholder={t("slds.datepicker.placeholder")}
            value={date} onChange={(d) => emit(d ?? undefined, time)} />
          <TimePicker {...inner} label={t("slds.datetimepicker.time")} value={time}
            onChange={(tm) => emit(date ?? new Date(), tm)} />
        </div>
      </div>
    </FormElement>
  );
}

// Pick a human-readable label field for a related entity: prefer an explicit `labelField`
// option, then a field literally named "name", then the first text/email field, else fall
// back to the record id. Returns the field `name` to read from each record's `data` map.
function pickLabelField(fields: FieldMeta[], labelField?: string): string | null {
  // #479: server order — the table's EXPLICIT choice > the relation's labelField > the inferred default.
  const explicit = fields.find((f) => f.isLabel && f.isLabelExplicit);
  if (explicit) return explicit.name;
  if (labelField && fields.some((f) => f.name === labelField)) return labelField;
  const chosen = fields.find((f) => f.isLabel);
  if (chosen) return chosen.name;
  const byName = fields.find((f) => f.name === "name");
  if (byName) return byName.name;
  const firstText = fields.find((f) => f.fieldType === "text" || f.fieldType === "email");
  if (firstText) return firstText.name;
  return null;
}

// Derive a record's display label from the chosen label field, gracefully falling back to id.
function recordLabel(rec: ApiRecord, labelField: string | null): string {
  if (labelField) {
    const v = rec.data[labelField];
    if (v != null && String(v).trim() !== "") return String(v);
  }
  return rec.id;
}

const recordIcon = () => ({ glyph: FileText, name: "record", label: t("page.recordForm.record") });
const userIcon = () => ({ glyph: UserRound, name: "user", label: t("page.recordForm.user") });

// Record lookup (blueprint `lookups` = autocomplete combobox with object options): focusing the
// field loads the first page of the target entity (GET /:relatedEntity), typing searches it
// server-side (?q=). Stores record.id; shows the readable label. Not the kit Lookup: that one only
// searches once something is typed, and this field has always listed records on open.
// `display:contents` wrapper = no box of its own, it only hears focus from the combobox inside.
export function RecordPicker({ frame, relatedEntity, labelFieldOpt, value, onChange, className, filter, blockedBy }: {
  frame: Frame & { errorId?: string }; relatedEntity: string; labelFieldOpt?: string; value: string; onChange: (v: string) => void; className?: string;
  /** queue #304: list only target records where `field` = `value` (the sibling relation's id). */
  filter?: { field: string; value: string };
  /** The sibling relation's label while it is still empty: the picker waits for it. */
  blockedBy?: string;
}) {
  const [wanted, setWanted] = useState(false);
  const [q, setQ] = useState("");
  const [debouncedQ, setDebouncedQ] = useState("");
  const [records, setRecords] = useState<ApiRecord[]>([]);
  // undefined = discovery not finished yet; null = discovered "no label field exists".
  // The distinction matters: the id→label resolve effect must WAIT for discovery, or it
  // caches the raw id as the label and the guard blocks the re-resolve.
  const [labelField, setLabelField] = useState<string | null | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [errMsg, setErrMsg] = useState("");
  // Resolved id → label, so the chosen value reads as a name before/without a search.
  const [labels, setLabels] = useState<Record<string, string>>({});

  useEffect(() => {
    const timer = setTimeout(() => setDebouncedQ(q.trim()), 250);
    return () => clearTimeout(timer);
  }, [q]);

  // Discover the display label field once per related entity.
  useEffect(() => {
    if (!relatedEntity) return;
    let cancelled = false;
    (async () => {
      try {
        await ensureAuth();
        const fields = await listFields(relatedEntity);
        if (!cancelled) setLabelField(pickLabelField(fields, labelFieldOpt));
      } catch {
        // Non-fatal: fall back to showing the raw id if metadata can't be read.
        if (!cancelled) setLabelField(null);
      }
    })();
    return () => { cancelled = true; };
  }, [relatedEntity, labelFieldOpt]);

  const filterField = filter?.field ?? "";
  const filterValue = filter?.value ?? "";

  // Fetch matching records once the field has been focused (re-fetch on debounced query change).
  useEffect(() => {
    if (!wanted || !relatedEntity) return;
    let cancelled = false;
    (async () => {
      setLoading(true);
      setErrMsg("");
      try {
        await ensureAuth();
        const qs = [
          filterField ? `${encodeURIComponent(filterField)}=${encodeURIComponent(filterValue)}` : "",
          debouncedQ ? `q=${encodeURIComponent(debouncedQ)}` : "",
        ].filter(Boolean).join("&");
        const recs = await listRecords(relatedEntity, qs);
        if (cancelled) return;
        setRecords(recs);
        setLabels((prev) => {
          const next = { ...prev };
          for (const r of recs) next[r.id] = recordLabel(r, labelField ?? null);
          return next;
        });
      } catch (e) {
        if (!cancelled) setErrMsg(e instanceof ApiError ? e.message : t("page.recordForm.recordsFailed"));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [wanted, relatedEntity, debouncedQ, labelField, filterField, filterValue]);

  // Resolve the stored id → label when we have a value we haven't seen yet (e.g. on edit load).
  useEffect(() => {
    if (!value || !relatedEntity || labels[value] || labelField === undefined) return;
    let cancelled = false;
    (async () => {
      try {
        await ensureAuth();
        // GET /:entity/:id — `id` is not a filterable field name (LIST_UNKNOWN_FILTER on ?id=).
        const match = await getRecord(relatedEntity, value);
        if (!cancelled && match) setLabels((prev) => ({ ...prev, [value]: recordLabel(match, labelField) }));
      } catch {
        // Leave unresolved; the field shows the raw id as a graceful fallback.
      }
    })();
    return () => { cancelled = true; };
  }, [value, relatedEntity, labelField, labels]);

  const icon = recordIcon();
  const options = records.map((r) => ({ value: r.id, label: recordLabel(r, labelField ?? null), icon }));
  const selected = value ? [{ value, label: labels[value] ?? value, icon }] : [];
  return (
    <div className="contents" onFocus={() => setWanted(true)}>
      <ComboboxField {...frame} className={className} mode="autocomplete" options={relatedEntity ? options : []} selected={selected}
        onSelect={(o) => onChange(o.value)} onDeselect={() => onChange("")}
        inputValue={q} onInputChange={setQ} loading={loading}
        noResultsText={t("page.recordForm.noRecords")}
        disabled={!relatedEntity || (!!blockedBy && !value)}
        placeholder={blockedBy ? tf("page.recordForm.pickParentFirst", { field: blockedBy }) : t("page.recordForm.searchRecord")}
        help={!relatedEntity ? t("page.recordForm.noRelatedEntity") : errMsg || frame.help} />
    </div>
  );
}

// UserPicker — write-mode for the `user` field type. GET /users/directory returns the tenant's
// ACTIVE users (id/fullName/email) in one call — the same set the backend accepts
// (services.checkUserTargets rejects disabled/foreign users) — so filtering happens locally.
// Loaded once the field is focused, or at once when it already holds someone (to show the name).
export function UserPicker({ frame, value, onChange }: { frame: Frame; value: string; onChange: (v: string) => void }) {
  const [wanted, setWanted] = useState(false);
  const [q, setQ] = useState("");
  const [users, setUsers] = useState<DirectoryUser[]>([]);
  const [loading, setLoading] = useState(false);
  const [errMsg, setErrMsg] = useState("");

  useEffect(() => {
    if (!(wanted || value) || users.length > 0 || errMsg) return;
    let cancelled = false;
    (async () => {
      setLoading(true);
      try {
        await ensureAuth();
        const list = await listUserDirectory();
        if (!cancelled) setUsers(list);
      } catch (e) {
        // Honest failure: say the list could not load — never an empty list that reads as
        // "nobody works here".
        if (!cancelled) setErrMsg(e instanceof ApiError ? e.message : t("page.recordForm.usersFailed"));
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [wanted, value, users.length, errMsg]);

  const label = (u: DirectoryUser) => (u.fullName?.trim() ? u.fullName : u.email);
  const icon = userIcon();
  const needle = q.trim().toLowerCase();
  const shown = needle
    ? users.filter((u) => label(u).toLowerCase().includes(needle) || u.email.toLowerCase().includes(needle))
    : users;
  const chosen = users.find((u) => u.id === value);
  // Before the directory loads a stored id has no name yet — show the id rather than "not chosen"
  // (claiming the field is empty when it is not would be a lie).
  const selected = value ? [{ value, label: chosen ? label(chosen) : value, icon }] : [];
  return (
    <div className="contents" onFocus={() => setWanted(true)}>
      <ComboboxField {...frame} mode="autocomplete" selected={selected}
        options={shown.map((u) => ({ value: u.id, label: label(u), meta: u.email, icon }))}
        onSelect={(o) => onChange(o.value)} onDeselect={() => onChange("")}
        inputValue={q} onInputChange={setQ} loading={loading}
        placeholder={t("page.recordForm.pickUser")} noResultsText={t("page.recordForm.noUsers")}
        help={errMsg || frame.help} />
    </div>
  );
}

// Coerce a raw form value into a well-formed polymorphic ref (or null when unset/partial).
function asPolyRef(value: unknown): { type: string; id: string } | null {
  if (value && typeof value === "object") {
    const pv = value as { type?: unknown; id?: unknown };
    if (typeof pv.type === "string" && pv.type !== "" && typeof pv.id === "string" && pv.id !== "") {
      return { type: pv.type, id: pv.id };
    }
  }
  return null;
}

// PolymorphicPicker (ACTIVITY-CORE #3a): an ENTITY combobox (choices = options.targetEntities) THEN
// the shared RecordPicker for the chosen entity, as one compound fieldset. Stores the ATOMIC value
// { type, id } — never a bare { type } (that 422s). Switching the entity clears the id.
function PolymorphicPicker({ frame, field, value, onChange }: { frame: Frame; field: FieldSpec; value: unknown; onChange: (v: unknown) => void }) {
  const targets = (field.options?.targetEntities as string[] | undefined) ?? [];
  const ref = asPolyRef(value);
  // Transient chosen-entity: seed from the stored value, else the sole target when there's just one.
  const [entity, setEntity] = useState<string>(() => ref?.type ?? (targets.length === 1 ? targets[0] : ""));

  // Re-sync when a stored value arrives after mount (edit load resolves record.data async):
  // adjusted during render when the stored type changes, not in an effect (no extra render pass).
  const [seenType, setSeenType] = useState(ref?.type);
  if (ref?.type !== seenType) {
    setSeenType(ref?.type);
    if (ref?.type && ref.type !== entity) setEntity(ref.type);
  }

  const changeEntity = (e: string) => {
    setEntity(e);
    if (e !== ref?.type) onChange(null); // atomic: switching target clears the id (must re-pick)
  };
  const idForEntity = entity && entity === ref?.type ? ref.id : "";
  const { helpId, errorId } = useCompoundError(frame);
  const part = { labelHidden: frame.labelHidden, required: frame.required, errorId };
  // Target choices read as entity LABELS ("Khách hàng"), never machine names ("crm_accounts").
  const { data: entities = [] } = useEntities();
  const entityLabel = (e: string) => entities.find((x) => x.name === e)?.label ?? e;
  return (
    <FormElement {...frame} as="fieldset" helpId={helpId}
      className={["slds-form-element_compound", stackedClass(frame)].filter(Boolean).join(" ")}
      help={targets.length === 0 ? t("page.recordForm.polyNoTargets") : frame.help}>
      {targets.length > 0 && (
        <div className="slds-form-element__group">
          <div className="slds-form-element__row">
            <Combobox {...part} className="slds-size_1-of-2" label={t("page.recordForm.polyEntity")} options={targets.map((e) => ({ value: e, label: entityLabel(e) }))}
              value={entity || null} onChange={(e) => changeEntity(e ?? "")} />
            {entity && (
              <RecordPicker className="slds-size_1-of-2" frame={{ ...part, label: t("page.recordForm.record") }} relatedEntity={entity} value={idForEntity}
                onChange={(id) => onChange(id ? { type: entity, id } : null)} />
            )}
          </div>
        </div>
      )}
    </FormElement>
  );
}

// PolymorphicDisplay: read view of a set { type, id } value. The backend stores the value ATOMICALLY
// (no label key allowed), so — unlike relation's server ?expand= — the label is resolved client-side
// via one getRecord(type, id), falling back to the raw id. Links to /<type>/<id>.
export function PolymorphicDisplay({ value }: { value: { type: string; id: string } }) {
  const { type, id } = value;
  const [label, setLabel] = useState<string>("");
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        await ensureAuth();
        const [fields, rec] = await Promise.all([listFields(type), getRecord(type, id)]);
        if (!cancelled && rec) setLabel(recordLabel(rec, pickLabelField(fields)));
      } catch {
        // Leave unresolved; the raw id shows as a graceful fallback.
      }
    })();
    return () => { cancelled = true; };
  }, [type, id]);
  return (
    <a href={`/${type}/${id}`}>
      <span className="slds-truncate">{label || id}</span>
      <span className="slds-text-color_weak slds-m-left_xx-small">· {type}</span>
    </a>
  );
}

// Composite (e.g. address): the sub-fields as one compound fieldset, each a labelled field.
function Composite({ frame, subFields, value, onChange }: { frame: Frame; subFields: FieldSpec[]; value: Record<string, unknown>; onChange: (v: Record<string, unknown>) => void }) {
  return (
    <FormElement {...frame} as="fieldset" className={["slds-form-element_compound", stackedClass(frame)].filter(Boolean).join(" ")}>
      <div className="grid grid-cols-1 gap-x-4 md:grid-cols-2">
        {subFields.map((sf) => (
          <FieldRow key={sf.name} field={sf}>
            <FieldInput field={sf} value={value[sf.name]} onChange={(v) => onChange({ ...value, [sf.name]: v })} />
          </FieldRow>
        ))}
      </div>
    </FormElement>
  );
}

// File/image FORM widget — honest pointer. Attachments have a WORKING home: the "Tệp" tab on the
// record detail (file API, mig 000023). Until an inline-upload widget ships, say the truth.
function AttachmentNote({ frame, kind, value }: { frame: Frame; kind: "file" | "image"; value?: unknown }) {
  const src = kind === "image" && typeof value === "string" && /^https?:\/\//.test(value) ? value : undefined;
  return (
    <FormElement {...frame} readOnly="static">
      {src && (
        // eslint-disable-next-line @next/next/no-img-element
        <img src={src} alt="" className="slds-m-bottom_x-small h-20 w-20 object-cover" />
      )}
      <span className="slds-text-color_weak">
        {kind === "image" ? t("page.recordForm.attachImage") : t("page.recordForm.attachFile")}
      </span>
    </FormElement>
  );
}

// ── coercion helpers ─────────────────────────────────────────────────────────────────────────────
const str = (v: unknown) => (typeof v === "string" ? v : v == null ? "" : String(v));
const num = (v: unknown) => (typeof v === "number" ? v : Number(v) || 0);
const arr = (v: unknown) => (Array.isArray(v) ? (v as string[]) : []);
const obj = (v: unknown) => (v && typeof v === "object" ? (v as Record<string, unknown>) : {});
// A `user` value is a bare id on write, but an EXPANDED {id,label,email} on a read that asked for
// it. Unwrap so the picker never round-trips an object into a scalar column.
const userIdOf = (v: unknown) =>
  v && typeof v === "object" ? str((v as { id?: unknown }).id) : str(v);
