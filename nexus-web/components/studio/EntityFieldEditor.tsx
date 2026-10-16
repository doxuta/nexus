"use client";

// EntityFieldEditor — the Entity Hub "Trường" tab (NEXUS-MOT-XUONG-2026-07-29 §2b "Object Manager
// hub với tab con cố định"). SLDS 2.264.1, the way Salesforce's Object Manager shows "Fields &
// Relationships":
//   • the field list is a related-list card (`cards`) holding a `data-tables/advanced` grid — label
//     (opens the inspector) · code · type · properties (badges) · row menu (edit · move up/down ·
//     delete). Drag handles keep the persisted field_order reorder (@dnd-kit); the menu's
//     "Chuyển lên/xuống" is the keyboard path the drag never had.
//   • the inspector is a right-docked `panels/base` property panel of `form-element`s, `combobox`,
//     `checkbox-toggle`; "Bắt buộc" lives HERE only — the one-click asterisk on each row saved at
//     once with no confirm and no undo, and the 2026-09-29 acceptance run hit it by mistake (#92).
//   • "Thêm trường" is a `modals/base` dialog; the type is chosen from `visual-picker/vertical`
//     rows grouped as before (Cơ bản · Quan hệ · Tính toán · Nâng cao), with type-to-filter.
//   • deleting a field is a `prompt` that counts the table's real records first and asks for the
//     field code when there are any.
// Queue #92: saving the inspector sends ONLY the properties changed since the field was opened
// (lib/fieldInspector changedUpdate) — a save no longer writes this screen's stale copy of the
// other properties back over a change made elsewhere.
// The table's own rename/delete moved up to the hub's record-home header (EntityHub).

import { useEntityOptions } from "@/lib/entityOptions";
import { useEffect, useMemo, useReducer, useRef, useState, type ReactNode } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import {
  Plus, Lock, Trash2, KeyRound, Search as SearchIcon, Info, GripVertical, Sigma, Type, Hash,
  ToggleLeft, CalendarDays, Clock, Mail, Banknote, List, ListChecks, GitFork, Star, Combine,
  File as FileIcon, Image as ImageIcon, SearchCode, Calculator, ArrowDown01, AlignLeft, Waypoints,
  UserRound, ListPlus, X, Check,
} from "lucide-react";
import {
  DndContext, DragOverlay, closestCenter, PointerSensor, useSensor, useSensors,
  type DragEndEvent, type DragStartEvent,
} from "@dnd-kit/core";
import { SortableContext, verticalListSortingStrategy, useSortable } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import {
  getLayout, listFields, addField, updateField, deleteField, listAllRelationships, reorderFields,
  validateFormula, listRecordsPage, recomputeStoredRollup, ApiError, getPublishState,
  type FieldMeta,
} from "@/lib/api";
import { useAuth } from "@/lib/auth/AuthProvider";
import { useProjectQuery } from "./useProjectQuery";
import { currentProjectSlug } from "@/lib/studioRoutes";
import { qk } from "@/lib/query/keys";
import { useEntities } from "@/lib/metadata/hooks";
import { FIELD_TYPE_META, type FieldType } from "@/lib/metadata";
import { READONLY_FIELD_TYPES } from "@/lib/recordPayload";
import { getHelp } from "@/lib/help-content";
import { t, tf } from "@/lib/i18n";
import { undoDraftDelete } from "@/lib/draftUndo";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { toast } from "@/components/ui/Toaster";
import { useWorkshopScope } from "@/lib/workshopScope";
import { studioHref } from "@/lib/studioRoutes";
import { projectSlugAt } from "@/lib/studioContext";
import type { I18nKey } from "@/lib/i18n/vi";
import {
  rowsToValues, rowsToValueLabels, relabelRow, addRow, removeRow, renameRow, moveRow, duplicateIds,
  type PicklistRow,
} from "@/lib/picklistRows";
import { applyMove, reorderPayload, orderChanged } from "@/lib/fieldReorder";
import {
  buildOptions,
  emptyLookup, emptyRollup, emptyAutoNumber, emptyRelation,
  defaultInputKind, supportsDefaultValue, supportsIndexed, supportsSensitive,
  encodeDefaultValue, withSensitive, isRollupAggregatable,
  type LookupCfg, type RollupCfg, type AutoNumberCfg, type RelationCfg,
} from "@/lib/fieldOptions";
import { changedUpdate, draftFromField, type InspectorDraft } from "@/lib/fieldInspector";
import { PicklistDependency, dependencyUnmapped } from "@/components/studio/PicklistDependency";
import { RelationFilter, relationFilterIncomplete } from "@/components/studio/RelationFilter";
import {
  formulaReducer, initialFormulaState, shapeValidateRequest, FORMULA_DEBOUNCE_MS,
} from "@/lib/formulaLive";
import { DESCRIPTION_MAX, onForm } from "@/lib/layoutDraft";
import { FieldInput, FieldRow } from "@/components/fields/widgets";
import { Card } from "@/components/slds/Card";
import { DataTable, fill } from "@/components/slds/DataTable";
import { Panel } from "@/components/slds/Panel";
import { InlineConfirm } from "./InlineConfirm";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Badge } from "@/components/slds/Badge";
import { Input } from "@/components/slds/Input";
import { Textarea } from "@/components/slds/Textarea";
import { Select } from "@/components/slds/Select";
import { Toggle } from "@/components/slds/Toggle";
import { CheckboxGroup } from "@/components/slds/Checkbox";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { Combobox, type ComboboxOption } from "@/components/slds/Combobox";
import { FormElement } from "@/components/slds/FormElement";
import { ExpandableSection } from "@/components/slds/ExpandableSection";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Tooltip } from "@/components/slds/Tooltip";
import { Icon } from "@/components/slds/Icon";
import { MenuDivider, MenuItem } from "@/components/slds/Menu";
import { VisualPicker } from "@/components/slds/VisualPicker";

// ── type-picker palette: 4 groups + icon per type (explanations live in the i18n dictionary) ──
const TYPE_ICON: Record<FieldType, typeof Type> = {
  text: Type, rich_text: AlignLeft, number: Hash, boolean: ToggleLeft, date: CalendarDays,
  datetime: Clock, email: Mail, currency: Banknote, picklist: List, multi_select: ListChecks,
  relation: GitFork, polymorphic_relation: Waypoints, user: UserRound, rating: Star, composite: Combine,
  file: FileIcon, image: ImageIcon,
  formula: Sigma, lookup: SearchCode, rollup: Calculator, auto_number: ArrowDown01,
};
const typeExplain = (ft: FieldType) => t(`page.fieldEditor.explain.${ft}` as I18nKey);
const TYPE_GROUPS = [
  { title: "page.fieldEditor.group.basic", types: ["text", "rich_text", "number", "boolean", "date", "datetime", "email", "currency", "picklist", "multi_select", "rating"] },
  { title: "page.fieldEditor.group.relation", types: ["relation", "polymorphic_relation", "user"] },
  { title: "page.fieldEditor.group.computed", types: ["formula", "lookup", "rollup"] },
  { title: "page.fieldEditor.group.advanced", types: ["composite", "auto_number", "file", "image"] },
] as const satisfies ReadonlyArray<{ title: I18nKey; types: readonly FieldType[] }>;

// A field type absent from every group is INVISIBLE in Studio — it cannot be created at all, no
// matter how complete the backend is. That is how `user` stayed unbuildable while the engine
// validated it, expanded it and stored it (2026-07-29). TYPE_ICON and the explain keys are keyed by
// FieldType; this list was a plain array and guarded nothing. The assignment below fails to compile
// while any type is ungrouped, naming the offender.
// The [T] extends [never] tuple wrapping is load-bearing: a bare `T extends never` distributes
// over the empty union and collapses to never, and `const x: Ungrouped[] = []` would be vacuous
// (an empty array satisfies every array type). Written this way the value `true` stops being
// assignable the moment a type is ungrouped, and the error text names it.
type UngroupedFieldType = Exclude<FieldType, (typeof TYPE_GROUPS)[number]["types"][number]>;
const _everyFieldTypeIsGrouped: [UngroupedFieldType] extends [never] ? true : UngroupedFieldType = true;
void _everyFieldTypeIsGrouped;

// Types backend từ chối `isUnique: true` tại config-time — ẩn toggle "Duy nhất" thay vì để user
// dính 422 sau khi submit. Mirror (nexus-platform/internal/services/entity_service.go +
// field_service.go): validateFileFieldFlags (file/image → FIELD_FILE_NOT_UNIQUE),
// validateComputedFieldFlags (lookup/rollup → FIELD_COMPUTED_NOT_UNIQUE),
// validateRelationFieldFlags (relation → FIELD_RELATION_UNIQUE_UNSUPPORTED).
// auto_number is NOT here (queue #242): the BE allows it since RM-28 (a unique invoice number).
const UNIQUE_UNSUPPORTED_TYPES = new Set<FieldType>(["relation", "polymorphic_relation", "lookup", "rollup", "file", "image"]);

// Nước 3 quick add-field: types whose config block is REQUIRED to create a valid field.
const REQUIRED_CONFIG_TYPES = new Set<FieldType>([
  "picklist", "multi_select", "relation", "polymorphic_relation", "composite",
  "formula", "lookup", "rollup", "auto_number", "currency",
]);

// A dropdown sourced from an async fetch (target/child entity fields, N-N relationships) must
// still show the CURRENTLY STORED value while loading/before the fetch resolves.
function withStoredFallback<T extends { name: string; label?: string }>(list: T[], stored: string): T[] {
  if (!stored || list.some((f) => f.name === stored)) return list;
  return [{ name: stored, label: fill(t("page.fieldEditor.storedSuffix"), { value: stored }) } as T, ...list];
}

// Slugify: lowercase, Vietnamese diacritics stripped, non-alnum → underscore
function slugify(str: string): string {
  return str.toLowerCase().replace(/đ/g, "d").normalize("NFD")
    .replace(/[̀-ͯ]/g, "").replace(/[^a-z0-9]+/g, "_").replace(/^_+|_+$/g, "");
}

const typeLabel = (ft: string) => FIELD_TYPE_META[ft as FieldType]?.label ?? ft;
const toOption = (f: { name: string; label?: string }): ComboboxOption => ({ value: f.name, label: f.label || f.name, meta: f.name });

export function EntityFieldEditor({
  entityName, initialField, onSelectField, addSignal,
}: {
  entityName: string;
  /** Field to open in the inspector once the list has loaded (EntityPage `?truong=`). Unknown code = nothing opens. */
  initialField?: string | null;
  /** Called with the open field's code, or null when the inspector closes (EntityPage keeps `?truong=` in step). */
  onSelectField?: (name: string | null) => void;
  /** A counter the host bumps to open the add-field form from outside (the table page header button). */
  addSignal?: number;
}) {
  const workshop = useWorkshopScope();
  const qc = useQueryClient();
  // Tenant-wide entity list — relation/rollup target pickers + this table's own pretty label.
  const entitiesQ = useEntities();
  const entities = useMemo(() => entitiesQ.data ?? [], [entitiesQ.data]);
  const ent = entities.find((e) => e.name === entityName) ?? null;
  // Two tables called "Khách hàng" (CRM + Nha khoa) get code + app on a second line (queue #301);
  // the tables of the app this table sits in come first.
  const entityOptions = useEntityOptions(entities, { preferAppOf: entityName });

  // Fields of this table — the SAME cache entry every other metadata reader uses (layout builder,
  // record quick view…), so an edit here refreshes them too.
  const fieldsKey = qk.fields(entityName);
  const fieldsQ = useQuery({ queryKey: fieldsKey, queryFn: () => listFields(entityName) });
  const fields = useMemo(() => fieldsQ.data ?? [], [fieldsQ.data]);
  // Which fields the edit form shows — same cache entry the screen editor uses.
  const layoutQ = useQuery({ queryKey: qk.layout(entityName), queryFn: () => getLayout(entityName) });
  const onFormSet = useMemo(() => onForm(layoutQ.data, fields), [layoutQ.data, fields]);
  async function refreshFields(): Promise<FieldMeta[]> {
    const refreshed = await listFields(entityName);
    qc.setQueryData(fieldsKey, refreshed);
    return refreshed;
  }

  // ── Inspector: `base` is the field as it was when opened (the #92 baseline), `draft` the inputs ──
  const [insp, setInsp] = useState<{ base: FieldMeta; draft: InspectorDraft } | null>(null);
  const [fEditFormulaOk, setFEditFormulaOk] = useState<boolean | null>(null);
  const [fEditSaving, setFEditSaving] = useState(false);
  const [fEditOk, setFEditOk] = useState(false);
  const [fEditErr, setFEditErr] = useState<string | null>(null);
  // specs/063 S7-8: on a delivered workspace the "indexed" switch is the one inspector setting that reaches
  // the customer at once (the rest waits for Xuất bản) — say so. Same query key as the other draft screens.
  const { user } = useAuth();
  const slug = currentProjectSlug(usePathname() ?? "") ?? "";
  const { data: publishState } = useProjectQuery(slug, ["publishState", "draftView"], () =>
    user?.isAdmin ? getPublishState().catch(() => null) : Promise.resolve(null));
  const delivered = Boolean(publishState?.delivered);
  const labelRef = useRef<HTMLInputElement>(null);

  // URL → inspector: open the field the URL names once, when it can be found. `applied` stops a close from reopening it
  // while the URL is still catching up.
  const wanted = initialField ?? null;
  const [applied, setApplied] = useState<string | null>(null);
  if (applied !== wanted) {
    // Adjusting state while rendering (the documented alternative to an effect): runs again at once with `applied` settled.
    const f = wanted ? fields.find((x) => x.name === wanted) : undefined;
    if (!wanted) {
      setApplied(null);
      // The URL dropped ?truong= from outside: close the panel it had opened.
      if (insp?.base.name === applied) setInsp(null);
    } else if (f) {
      setApplied(wanted);
      if (insp?.base.name !== wanted) setInsp({ base: f, draft: draftFromField(f) });
    }
  }
  // Inspector → URL: report a CHANGE of the open field (not the first render).
  const openName = insp?.base.name ?? null;
  const reported = useRef(openName);
  useEffect(() => {
    if (reported.current === openName) return;
    reported.current = openName;
    onSelectField?.(openName);
  }, [openName, onSelectField]);

  function openInspector(f: FieldMeta) {
    setInsp({ base: f, draft: draftFromField(f) });
    setFEditOk(false);
    setFEditErr(null);
    setFEditFormulaOk(null);
  }
  function patchDraft(p: Partial<InspectorDraft>) {
    setInsp((s) => (s ? { ...s, draft: { ...s.draft, ...p } } : s));
    setFEditOk(false);
  }
  // Menu actions return focus to their trigger when the menu closes; move it to the label after that.
  function focusLabelSoon() {
    setTimeout(() => labelRef.current?.focus(), 60);
  }

  // ── Add field (in-place strip) ──
  const [showAddField, setShowAddField] = useState(false);
  // Focus the first field when the in-place strip opens (specs/074 F2 lát 6).
  const addStripRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!showAddField) return;
    const el = addStripRef.current;
    el?.scrollIntoView?.({ block: "nearest" });
    el?.querySelector<HTMLInputElement>("input")?.focus();
  }, [showAddField]);
  // Host asked to add a field: open the form once per bump (state adjusted while rendering, no effect).
  const [seenAdd, setSeenAdd] = useState(addSignal ?? 0);
  if (addSignal !== undefined && addSignal !== seenAdd) {
    setSeenAdd(addSignal);
    setShowAddField(true);
  }

  const [afLabel, setAfLabel] = useState("");
  const [afName, setAfName] = useState("");
  const [afNameTouched, setAfNameTouched] = useState(false);
  const [afType, setAfType] = useState<FieldType>("text");
  const [afRequired, setAfRequired] = useState(false);
  // Nước 3 QUICK ADD-FIELD: default = label + type + Tạo; everything optional sits behind this
  // disclosure. Types whose config is REQUIRED always render their block regardless.
  const [afAdvanced, setAfAdvanced] = useState(false);
  const [afUnique, setAfUnique] = useState(false);
  const [afCurrencyCode, setAfCurrencyCode] = useState("VND");
  const [afValues, setAfValues] = useState<PicklistRow[]>([]);
  const [afDesc, setAfDesc] = useState("");
  const [afFormulaOk, setAfFormulaOk] = useState<boolean | null>(null);
  const [afRelated, setAfRelated] = useState("");
  const [afPolyTargets, setAfPolyTargets] = useState<string[]>([]);
  const [afNumMin, setAfNumMin] = useState("");
  const [afNumMax, setAfNumMax] = useState("");
  const [afNumStep, setAfNumStep] = useState("");
  const [afTextMaxLen, setAfTextMaxLen] = useState("");
  const [afRatingMax, setAfRatingMax] = useState("");
  const [afFormula, setAfFormula] = useState("");
  // Backend accepts only number|currency (validateFormulaConfig); "text" here made Save 422.
  const [afFormulaResultType, setAfFormulaResultType] = useState("number");
  const [afSubFields, setAfSubFields] = useState("");
  const [afLookup, setAfLookup] = useState<LookupCfg>(emptyLookup());
  const [afRollup, setAfRollup] = useState<RollupCfg>(emptyRollup());
  const [afAutoNumber, setAfAutoNumber] = useState<AutoNumberCfg>(emptyAutoNumber());
  const [afRelation, setAfRelation] = useState<RelationCfg>(emptyRelation());
  const [afDefault, setAfDefault] = useState("");
  const [afDefaultMulti, setAfDefaultMulti] = useState<string[]>([]);
  const [afIndexed, setAfIndexed] = useState(false);
  const [afSensitive, setAfSensitive] = useState(false);
  const [afLoading, setAfLoading] = useState(false);
  const [afErr, setAfErr] = useState<string | null>(null);

  // ── Delete field (prompt with the table's real record count) ──
  const [delTarget, setDelTarget] = useState<FieldMeta | null>(null);
  const [delCount, setDelCount] = useState<number | null>(null);
  const [delTyped, setDelTyped] = useState("");
  const [delLoading, setDelLoading] = useState(false);

  // ── quick-find + reorder + list-level messages ──
  const [fldSearch, setFldSearch] = useState("");
  const [listErr, setListErr] = useState<string | null>(null);
  const [delFailed, setDelFailed] = useState<FieldMeta | null>(null);
  const [dragging, setDragging] = useState<string | null>(null);
  // Nước 1 chain strip: after "Thêm trường" succeeds, offer the NEXT stops (field → form layout →
  // list column).
  const [fieldChain, setFieldChain] = useState<{ label: string; entity: string } | null>(null);
  const pathname = usePathname();

  const q = fldSearch.trim().toLowerCase();
  const visible = q
    ? fields.filter((f) => f.name.toLowerCase().includes(q) || (f.label ?? "").toLowerCase().includes(q))
    : fields;
  const dragEnabled = !q; // reorder on a FILTERED list would send a wrong order — off while filtering

  // ── derived: inspector save gates ──
  const field = insp?.base ?? null;
  const draft = insp?.draft ?? null;
  const pending = useMemo(() => (insp ? changedUpdate(insp.base, insp.draft) : {}), [insp]);
  const dirty = Object.keys(pending).length > 0;
  const picklistDupes = useMemo(
    () => (draft && (field?.fieldType === "picklist" || field?.fieldType === "multi_select") ? duplicateIds(draft.rows) : new Set<string>()),
    [field?.fieldType, draft],
  );
  // specs/052 #37: a controlling field with nothing mapped is a 422 (empty dependentValues).
  const depUnmapped = !!draft && field?.fieldType === "picklist" && dependencyUnmapped(draft);
  // queue #304: a relation filter needs both halves (the sibling and the target field).
  const relFilterHalf = !!draft && field?.fieldType === "relation" && relationFilterIncomplete(draft);
  const saveBlocked = picklistDupes.size > 0 || (field?.fieldType === "formula" && fEditFormulaOk === false) || depUnmapped || relFilterHalf;
  const saveBlockedReason = picklistDupes.size > 0 ? t("page.fieldEditor.blockedDupes")
    : depUnmapped ? tf("page.fieldEditor.dep.blocked", { parent: fields.find((f) => f.name === draft?.controllingField)?.label || draft?.controllingField || "" })
      : relFilterHalf ? t("page.fieldEditor.relFilter.blocked")
        : t("page.fieldEditor.blockedFormula");

  // ── Handlers ──
  function resetAddField() {
    setShowAddField(false);
    setAfAdvanced(false);
    setAfLabel(""); setAfName(""); setAfNameTouched(false);
    setAfType("text"); setAfRequired(false); setAfUnique(false);
    setAfCurrencyCode("VND"); setAfValues([]); setAfRelated(""); setAfPolyTargets([]);
    setAfDesc(""); setAfFormulaOk(null);
    setAfNumMin(""); setAfNumMax(""); setAfNumStep("");
    setAfTextMaxLen(""); setAfRatingMax("");
    setAfFormula(""); setAfFormulaResultType("number"); setAfSubFields("");
    setAfLookup(emptyLookup()); setAfRollup(emptyRollup());
    setAfAutoNumber(emptyAutoNumber()); setAfRelation(emptyRelation());
    setAfDefault(""); setAfDefaultMulti([]); setAfIndexed(false); setAfSensitive(false);
    setAfErr(null);
  }

  function pickAddType(picked: FieldType) {
    setAfType(picked);
    if (UNIQUE_UNSUPPORTED_TYPES.has(picked)) setAfUnique(false);
    setAfDefault(""); setAfDefaultMulti([]);
    if (!supportsIndexed(picked, false)) setAfIndexed(false);
  }

  const addBlockedReason =
    afType === "formula" && afFormulaOk === false ? t("page.fieldEditor.blockedFormula")
      : afType === "polymorphic_relation" && afPolyTargets.length === 0 ? t("page.fieldEditor.polyPickOne")
        : undefined;
  const addDisabled = afLoading || !afName || !afLabel || !!addBlockedReason
    || ((afType === "picklist" || afType === "multi_select") && duplicateIds(afValues).size > 0);

  async function handleAddField() {
    if (addDisabled) return;
    setAfLoading(true); setAfErr(null);
    try {
      const options = buildOptions(afType, afCurrencyCode, afValues, afRelated, afNumMin, afNumMax, afNumStep, afTextMaxLen, afRatingMax, afFormula, afFormulaResultType, afSubFields,
        { lookup: afLookup, rollup: afRollup, autoNumber: afAutoNumber, relation: afRelation }, afPolyTargets,
        supportsIndexed(afType, afUnique) && afIndexed);
      // The PII flag rides on top of the per-type shape, same seam as `indexed` — set at CREATE
      // time too, or a field can only become sensitive on a second, separate save.
      const optionsWithFlags = withSensitive(options, supportsSensitive(afType), afSensitive);
      const created = await addField(entityName, {
        name: afName, label: afLabel, fieldType: afType, isRequired: afRequired, isUnique: afUnique, options: optionsWithFlags,
        description: afDesc.trim() || undefined, // fieldOrder omitted → backend appends cuối danh sách
      });
      // POST /admin/fields carries no defaultValue — chain the PUT the BE DOES accept. A failure
      // here must NOT look like a failed create (the field exists): surface it in the banner.
      let defaultErr: string | null = null;
      const dv = supportsDefaultValue(afType) ? encodeDefaultValue(afType, afDefault, afDefaultMulti) : "";
      if (dv && created.id) {
        try {
          await updateField(created.id, { defaultValue: dv });
        } catch (e) {
          defaultErr = e instanceof Error ? e.message : t("page.fieldEditor.unknownError");
        }
      }
      emitDraftsChanged("field-create"); // a new field is a pending change on a delivered workspace: the header recounts
      const refreshed = await refreshFields();
      const fresh = refreshed.find((f) => f.name === afName);
      if (fresh) openInspector(fresh);
      const createdLabel = afLabel;
      resetAddField();
      setFieldChain({ label: createdLabel, entity: entityName });
      if (defaultErr) setListErr(fill(t("page.fieldEditor.defaultNotSaved"), { label: createdLabel, error: defaultErr }));
    } catch (e) {
      setAfErr(e instanceof Error ? e.message : t("page.fieldEditor.addFailed"));
    } finally {
      setAfLoading(false);
    }
  }

  async function handleSaveField() {
    if (!insp?.base.id || !dirty) return;
    const { base } = insp;
    setFEditSaving(true); setFEditOk(false); setFEditErr(null);
    try {
      // #92: only what changed since the field was opened — never this screen's copy of the rest.
      await updateField(base.id!, pending);
      emitDraftsChanged("field-save");
      const refreshed = await refreshFields();
      const fresh = refreshed.find((f) => f.name === base.name);
      if (fresh) setInsp({ base: fresh, draft: draftFromField(fresh) });
      setFEditOk(true);
    } catch (e) {
      // FIELD_TYPE_IMMUTABLE (and any refusal that names a way out) carries details.hint: show it with the message.
      const hint = e instanceof ApiError && typeof e.details.hint === "string" ? e.details.hint : "";
      setFEditErr(e instanceof Error ? (hint ? `${e.message} — ${hint}` : e.message) : t("page.fieldEditor.saveFailed"));
    } finally {
      setFEditSaving(false);
    }
  }

  // Count the table's REAL records so the prompt says what the delete touches.
  function openDeleteField(f: FieldMeta) {
    setDelTarget(f);
    setDelCount(null);
    setDelTyped("");
    listRecordsPage(entityName, "limit=1")
      .then((p) => setDelCount(p.pagination.total))
      .catch(() => setDelCount(-1)); // -1 = đếm thất bại (hiện "không đếm được")
  }

  async function handleDeleteField(target: FieldMeta | null = delTarget) {
    if (!target?.id) return;
    const delTarget = target;
    setDelLoading(true);
    setDelFailed(null);
    try {
      await deleteField(delTarget.id!);
      const refreshed = await refreshFields();
      // specs/063 S7-9: on a delivered workspace the field is still there, marked "Sẽ xoá khi xuất bản".
      const deferred = refreshed.some((f) => f.id === delTarget.id && f.draftState === "deleted");
      if (deferred) {
        toast.info(tf("page.fieldEditor.deleteDeferred", { name: delTarget.label || delTarget.name }));
        emitDraftsChanged("field-delete");
        setInsp(null);
      }
      if (insp?.base.name === delTarget.name) setInsp(null);
      else if (insp && !refreshed.some((f) => f.name === insp.base.name)) setInsp(null);
      setDelTarget(null);
    } catch (e) {
      // "Xoá chưa xong · Thử lại" (specs/074 FR-064): the field stays, the maker can send the delete again.
      setListErr(e instanceof Error ? e.message : t("page.fieldEditor.deleteFailed"));
      setDelFailed(delTarget);
      setDelTarget(null);
    } finally {
      setDelLoading(false);
    }
  }

  // ── reorder (optimistic + rollback per contract §10.2) ──
  async function moveField(activeName: string, overName: string) {
    const snapshot = fields;
    const next = applyMove(fields, activeName, overName);
    if (!orderChanged(snapshot, next)) return;
    await qc.cancelQueries({ queryKey: fieldsKey });
    qc.setQueryData(fieldsKey, next); // optimistic — the list jumps at once
    setListErr(null);
    try {
      await reorderFields(entityName, reorderPayload(next));
      emitDraftsChanged("field-reorder");
    } catch (err) {
      qc.setQueryData(fieldsKey, snapshot); // all-or-nothing: the server rolled back, so do we
      setListErr(err instanceof Error ? err.message : t("page.fieldEditor.reorderFailed"));
    }
  }
  function moveBy(f: FieldMeta, delta: -1 | 1) {
    const i = fields.findIndex((x) => x.name === f.name);
    const neighbour = fields[i + delta];
    if (neighbour) void moveField(f.name, neighbour.name);
  }
  const reorderSensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));
  function onFieldDragStart(e: DragStartEvent) {
    setDragging(String(e.active.id));
  }
  function onFieldDragEnd(e: DragEndEvent) {
    setDragging(null);
    const { active, over } = e;
    if (!over || active.id === over.id) return;
    void moveField(String(active.id), String(over.id));
  }

  const columns = useMemo<ColumnDef<FieldMeta>[]>(() => [
    {
      id: "order",
      header: t("page.fieldEditor.col.order"),
      size: 72,
      enableSorting: false,
      cell: ({ row }) => (
        <DragHandle
          id={row.original.name}
          label={fill(t("page.fieldEditor.dragHandle"), { label: row.original.label || row.original.name })}
          disabled={!dragEnabled}
        />
      ),
    },
    {
      id: "label",
      header: t("page.fieldEditor.col.label"),
      accessorFn: (f) => f.label || f.name,
      cell: ({ row, getValue }) => (
        <>
          <button
            type="button"
            className="slds-button_reset slds-text-link slds-truncate"
            aria-current={insp?.base.name === row.original.name ? "true" : undefined}
            onClick={() => openInspector(row.original)}
          >
            {getValue<string>()}
          </button>
          {row.original.description && (
            <div className="slds-truncate slds-text-body_small slds-text-color_weak" title={row.original.description}>
              {row.original.description}
            </div>
          )}
        </>
      ),
    },
    { id: "name", header: t("page.fieldEditor.col.name"), accessorKey: "name" },
    {
      id: "type",
      header: t("page.fieldEditor.col.type"),
      accessorFn: (f) => typeLabel(f.fieldType),
      cell: ({ row, getValue }) => (
        <>
          <div>{getValue<string>()}</div>
          <FieldFlags f={row.original} entityName={entityName} />
        </>
      ),
    },
    {
      id: "required",
      header: t("studio.build.data.fields.required"),
      enableSorting: false,
      cell: ({ row }) => row.original.isRequired
        ? <TickMark label={t("studio.build.data.fields.required")} />
        : null,
    },
    {
      id: "onForm",
      header: t("studio.build.data.fields.onForm"),
      enableSorting: false,
      cell: ({ row }) => {
        if (!onFormSet) return null; // layout unknown (loading / failed): say nothing rather than guess
        return onFormSet.has(row.original.name)
          ? <TickMark label={t("studio.build.data.fields.onForm")} />
          : (
            <Link className="slds-text-link" href={studioHref(projectSlugAt(pathname), "dung/man-hinh/:bang", { bang: entityName }, { man: "form", truong: row.original.name })}>
              {t("studio.build.data.fields.place")}
            </Link>
          );
      },
    },
  ], [dragEnabled, insp?.base.name, onFormSet, entityName, pathname]);

  // Per-type config of the field being added: shown up front when the type cannot be created
  // without it (REQUIRED_CONFIG_TYPES), otherwise inside "Cấu hình nâng cao".
  const typeOptions = (
    <FieldTypeOptions type={afType}
      currencyCode={afCurrencyCode} onCurrency={setAfCurrencyCode}
      values={afValues} onValues={setAfValues}
      related={afRelated} onRelated={(v) => { setAfRelated(v); setAfDefault(""); }}
      polymorphicTargets={afPolyTargets} onPolymorphicTargets={setAfPolyTargets}
      entityOptions={entityOptions}
      numMin={afNumMin} numMax={afNumMax} numStep={afNumStep}
      onNumMin={setAfNumMin} onNumMax={setAfNumMax} onNumStep={setAfNumStep}
      textMaxLen={afTextMaxLen} onTextMaxLen={setAfTextMaxLen}
      ratingMax={afRatingMax} onRatingMax={setAfRatingMax}
      formula={afFormula} onFormula={setAfFormula}
      formulaResultType={afFormulaResultType} onFormulaResultType={setAfFormulaResultType}
      onFormulaValidity={setAfFormulaOk}
      subFields={afSubFields} onSubFields={setAfSubFields}
      entityFields={fields} currentEntityName={entityName}
      relationCfg={afRelation} onRelationCfg={setAfRelation}
      lookupCfg={afLookup} onLookupCfg={setAfLookup}
      rollupCfg={afRollup} onRollupCfg={setAfRollup}
      autoNumberCfg={afAutoNumber} onAutoNumberCfg={setAfAutoNumber}
    />
  );

  const inspTitle = draft ? draft.label || field?.name || "" : t("page.fieldEditor.inspector");
  const ft = (field?.fieldType ?? "text") as FieldType;
  const draggedField = dragging ? fields.find((f) => f.name === dragging) : undefined;

  const listBody = fieldsQ.error ? (
    <div className="p-3">
      <ScopedNotification theme="error">{fieldsQ.error.message}</ScopedNotification>
    </div>
  ) : fieldsQ.isSuccess && fields.length === 0 ? (
    <EmptyState
      className="p-6"
      icon={ListPlus}
      title={t("page.fieldEditor.emptyTitle")}
      description={t("page.fieldEditor.emptyDesc")}
      action={<div className="mt-3"><Button variant="brand" iconLeft={Plus} onClick={() => setShowAddField(true)}>{t("page.fieldEditor.add")}</Button></div>}
    />
  ) : q && visible.length === 0 ? (
    <EmptyState className="p-6" icon={SearchIcon} title={fill(t("page.fieldEditor.noMatch"), { q: fldSearch.trim() })} />
  ) : (
    <DndContext sensors={reorderSensors} collisionDetection={closestCenter} onDragStart={onFieldDragStart} onDragEnd={onFieldDragEnd} onDragCancel={() => setDragging(null)}>
      <SortableContext items={visible.map((f) => f.name)} strategy={verticalListSortingStrategy}>
        <DataTable
          label={t("page.fieldEditor.grid")}
          columns={columns}
          data={visible}
          getRowId={(f) => f.name}
          rowHeader="label"
          loading={fieldsQ.isLoading}
          rowActions={(f) => (
            <>
              <MenuItem label={t("page.fieldEditor.menu.edit")} onSelect={() => { openInspector(f); focusLabelSoon(); }} />
              <MenuItem label={t("page.fieldEditor.menu.up")} disabled={!dragEnabled || fields[0]?.name === f.name} onSelect={() => moveBy(f, -1)} />
              <MenuItem label={t("page.fieldEditor.menu.down")} disabled={!dragEnabled || fields[fields.length - 1]?.name === f.name} onSelect={() => moveBy(f, 1)} />
              <MenuDivider />
              <MenuItem label={t("page.fieldEditor.menu.delete")} disabled={!f.id} onSelect={() => openDeleteField(f)} />
            </>
          )}
        />
      </SortableContext>
      <DragOverlay>
        {draggedField && (
          <div className="slds-box slds-box_xx-small slds-theme_default flex items-center gap-2">
            <GripVertical aria-hidden="true" className="slds-icon slds-icon_x-small slds-icon-text-default nx-lucide" />
            {draggedField.label || draggedField.name}
          </div>
        )}
      </DragOverlay>
    </DndContext>
  );

  return (
    <div className="flex min-h-0 flex-1 overflow-hidden">
      <div className="flex min-w-0 flex-1 flex-col gap-3 overflow-y-auto p-4">
        {/* Nước 1 chain strip — the exact next stops after creating a field, at the moment of
            need. Both stops are the Màn hình page of this table, on the Form / the list view. */}
        {fieldChain && fieldChain.entity === entityName && (
          <ScopedNotification theme="success">
            <span className="flex flex-wrap items-center gap-2">
              <span>{fill(t("page.fieldEditor.chain"), { label: fieldChain.label })}</span>
              <Link href={studioHref(projectSlugAt(pathname), "dung/man-hinh/:bang", { bang: fieldChain.entity }, { man: "form" })}>{t("page.fieldEditor.chainForm")}</Link>
              <span aria-hidden="true">·</span>
              <Link href={studioHref(projectSlugAt(pathname), "dung/man-hinh/:bang", { bang: fieldChain.entity }, { man: "danh-sach" })}>{t("page.fieldEditor.chainList")}</Link>
              <ButtonIcon icon={X} variant="inverse" size="small" label={t("action.close")} onClick={() => setFieldChain(null)} />
            </span>
          </ScopedNotification>
        )}
        {listErr && (
          <ScopedNotification theme="error">
            <span className="flex items-center gap-2">
              <span>{delFailed ? `${t("page.fieldEditor.deleteNotDone")} · ${listErr}` : listErr}</span>
              {delFailed && <Button variant="base" disabled={delLoading} onClick={() => { setListErr(null); void handleDeleteField(delFailed); }}>{t("studio.query.retry")}</Button>}
              <ButtonIcon icon={X} variant="inverse" size="small" label={t("action.close")} onClick={() => { setListErr(null); setDelFailed(null); }} />
            </span>
          </ScopedNotification>
        )}

        {showAddField && (
          <div ref={addStripRef} className="slds-theme_shade slds-p-around_medium" role="region" aria-label={t("studio.build.data.addField.title")}
            onKeyDown={(e) => { if (e.key === "Escape") { e.stopPropagation(); resetAddField(); } }}>
            <h2 className="slds-text-heading_small slds-m-bottom_small">{t("studio.build.data.addField.title")}</h2>
            <form id="nx-add-field" className="flex flex-col gap-3" onSubmit={(e) => { e.preventDefault(); void handleAddField(); }}>
              <div className="grid grid-cols-2 gap-3">
                <Input label={t("page.fieldEditor.addLabel")} required value={afLabel} placeholder={t("page.fieldEditor.addLabelPh")}
                  onChange={(e) => {
                    setAfLabel(e.target.value);
                    if (!afNameTouched) setAfName(slugify(e.target.value));
                  }} />
                <Input label={t("page.fieldEditor.addName")} required value={afName} placeholder="contract_date"
                  onChange={(e) => { setAfName(e.target.value); setAfNameTouched(true); }} />
              </div>
    
              <div className="flex gap-4">
                <div className="shrink-0">
                  <TypePicker value={afType} onChange={pickAddType} />
                </div>
                <div className="flex min-w-0 flex-1 flex-col gap-3">
                  {REQUIRED_CONFIG_TYPES.has(afType) && typeOptions}
                  <ExpandableSection title={t("page.fieldEditor.advanced")} open={afAdvanced} onOpenChange={setAfAdvanced}>
                    <div className="flex flex-col gap-3">
                      <div className="flex flex-wrap items-start gap-4">
                        <Toggle label={t("page.fieldEditor.required")} checked={afRequired} onChange={(e) => setAfRequired(e.target.checked)} />
                        {!UNIQUE_UNSUPPORTED_TYPES.has(afType) && (
                          <span className="flex items-center gap-1">
                            <Toggle label={t("page.fieldEditor.unique")} checked={afUnique} onChange={(e) => {
                              setAfUnique(e.target.checked);
                              if (e.target.checked) setAfIndexed(false); // unique đã có cột index vật lý — toggle indexed thành thừa
                            }} />
                            <FieldHelp helpKey="fieldUnique" />
                          </span>
                        )}
                        {supportsIndexed(afType, afUnique) && (
                          <span className="flex items-center gap-1">
                            <Toggle label={t("page.fieldEditor.indexedShort")} checked={afIndexed} onChange={(e) => setAfIndexed(e.target.checked)} />
                            <FieldHelp text={t("page.fieldEditor.indexedHelp")} />
                          </span>
                        )}
                        {supportsSensitive(afType) && (
                          <span className="flex items-center gap-1">
                            <Toggle label={t("page.fieldEditor.sensitiveShort")} checked={afSensitive} onChange={(e) => setAfSensitive(e.target.checked)} />
                            <FieldHelp text={t("page.fieldEditor.sensitiveHelpCreate")} />
                          </span>
                        )}
                      </div>
    
                      {!REQUIRED_CONFIG_TYPES.has(afType) && typeOptions}
    
                      {supportsDefaultValue(afType) && (
                        <DefaultValueInput type={afType} label={t("page.fieldEditor.default")} help={t("page.fieldEditor.defaultHelpCreate")}
                          scalar={afDefault} multi={afDefaultMulti}
                          onScalar={setAfDefault} onMulti={setAfDefaultMulti}
                          picklistValues={rowsToValues(afValues)} picklistLabels={rowsToValueLabels(afValues)}
                          relatedEntity={afRelated} />
                      )}
    
                      <Input label={t("page.fieldEditor.addDescription")} value={afDesc} placeholder={t("page.fieldEditor.descriptionPh")}
                        onChange={(e) => setAfDesc(e.target.value.slice(0, DESCRIPTION_MAX))} />
                    </div>
                  </ExpandableSection>
                  {afErr && <ScopedNotification theme="error">{afErr}</ScopedNotification>}
                </div>
              </div>
            </form>
            <div className="slds-m-top_small slds-grid slds-grid_align-end">
              <Button onClick={resetAddField}>{t("action.cancel")}</Button>
              <Button variant="brand" className="slds-m-left_x-small" type="submit" form="nx-add-field" disabled={addDisabled} title={addBlockedReason}>
                {t("page.fieldEditor.add")}
              </Button>
            </div>
          </div>
        )}
        {delTarget && (
          <DeleteFieldConfirm
            field={delTarget}
            entityLabel={ent?.label ?? entityName}
            count={delCount}
            typed={delTyped}
            onTyped={setDelTyped}
            loading={delLoading}
            onCancel={() => { if (!delLoading) setDelTarget(null); }}
            onConfirm={() => void handleDeleteField()}
          />
        )}
        <Card
          title={t("page.fieldEditor.title")}
          count={fieldsQ.isSuccess ? fields.length : undefined}
          padded={false}
          actions={
            <div className="flex items-center gap-2">
              <Input
                label={t("page.fieldEditor.search")}
                labelHidden
                iconLeft={SearchIcon}
                clearable={fldSearch !== ""}
                value={fldSearch}
                placeholder={t("page.fieldEditor.searchPh")}
                onChange={(e) => setFldSearch(e.target.value)}
              />
              <Button variant="brand" iconLeft={Plus} onClick={() => { setAfErr(null); setShowAddField(true); }}>
                {t("page.fieldEditor.add")}
              </Button>
            </div>
          }
        >
          {q && fields.length > 0 && (
            <p className="slds-text-body_small slds-text-color_weak px-3 pb-2">{t("page.fieldEditor.dragOffWhileFiltering")}</p>
          )}
          {listBody}
        </Card>
      </div>

      {/* Field inspector — panels/base, docked right */}
      <Panel
        id="nx-field-inspector"
        open={!!insp}
        position="right"
        size="medium"
        title={inspTitle}
        onClose={() => setInsp(null)}
      >
        {insp && field && draft && (
          <div className="flex flex-col gap-3">
            <Input
              ref={labelRef}
              label={t("page.fieldEditor.label")}
              value={draft.label}
              onChange={(e) => patchDraft({ label: e.target.value })}
            />

            <FormElement label={t("page.fieldEditor.type")} readOnly="static" help={`${t("page.fieldEditor.typeLocked")} ${t("page.fieldEditor.typeChangeHint")}${delivered ? ` ${t("page.fieldEditor.typeChangeDelivered")}` : ""}`}>
              <span className="flex flex-wrap items-center gap-2">
                <span>{typeLabel(field.fieldType)}</span>
                <Badge variant="lightest">{field.fieldType}</Badge>
                {READONLY_FIELD_TYPES.has(field.fieldType) && <Badge iconLeft={Lock}>{t("page.fieldEditor.readonly")}</Badge>}
              </span>
            </FormElement>

            {(ft === "picklist" || ft === "multi_select") && (
              <FormElement label={t("page.fieldEditor.values")}>
                <PicklistRowsEditor rows={draft.rows} onChange={(rows) => patchDraft({ rows })} />
              </FormElement>
            )}
            {ft === "picklist" && (
              <PicklistDependency entityName={entityName} field={field} fields={fields} draft={draft} onChange={patchDraft} />
            )}
            {ft === "relation" && (
              <>
                <LockedValue label={t("page.fieldEditor.relTarget")} value={(field.options?.relatedEntity as string | undefined) || "—"} />
                <LockedValue label={t("page.fieldEditor.relCardinality")} value={(field.options?.cardinality as string | undefined) ?? t("page.fieldEditor.relCardinalityDefault")} />
                <LockedValue label={t("page.fieldEditor.relInverse")} value={(field.options?.inverseField as string | undefined) || t("page.fieldEditor.relInverseAuto")} />
                <p className="slds-text-body_small slds-text-color_weak">{t("page.fieldEditor.lockHint")}</p>
                <RelationFilter field={field} fields={fields} draft={draft} onChange={patchDraft} />
              </>
            )}
            {ft === "polymorphic_relation" && (
              <>
                <LockedValue
                  label={t("page.fieldEditor.polyTargets")}
                  value={((field.options?.targetEntities as string[] | undefined) ?? []).join(", ") || "—"}
                />
                <p className="slds-text-body_small slds-text-color_weak">{t("page.fieldEditor.lockHint")}</p>
              </>
            )}
            {ft === "currency" && (
              <>
                <div className="grid grid-cols-2 gap-2">
                  <Input label={t("page.fieldEditor.currencyCode")} value={draft.currency} placeholder="VND"
                    onChange={(e) => patchDraft({ currency: e.target.value.toUpperCase() })} />
                  <Input label={t("page.fieldEditor.precision")} type="number" min={0} max={8} value={draft.precision} placeholder="—"
                    onChange={(e) => patchDraft({ precision: e.target.value })} />
                  <Input label={t("page.fieldEditor.min")} type="number" value={draft.curMin} placeholder="—"
                    onChange={(e) => patchDraft({ curMin: e.target.value })} />
                  <Input label={t("page.fieldEditor.max")} type="number" value={draft.curMax} placeholder="—"
                    onChange={(e) => patchDraft({ curMax: e.target.value })} />
                </div>
                <p className="slds-text-body_small slds-text-color_weak">{t("page.fieldEditor.currencyMinHint")}</p>
              </>
            )}
            {ft === "number" && (
              <div className="grid grid-cols-3 gap-2">
                <Input label={t("page.fieldEditor.min")} type="number" value={draft.numMin} placeholder="—" onChange={(e) => patchDraft({ numMin: e.target.value })} />
                <Input label={t("page.fieldEditor.max")} type="number" value={draft.numMax} placeholder="—" onChange={(e) => patchDraft({ numMax: e.target.value })} />
                <Input label={t("page.fieldEditor.step")} type="number" value={draft.numStep} placeholder="—" onChange={(e) => patchDraft({ numStep: e.target.value })} />
              </div>
            )}
            {(ft === "text" || ft === "rich_text") && (
              <>
                <Input label={t("page.fieldEditor.maxLen")} type="number" min={1} value={draft.maxLen} placeholder="255"
                  onChange={(e) => patchDraft({ maxLen: e.target.value })} />
                <Input label={t("page.fieldEditor.pattern")} value={draft.pattern} placeholder={t("page.fieldEditor.patternPh")}
                  onChange={(e) => patchDraft({ pattern: e.target.value })} />
              </>
            )}
            {ft === "rating" && (
              <Input label={t("page.fieldEditor.ratingMax")} type="number" min={1} value={draft.ratingMax} placeholder="5"
                onChange={(e) => patchDraft({ ratingMax: e.target.value })} />
            )}
            {ft === "formula" && (
              <FormulaEditor entityName={entityName} formula={draft.formula} onFormula={(formula) => patchDraft({ formula })}
                resultType={(field.options?.resultType as string | undefined) ?? "number"}
                resultTypeLocked onValidity={setFEditFormulaOk} />
            )}
            {ft === "lookup" && (
              <LookupFields entityFields={fields} value={draft.lookup} onChange={(lookup) => patchDraft({ lookup })} />
            )}
            {ft === "rollup" && (field.options?.stored === true ? (
              <StoredRollupPanel field={field} />
            ) : (
              <RollupFields entityOptions={entityOptions} currentEntityName={entityName}
                value={draft.rollup} onChange={(rollup) => patchDraft({ rollup })} />
            ))}
            {ft === "auto_number" && (
              <AutoNumberFields value={draft.autoNumber} onChange={(autoNumber) => patchDraft({ autoNumber })} />
            )}

            {supportsDefaultValue(ft) && (
              <DefaultValueInput type={ft} label={t("page.fieldEditor.default")} help={t("page.fieldEditor.defaultHelp")}
                scalar={draft.defaultScalar} multi={draft.defaultMulti}
                onScalar={(defaultScalar) => patchDraft({ defaultScalar })} onMulti={(defaultMulti) => patchDraft({ defaultMulti })}
                picklistValues={rowsToValues(draft.rows)} picklistLabels={rowsToValueLabels(draft.rows)}
                relatedEntity={field.options?.relatedEntity as string | undefined} />
            )}

            <FormElement as="fieldset" label={t("page.fieldEditor.constraints")}>
              <div className="flex flex-col gap-2">
                <Toggle label={t("page.fieldEditor.required")} checked={draft.isRequired}
                  aria-describedby={draft.isRequired && (ft === "file" || ft === "image") ? "required-attachment-note" : undefined}
                  onChange={(e) => patchDraft({ isRequired: e.target.checked })} />
                {/* #350: file/image upload only after the record exists, so "required" cannot block creating it. */}
                {draft.isRequired && (ft === "file" || ft === "image") && (
                  <p id="required-attachment-note" className="slds-form-element__help">{t("page.fieldEditor.requiredAttachmentNote")}</p>
                )}
                {supportsIndexed(ft, !!field.isUnique) && (
                  <div>
                    <Toggle label={t("page.fieldEditor.indexed")} checked={draft.indexed}
                      aria-describedby={delivered ? "indexed-applies-now" : undefined}
                      onChange={(e) => patchDraft({ indexed: e.target.checked })} />
                    {delivered && <span id="indexed-applies-now"><Badge variant="lightest">{t("page.fieldEditor.appliesNow")}</Badge></span>}
                    <p className="slds-form-element__help">{t("page.fieldEditor.indexedHelp")}</p>
                  </div>
                )}
                {supportsSensitive(ft) && (
                  <div>
                    <Toggle label={t("page.fieldEditor.sensitive")} checked={draft.sensitive}
                      onChange={(e) => patchDraft({ sensitive: e.target.checked })} />
                    <p className="slds-form-element__help">{t("page.fieldEditor.sensitiveHelp")}</p>
                  </div>
                )}
                {field.isUnique && (
                  <p className="flex items-center gap-1">
                    <Icon icon={KeyRound} size="x-small" />
                    {t("page.fieldEditor.uniqueNote")}
                  </p>
                )}
                {/* STUDIO-DISCOVERABILITY 1.4a: per-field jump to the role visibility matrix,
                    preselected on exactly this entity+field (?entity=&field= deep-link). */}
                <Link href={workshop ? workshop.panelHref("quyen-truong", { entity: entityName, field: field.name }) : studioHref(projectSlugAt(pathname), "dung/nguoi-dung-quyen/quyen-truong", {}, { bang: entityName, truong: field.name })}>
                  {t("page.fieldEditor.fieldAcl")}
                </Link>
              </div>
            </FormElement>

            <Textarea
              label={t("page.fieldEditor.description")}
              rows={3}
              value={draft.description}
              placeholder={t("page.fieldEditor.descriptionPh")}
              help={fill(t("page.fieldEditor.descriptionHelp"), { n: draft.description.length, max: DESCRIPTION_MAX })}
              onChange={(e) => patchDraft({ description: e.target.value.slice(0, DESCRIPTION_MAX) })}
            />

            {/* Footer: delete · status · cancel · save — pinned to the bottom of the scrolling panel */}
            <div className="slds-theme_default slds-border_top sticky bottom-0 flex flex-wrap items-center gap-2 py-3">
              <Button variant="text-destructive" iconLeft={Trash2} disabled={!field.id} onClick={() => openDeleteField(field)}>
                {t("page.fieldEditor.delete")}
              </Button>
              <span className="ml-auto" />
              {fEditOk && <Badge theme="success">{t("page.fieldEditor.saved")}</Badge>}
              {fEditErr && <span role="alert" className="slds-text-color_error basis-full text-right">{fEditErr}</span>}
              <Button onClick={() => setInsp(null)}>{t("action.cancel")}</Button>
              <Button
                variant="brand"
                disabled={fEditSaving || !field.id || saveBlocked || !dirty}
                title={saveBlocked ? saveBlockedReason : !dirty ? t("page.fieldEditor.nothingChanged") : undefined}
                onClick={handleSaveField}
              >
                {t("action.save")}
              </Button>
            </div>
          </div>
        )}
      </Panel>

    </div>
  );
}

// ── Sub-components ──────────────────────────────────────────────────────────────────────────

/** Stored flags as weak plain text under the type (read-only; edit them in the inspector). Required has its own column. */
function FieldFlags({ f, entityName }: { f: FieldMeta; entityName: string }) {
  const parts = [
    f.isUnique && t("page.fieldEditor.flag.unique"),
    READONLY_FIELD_TYPES.has(f.fieldType) && t("page.fieldEditor.readonly"),
    f.options?.indexed === true && t("page.fieldEditor.flag.indexed"),
    f.options?.sensitive === true && t("page.fieldEditor.flag.sensitive"),
    f.draftState === "new" && t("page.fieldEditor.flag.newUntilPublish"),
    f.pendingRequired && t("page.fieldEditor.flag.requiredOnPublish"),
    f.draftState === "deleted" && t("page.fieldEditor.flag.deleteOnPublish"),
  ].filter(Boolean);
  const undo = f.draftState === "deleted" && f.id ? <UndoFieldDelete field={f} entityName={entityName} /> : null;
  if (parts.length === 0 && !undo) return null;
  return (
    <div className="slds-text-body_small slds-text-color_weak">
      {parts.join(" · ")} {undo}
    </div>
  );
}

function TickMark({ label }: { label: string }) {
  return (
    <span className="slds-icon_container">
      <Check size={16} aria-hidden="true" />
      <span className="slds-assistive-text">{label}</span>
    </span>
  );
}

/** Hoàn tác of "Sẽ xoá khi xuất bản" (specs/063 S7-9): discards the pending delete, then re-reads the list. */
function UndoFieldDelete({ field, entityName }: { field: FieldMeta; entityName: string }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  async function undo() {
    if (!field.id) return;
    setBusy(true);
    try {
      await undoDraftDelete("field", field.id);
      await qc.invalidateQueries({ queryKey: qk.fields(entityName) });
    } catch (e) {
      toast.error(e instanceof Error ? e.message : t("page.fieldEditor.undoFailed"));
    } finally {
      setBusy(false);
    }
  }
  return (
    <Button variant="base" disabled={busy} aria-label={tf("page.fieldEditor.undoDeleteLabel", { name: field.label || field.name })} onClick={() => void undo()}>
      {t("page.fieldEditor.undoDelete")}
    </Button>
  );
}

// Drag handle in the grid's first column. The row itself belongs to the DataTable, so only the
// handle registers with @dnd-kit (its rect is what collision detection measures); a DragOverlay
// shows the field under the pointer while the rows stay put.
function DragHandle({ id, label, disabled }: { id: string; label: string; disabled: boolean }) {
  const { attributes, listeners, setNodeRef, isDragging } = useSortable({ id, disabled });
  return (
    <button
      ref={setNodeRef}
      type="button"
      {...attributes}
      {...listeners}
      tabIndex={-1}
      className="slds-button slds-button_icon touch-none"
      style={{ cursor: disabled ? "default" : "grab", opacity: isDragging ? 0.4 : 1 }}
      title={label}
      disabled={disabled}
    >
      <GripVertical aria-hidden="true" className="slds-button__icon slds-button__icon_hint nx-lucide" />
      <span className="slds-assistive-text">{label}</span>
    </button>
  );
}

/** Field-level help (form-element blueprint): an info button with a tooltip. */
function FieldHelp({ helpKey, text }: { helpKey?: string; text?: string }) {
  const entry = helpKey ? getHelp(helpKey) : undefined;
  const content = text ?? entry?.short;
  if (!content) return null; // unknown key — fail quiet, never an empty icon
  const label = fill(t("page.fieldEditor.helpFor"), { term: entry?.term ?? t("page.fieldEditor.helpGeneric") });
  return (
    <Tooltip
      trigger={
        <button type="button" className="slds-button slds-button_icon">
          <Info aria-hidden="true" className="slds-button__icon nx-lucide" />
          <span className="slds-assistive-text">{label}</span>
        </button>
      }
    >
      {content}
    </Tooltip>
  );
}

/** Immutable-after-create value (relation targets) — read-only form element with the lock reason. */
function LockedValue({ label, value }: { label: string; value: string }) {
  return (
    <FormElement label={label} readOnly="static">
      <span className="flex items-center gap-1">
        <Icon icon={Lock} size="x-small" label={t("page.fieldEditor.locked")} />
        <span className="slds-truncate" title={value}>{value}</span>
      </span>
    </FormElement>
  );
}

// picklist / multi_select row editor — one row per value: drag to reorder, edit code + label, delete, add.
function PicklistRowsEditor({ rows, onChange }: { rows: PicklistRow[]; onChange: (rows: PicklistRow[]) => void }) {
  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));
  const dupes = duplicateIds(rows);
  function onDragEnd(e: DragEndEvent) {
    const { active, over } = e;
    if (!over || active.id === over.id) return;
    onChange(moveRow(rows, String(active.id), String(over.id)));
  }
  return (
    <div className="flex flex-col gap-1">
      {rows.length === 0 ? (
        <p className="slds-text-body_small slds-text-color_weak">{t("page.fieldEditor.valuesEmpty")}</p>
      ) : (
        <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onDragEnd}>
          <SortableContext items={rows.map((r) => r.id)} strategy={verticalListSortingStrategy}>
            {rows.map((r) => (
              <PicklistRowItem key={r.id} row={r} dupe={dupes.has(r.id)}
                onRename={(v) => onChange(renameRow(rows, r.id, v))}
                onRelabel={(v) => onChange(relabelRow(rows, r.id, v))}
                onRemove={() => onChange(removeRow(rows, r.id))}
                onEnter={() => onChange(addRow(rows))} />
            ))}
          </SortableContext>
        </DndContext>
      )}
      {dupes.size > 0 && <p className="slds-text-body_small slds-text-color_error">{t("page.fieldEditor.valuesDupes")}</p>}
      <div className="flex items-center justify-between gap-2">
        <Button iconLeft={Plus} onClick={() => onChange(addRow(rows))}>{t("page.fieldEditor.valuesAdd")}</Button>
        <span className="slds-text-body_small slds-text-color_weak">{fill(t("page.fieldEditor.valuesCount"), { n: rowsToValues(rows).length })}</span>
      </div>
    </div>
  );
}

function PicklistRowItem({ row, dupe, onRename, onRelabel, onRemove, onEnter }: {
  row: PicklistRow; dupe: boolean; onRename: (v: string) => void; onRelabel: (v: string) => void; onRemove: () => void; onEnter?: () => void;
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: row.id });
  const style = { transform: CSS.Transform.toString(transform), transition, opacity: isDragging ? 0.5 : 1 };
  const name = row.value || t("page.fieldEditor.valueCode");
  return (
    <div ref={setNodeRef} style={style} className="flex items-start gap-1">
      <button type="button" {...attributes} {...listeners} className="slds-button slds-button_icon touch-none" title={t("page.fieldEditor.valueDrag")}>
        <GripVertical aria-hidden="true" className="slds-button__icon slds-button__icon_hint nx-lucide" />
        <span className="slds-assistive-text">{t("page.fieldEditor.valueDrag")}</span>
      </button>
      {/* Dogfood: a freshly-added row must be TYPEABLE immediately (autoFocus — a persisted row is
          never empty, rowsToValues drops blanks) and Enter appends the next row instead of
          submitting the surrounding field form — the fast-entry loop for picklist values. */}
      <div className="min-w-0 flex-1">
        <Input label={t("page.fieldEditor.valueCode")} labelHidden value={row.value} placeholder={t("page.fieldEditor.valueCodePh")}
          autoFocus={row.value === ""}
          error={dupe ? t("page.fieldEditor.valueDupe") : undefined}
          onChange={(e) => onRename(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); if (row.value.trim() !== "") onEnter?.(); } }} />
      </div>
      {/* Display label (options.valueLabels): what end users READ; the code on the left is what the
          system STORES. Two boxes so a label edit never touches the code — renaming a code orphans records. */}
      <div className="min-w-0 flex-1">
        <Input label={t("page.fieldEditor.valueLabel")} labelHidden value={row.label ?? ""} placeholder={t("page.fieldEditor.valueLabelPh")}
          onChange={(e) => onRelabel(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); if (row.value.trim() !== "") onEnter?.(); } }} />
      </div>
      <ButtonIcon icon={Trash2} label={fill(t("page.fieldEditor.valueRemove"), { value: name })} onClick={onRemove} />
    </div>
  );
}

// formula editor with LIVE validation — debounced POST /admin/formula/validate (dry-run through
// the SAME validator as the real save). State via formulaReducer (pure, unit-tested).
function FormulaEditor({
  entityName, formula, onFormula, resultType, onResultType, resultTypeLocked = false, onValidity,
}: {
  entityName: string;
  formula: string;
  onFormula: (v: string) => void;
  resultType: string;
  onResultType?: (v: string) => void;
  resultTypeLocked?: boolean;
  onValidity?: (ok: boolean | null) => void;
}) {
  const [state, dispatch] = useReducer(formulaReducer, undefined, initialFormulaState);
  const seqRef = useRef(0);

  // Keystroke → typing (new seq) → debounce → request; the reducer drops any out-of-seq response.
  useEffect(() => {
    const seq = ++seqRef.current;
    dispatch({ type: "input", formula, seq });
    const body = shapeValidateRequest(entityName, formula, resultType);
    if (!body) return;
    const timer = setTimeout(() => {
      dispatch({ type: "debounce-fire", seq });
      validateFormula(body.entityName, body.formula, body.resultType)
        .then((r) => dispatch({ type: "response-ok", seq, fields: r.fields, resultType: r.resultType }))
        .catch((e) => dispatch({
          type: "response-err", seq,
          code: e instanceof ApiError ? e.code : "NETWORK",
          message: e instanceof Error ? e.message : t("page.fieldEditor.formulaCheckFailed"),
        }));
    }, FORMULA_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [formula, resultType, entityName]);

  // Validity goes up to the caller to gate the save button (valid=true / invalid=false / typing|idle=null).
  useEffect(() => {
    onValidity?.(state.kind === "valid" ? true : state.kind === "invalid" ? false : null);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [state.kind]);

  let help: ReactNode = null;
  let error: ReactNode;
  if (state.kind === "checking" || state.kind === "typing") help = t("page.fieldEditor.formulaChecking");
  else if (state.kind === "valid") {
    help = (
      <span className="slds-text-color_success flex flex-wrap items-center gap-1">
        {t("page.fieldEditor.formulaValid")}
        {state.fields.length > 0 && <>{" · "}{t("page.fieldEditor.formulaRefs")} {state.fields.map((f) => <Badge key={f} variant="lightest">{f}</Badge>)}</>}
        {" → "}{state.resultType}
      </span>
    );
  } else if (state.kind === "invalid") error = state.message; // the server sentence already reads in Vietnamese; the code is for logs
  else help = t("page.fieldEditor.formulaIdle");

  return (
    <div className="flex flex-col gap-2">
      <Input
        label={t("page.fieldEditor.formula")}
        fieldLevelHelp={<FieldHelp helpKey="formula" />}
        value={formula}
        placeholder={t("page.fieldEditor.formulaPh")}
        help={help}
        error={error}
        onChange={(e) => onFormula(e.target.value)}
      />
      {resultTypeLocked ? (
        <FormElement label={t("page.fieldEditor.formulaResult")} readOnly="static" help={t("page.fieldEditor.formulaResultLocked")}>
          <span className="flex items-center gap-1">
            <Icon icon={Lock} size="x-small" label={t("page.fieldEditor.locked")} />
            {resultType}
          </span>
        </FormElement>
      ) : (
        <Select
          label={t("page.fieldEditor.formulaResult")}
          value={resultType}
          placeholder={false}
          options={[
            { value: "number", label: t("page.fieldEditor.formulaResultNumber") },
            { value: "currency", label: t("page.fieldEditor.formulaResultCurrency") },
          ]}
          onChange={(e) => onResultType?.(e.target.value)}
        />
      )}
    </div>
  );
}

function FieldTypeOptions({
  type, currencyCode, onCurrency, values, onValues, related, onRelated, entityOptions,
  polymorphicTargets, onPolymorphicTargets,
  numMin, numMax, numStep, onNumMin, onNumMax, onNumStep,
  textMaxLen, onTextMaxLen, ratingMax, onRatingMax,
  formula, onFormula, formulaResultType, onFormulaResultType, onFormulaValidity,
  subFields, onSubFields,
  entityFields, currentEntityName,
  relationCfg, onRelationCfg, lookupCfg, onLookupCfg, rollupCfg, onRollupCfg, autoNumberCfg, onAutoNumberCfg,
}: {
  type: FieldType;
  currencyCode: string; onCurrency: (v: string) => void;
  values: PicklistRow[]; onValues: (v: PicklistRow[]) => void;
  related: string; onRelated: (v: string) => void;
  polymorphicTargets: string[]; onPolymorphicTargets: (v: string[]) => void; // polymorphic_relation allowlist
  entityOptions: ComboboxOption[];
  numMin: string; numMax: string; numStep: string;
  onNumMin: (v: string) => void; onNumMax: (v: string) => void; onNumStep: (v: string) => void;
  textMaxLen: string; onTextMaxLen: (v: string) => void;
  ratingMax: string; onRatingMax: (v: string) => void;
  formula: string; onFormula: (v: string) => void;
  formulaResultType: string; onFormulaResultType: (v: string) => void;
  onFormulaValidity: (ok: boolean | null) => void;
  subFields: string; onSubFields: (v: string) => void;
  entityFields: FieldMeta[]; currentEntityName: string;
  relationCfg: RelationCfg; onRelationCfg: (v: RelationCfg) => void;
  lookupCfg: LookupCfg; onLookupCfg: (v: LookupCfg) => void;
  rollupCfg: RollupCfg; onRollupCfg: (v: RollupCfg) => void;
  autoNumberCfg: AutoNumberCfg; onAutoNumberCfg: (v: AutoNumberCfg) => void;
}) {
  if (type === "currency") return (
    <Input label={t("page.fieldEditor.currencyCode")} value={currencyCode} placeholder="VND"
      onChange={(e) => onCurrency(e.target.value.toUpperCase())} />
  );
  if (type === "picklist" || type === "multi_select") return (
    <FormElement label={t("page.fieldEditor.values")}>
      <PicklistRowsEditor rows={values} onChange={onValues} />
    </FormElement>
  );
  if (type === "relation") return (
    <div className="flex flex-col gap-2">
      <Combobox variant="autocomplete" label={t("page.fieldEditor.relTarget")} options={entityOptions}
        value={related || null} onChange={(v) => onRelated(v ?? "")} placeholder={t("page.fieldEditor.pickEntity")} />
      <div className="grid grid-cols-2 gap-2">
        <Select label={t("page.fieldEditor.relCardinality")} fieldLevelHelp={<FieldHelp helpKey="relationCardinality" />}
          value={relationCfg.cardinality} placeholder={false}
          options={[
            { value: "1-N", label: t("page.fieldEditor.relCardinalityDefault") },
            { value: "1-1", label: "1-1" },
          ]}
          onChange={(e) => onRelationCfg({ ...relationCfg, cardinality: e.target.value as "1-N" | "1-1" })} />
        <Input label={t("page.fieldEditor.relInverse")} fieldLevelHelp={<FieldHelp helpKey="inverseField" />}
          value={relationCfg.inverseField} placeholder={t("page.fieldEditor.relInversePh")}
          onChange={(e) => onRelationCfg({ ...relationCfg, inverseField: e.target.value })} />
      </div>
    </div>
  );
  if (type === "polymorphic_relation") return (
    // Multi-target allowlist: the value {type,id} may point at a record in ANY chosen table.
    <Combobox variant="autocomplete" multiple label={fill(t("page.fieldEditor.polyTargetsCount"), { n: polymorphicTargets.length })}
      options={entityOptions} value={polymorphicTargets} onChange={onPolymorphicTargets}
      placeholder={t("page.fieldEditor.pickEntity")}
      help={polymorphicTargets.length === 0 ? t("page.fieldEditor.polyHelp") : undefined} />
  );
  if (type === "lookup") return <LookupFields entityFields={entityFields} value={lookupCfg} onChange={onLookupCfg} />;
  if (type === "rollup") return <RollupFields entityOptions={entityOptions} currentEntityName={currentEntityName} value={rollupCfg} onChange={onRollupCfg} />;
  if (type === "auto_number") return <AutoNumberFields value={autoNumberCfg} onChange={onAutoNumberCfg} />;
  if (type === "number") return (
    <div className="grid grid-cols-3 gap-2">
      <Input label={t("page.fieldEditor.min")} type="number" value={numMin} placeholder="—" onChange={(e) => onNumMin(e.target.value)} />
      <Input label={t("page.fieldEditor.max")} type="number" value={numMax} placeholder="—" onChange={(e) => onNumMax(e.target.value)} />
      <Input label={t("page.fieldEditor.step")} type="number" value={numStep} placeholder="—" onChange={(e) => onNumStep(e.target.value)} />
    </div>
  );
  if (type === "text" || type === "rich_text") return (
    <Input label={t("page.fieldEditor.maxLen")} type="number" value={textMaxLen} placeholder="255" onChange={(e) => onTextMaxLen(e.target.value)} />
  );
  if (type === "rating") return (
    <Input label={t("page.fieldEditor.ratingMax")} type="number" value={ratingMax} placeholder="5" onChange={(e) => onRatingMax(e.target.value)} />
  );
  // Formula field: live-validated editor (dry-run via /admin/formula/validate)
  if (type === "formula") return (
    <FormulaEditor entityName={currentEntityName}
      formula={formula} onFormula={onFormula}
      resultType={formulaResultType} onResultType={onFormulaResultType}
      onValidity={onFormulaValidity} />
  );
  // Composite field: sub-field editor (P1 gap #9)
  if (type === "composite") return (
    <Input label={t("page.fieldEditor.composite")} fieldLevelHelp={<FieldHelp helpKey="composite" />}
      value={subFields} placeholder={t("page.fieldEditor.compositePh")}
      help={t("page.fieldEditor.compositeHelp")}
      onChange={(e) => onSubFields(e.target.value)} />
  );
  return null;
}

// Type-appropriate "Giá trị mặc định" input — ONE component for both the add-field modal and the
// inspector, so the two never drift.
function DefaultValueInput({ type, label, help, scalar, multi, onScalar, onMulti, picklistValues, picklistLabels, relatedEntity }: {
  type: FieldType;
  label: string;
  help?: string;
  scalar: string; onScalar: (v: string) => void;
  multi: string[]; onMulti: (v: string[]) => void;
  picklistValues: string[]; // current options.values (add form: afValues; inspector: draft rows)
  picklistLabels?: Record<string, string>; // #274: code → Vietnamese label, shown instead of the code
  relatedEntity?: string; // relation: table whose records the picker lists (queue #320)
}) {
  const kind = defaultInputKind(type);
  const shown = (v: string) => picklistLabels?.[v] ?? v;
  if (kind === "record") {
    const spec = { name: "default", label, fieldType: "relation" as const, options: { relatedEntity: relatedEntity ?? "" }, helpText: help };
    return (
      <FieldRow field={spec}>
        <FieldInput field={spec} value={scalar} onChange={(v) => onScalar(typeof v === "string" ? v : "")} />
      </FieldRow>
    );
  }
  const stale = t("page.fieldEditor.defaultStale");
  if (kind === "text") return (
    <Input label={label} help={help} value={scalar} onChange={(e) => onScalar(e.target.value)}
      placeholder={type === "email" ? t("page.fieldEditor.defaultEmailPh") : t("page.fieldEditor.defaultNonePh")} />
  );
  if (kind === "number") return <Input label={label} help={help} type="number" value={scalar} placeholder="—" onChange={(e) => onScalar(e.target.value)} />;
  if (kind === "boolean") return (
    <Select label={label} help={help} value={scalar} placeholder={false} onChange={(e) => onScalar(e.target.value)}
      options={[
        { value: "", label: t("page.fieldEditor.defaultNone") },
        { value: "true", label: t("page.fieldEditor.defaultTrue") },
        { value: "false", label: t("page.fieldEditor.defaultFalse") },
      ]} />
  );
  // Native date / datetime-local: the BE's exact YYYY-MM-DD / naive wall-clock layouts (fieldOptions).
  if (kind === "date") return <Input label={label} help={help} type="date" value={scalar} onChange={(e) => onScalar(e.target.value)} />;
  if (kind === "datetime") return <Input label={label} help={help} type="datetime-local" value={scalar} onChange={(e) => onScalar(e.target.value)} />;
  if (kind === "select") {
    // A stored value just removed from the list must still show (withStoredFallback pattern) —
    // clearing it is the admin's decision, not a silent drop; the BE 422s an orphaned default.
    const opts = scalar && !picklistValues.includes(scalar) ? [scalar, ...picklistValues] : picklistValues;
    return (
      <Select label={label} help={help} value={scalar} placeholder={t("page.fieldEditor.defaultNone")} onChange={(e) => onScalar(e.target.value)}
        options={opts.map((v) => ({ value: v, label: picklistValues.includes(v) ? shown(v) : `${v} ${stale}` }))} />
    );
  }
  if (kind === "multi") {
    if (picklistValues.length === 0 && multi.length === 0) {
      return <FormElement label={label} help={t("page.fieldEditor.defaultMultiEmpty")} />;
    }
    const all = [...multi.filter((v) => !picklistValues.includes(v)), ...picklistValues];
    return (
      <CheckboxGroup label={label} help={help} value={multi} onValueChange={onMulti}
        options={all.map((v) => ({ value: v, label: picklistValues.includes(v) ? shown(v) : `${v} ${stale}` }))} />
    );
  }
  return null; // kind === null: the caller gated on supportsDefaultValue — belt-and-suspenders
}

// Type picker — visual-picker/vertical rows in the four groups, with type-to-filter.
function TypePicker({ value, onChange }: { value: FieldType; onChange: (t: FieldType) => void }) {
  const [q, setQ] = useState("");
  const needle = q.trim().toLowerCase();
  const groups = TYPE_GROUPS.map((g) => ({
    ...g,
    types: g.types.filter((ty) =>
      !needle || ty.includes(needle)
      || FIELD_TYPE_META[ty].label.toLowerCase().includes(needle)
      || typeExplain(ty).toLowerCase().includes(needle)),
  })).filter((g) => g.types.length > 0);
  return (
    <FormElement as="fieldset" label={t("page.fieldEditor.addType")} fieldLevelHelp={<FieldHelp helpKey="fieldType" />}>
      <div className="flex flex-col gap-2">
        {/* Inside the add-field <form>: Enter would press "Thêm trường" (implicit submission) and
            create the field with whatever type was checked — a type is locked after creation. */}
        <Input label={t("page.fieldEditor.typeSearch")} labelHidden iconLeft={SearchIcon} clearable={q !== ""} value={q}
          placeholder={t("page.fieldEditor.typeSearchPh")} onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key !== "Enter") return;
            e.preventDefault();
            const first = groups[0]?.types[0];
            if (first) onChange(first);
          }} />
        <div className="max-h-96 overflow-y-auto">
          {groups.length === 0 && <p className="slds-text-color_weak p-2">{t("page.fieldEditor.typeNoMatch")}</p>}
          {groups.map((g) => (
            <VisualPicker
              key={g.title}
              variant="vertical"
              name="nx-add-field-type"
              label={t(g.title)}
              value={value}
              onChange={(v) => onChange(v as FieldType)}
              options={g.types.map((ty) => ({
                value: ty,
                label: FIELD_TYPE_META[ty].pending ? `${FIELD_TYPE_META[ty].label} (beta)` : FIELD_TYPE_META[ty].label,
                description: `${typeExplain(ty)} · ${ty}`,
                icon: TYPE_ICON[ty],
              }))}
            />
          ))}
        </div>
      </div>
    </FormElement>
  );
}

// lookup (ENG-COMPUTED #2) — options.via (to-one relation field on THIS table) + options.sourceField
// (field on the table `via` points to).
function LookupFields({ entityFields, value, onChange }: {
  entityFields: FieldMeta[]; value: LookupCfg; onChange: (v: LookupCfg) => void;
}) {
  const relationFields = entityFields.filter((f) => f.fieldType === "relation");
  const viaField = relationFields.find((f) => f.name === value.via);
  const targetEntity = (viaField?.options?.relatedEntity as string | undefined) ?? "";
  const targetQ = useQuery({
    queryKey: qk.fields(targetEntity),
    queryFn: () => listFields(targetEntity),
    enabled: !!targetEntity,
  });
  // Nested-computed guard is server-enforced (FIELD_FORMULA_NESTED) but filtering here too means
  // the list never offers a value that would bounce — a lookup can't mirror another computed field.
  const sourceCandidates = withStoredFallback(
    (targetQ.data ?? []).filter((f) => !READONLY_FIELD_TYPES.has(f.fieldType)),
    value.sourceField,
  );
  const loading = !!targetEntity && targetQ.isLoading;
  return (
    <div className="flex flex-col gap-2">
      <Combobox variant="autocomplete" label={t("page.fieldEditor.lookupVia")} fieldLevelHelp={<FieldHelp helpKey="lookup" />}
        options={withStoredFallback(relationFields, value.via).map(toOption)} value={value.via || null}
        onChange={(v) => onChange({ via: v ?? "", sourceField: "" })} placeholder={t("page.fieldEditor.lookupViaPh")}
        help={relationFields.length === 0 ? t("page.fieldEditor.lookupNoRelation") : undefined} />
      <Combobox variant="autocomplete" label={t("page.fieldEditor.lookupSource")}
        options={sourceCandidates.map(toOption)} value={value.sourceField || null}
        onChange={(v) => onChange({ ...value, sourceField: v ?? "" })}
        disabled={!targetEntity || loading} loading={loading}
        placeholder={!targetEntity ? t("page.fieldEditor.lookupPickViaFirst") : t("page.fieldEditor.pickField")}
        error={targetQ.error ? targetQ.error.message : undefined} />
      <p className="slds-text-body_small slds-text-color_weak">{t("page.fieldEditor.lookupNote")}</p>
    </div>
  );
}

// rollup (ENG-COMPUTED #3) — options.childEntity + options.metric + options.aggField (not for
// count) + exactly ONE of options.viaRelation (1-N) or options.relationship (N-N).
function RollupFields({ entityOptions, currentEntityName, value, onChange }: {
  entityOptions: ComboboxOption[]; currentEntityName: string; value: RollupCfg; onChange: (v: RollupCfg) => void;
}) {
  const childQ = useQuery({
    queryKey: qk.fields(value.childEntity),
    queryFn: () => listFields(value.childEntity),
    enabled: !!value.childEntity,
  });
  const relQ = useQuery({
    queryKey: ["studio", "relationships", currentEntityName],
    queryFn: () => listAllRelationships(currentEntityName),
    enabled: !!value.childEntity && value.mode === "relationship",
  });
  const childFields = childQ.data ?? [];
  const loadingChild = !!value.childEntity && childQ.isLoading;
  const loadingRel = !!value.childEntity && value.mode === "relationship" && relQ.isLoading;
  const nn = (relQ.data ?? []).filter((r) => r.relationshipType === "N-N" &&
    ((r.parentEntityName === currentEntityName && r.childEntityName === value.childEntity) ||
     (r.parentEntityName === value.childEntity && r.childEntityName === currentEntityName)));

  const numericFields = withStoredFallback(childFields.filter((f) => isRollupAggregatable(f, childFields)), value.aggField);
  const relationFieldsOnChild = withStoredFallback(
    childFields.filter((f) => f.fieldType === "relation" && (f.options?.relatedEntity as string | undefined) === currentEntityName),
    value.viaRelation,
  );
  const relationshipCandidates = withStoredFallback(nn.map((r) => ({ name: r.name, label: r.name })), value.relationship);
  const pickChildFirst = t("page.fieldEditor.rollupPickChildFirst");

  return (
    <div className="flex flex-col gap-2">
      <div className="grid grid-cols-2 gap-2">
        <Combobox variant="autocomplete" label={t("page.fieldEditor.rollupChild")} fieldLevelHelp={<FieldHelp helpKey="rollup" />}
          options={entityOptions} value={value.childEntity || null}
          onChange={(v) => onChange({ ...value, childEntity: v ?? "", aggField: "", viaRelation: "", relationship: "" })}
          placeholder={t("page.fieldEditor.pickEntity")} />
        <Select label={t("page.fieldEditor.rollupMetric")} value={value.metric} placeholder={false}
          onChange={(e) => onChange({ ...value, metric: e.target.value as RollupCfg["metric"] })}
          options={[
            { value: "count", label: t("page.fieldEditor.metric.count") },
            { value: "sum", label: t("page.fieldEditor.metric.sum") },
            { value: "avg", label: t("page.fieldEditor.metric.avg") },
            { value: "min", label: t("page.fieldEditor.metric.min") },
            { value: "max", label: t("page.fieldEditor.metric.max") },
          ]} />
      </div>

      {value.metric !== "count" && (
        <Combobox variant="autocomplete" label={t("page.fieldEditor.rollupAgg")}
          options={numericFields.map(toOption)} value={value.aggField || null}
          onChange={(v) => onChange({ ...value, aggField: v ?? "" })}
          disabled={!value.childEntity || loadingChild} loading={loadingChild}
          placeholder={!value.childEntity ? pickChildFirst : t("page.fieldEditor.pickField")}
          help={value.childEntity && !loadingChild && numericFields.length === 0 ? t("page.fieldEditor.rollupNoNumeric") : undefined} />
      )}

      <RadioButtonGroup label={t("page.fieldEditor.rollupJoin")} value={value.mode}
        onValueChange={(mode) => onChange(mode === "viaRelation"
          ? { ...value, mode: "viaRelation", relationship: "" }
          : { ...value, mode: "relationship", viaRelation: "" })}
        options={[
          { value: "viaRelation", label: t("page.fieldEditor.rollupVia") },
          { value: "relationship", label: t("page.fieldEditor.rollupNN") },
        ]} />

      {value.mode === "viaRelation" ? (
        <Combobox variant="autocomplete" label={t("page.fieldEditor.rollupViaField")}
          options={relationFieldsOnChild.map(toOption)} value={value.viaRelation || null}
          onChange={(v) => onChange({ ...value, viaRelation: v ?? "" })}
          disabled={!value.childEntity || loadingChild} loading={loadingChild}
          placeholder={!value.childEntity ? pickChildFirst : t("page.fieldEditor.pickField")}
          help={value.childEntity && !loadingChild && relationFieldsOnChild.length === 0 ? t("page.fieldEditor.rollupNoVia") : undefined}
          error={childQ.error ? childQ.error.message : undefined} />
      ) : (
        <Combobox variant="autocomplete" label={t("page.fieldEditor.rollupRelationship")}
          options={relationshipCandidates.map((r) => ({ value: r.name, label: r.label ?? r.name }))}
          value={value.relationship || null}
          onChange={(v) => onChange({ ...value, relationship: v ?? "" })}
          disabled={!value.childEntity || loadingRel} loading={loadingRel}
          placeholder={!value.childEntity ? pickChildFirst : t("page.fieldEditor.rollupPickRelationship")}
          help={value.childEntity && !loadingRel && relationshipCandidates.length === 0 ? t("page.fieldEditor.rollupNoNN") : undefined} />
      )}
      <div>
        <Toggle label={t("page.fieldEditor.rollupStored")} checked={!!value.stored && value.mode === "viaRelation"}
          disabled={value.mode !== "viaRelation"}
          onChange={(e) => onChange({ ...value, stored: e.target.checked })} />
        <p className="slds-form-element__help">
          {value.mode !== "viaRelation" ? t("page.fieldEditor.rollupStoredNN") : t("page.fieldEditor.rollupStoredHelp")}
        </p>
      </div>
      <p className="slds-text-body_small slds-text-color_weak">
        {value.stored && value.mode === "viaRelation" ? t("page.fieldEditor.rollupNoteStored") : t("page.fieldEditor.rollupNote")}
      </p>
    </div>
  );
}

function metricLabel(metric: string): string {
  switch (metric) {
    case "sum": return t("page.fieldEditor.metric.sum");
    case "avg": return t("page.fieldEditor.metric.avg");
    case "min": return t("page.fieldEditor.metric.min");
    case "max": return t("page.fieldEditor.metric.max");
    default: return t("page.fieldEditor.metric.count");
  }
}

// A roll-up that keeps its total in the record (specs/083). What it totals is fixed once rows hold
// numbers, so the inspector shows the definition read-only and offers "Tính lại" — recompute every
// row now (after a data repair, or if a total ever looks off).
function StoredRollupPanel({ field }: { field: FieldMeta }) {
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ theme: "success" | "error"; text: string } | null>(null);
  const o = field.options ?? {};
  async function recompute() {
    if (!field.id) return;
    setBusy(true); setMsg(null);
    try {
      const r = await recomputeStoredRollup(field.id);
      setMsg({ theme: "success", text: t("page.fieldEditor.rollupRecomputed").replace("{n}", String(r.updated)) });
    } catch (e) {
      setMsg({ theme: "error", text: e instanceof Error ? e.message : t("page.fieldEditor.rollupRecomputeFailed") });
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="flex flex-col gap-2">
      <LockedValue label={t("page.fieldEditor.rollupChild")} value={(o.childEntity as string | undefined) || "—"} />
      <LockedValue label={t("page.fieldEditor.rollupMetric")} value={metricLabel((o.metric as string | undefined) ?? "count")} />
      {typeof o.aggField === "string" && o.aggField !== "" && (
        <LockedValue label={t("page.fieldEditor.rollupAgg")} value={o.aggField} />
      )}
      <LockedValue label={t("page.fieldEditor.rollupViaField")} value={(o.viaRelation as string | undefined) || "—"} />
      <p className="slds-text-body_small slds-text-color_weak">{t("page.fieldEditor.rollupStoredLocked")}</p>
      <div>
        <Button variant="neutral" disabled={busy} onClick={() => void recompute()}>
          {busy ? t("page.fieldEditor.rollupRecomputing") : t("page.fieldEditor.rollupRecompute")}
        </Button>
      </div>
      {msg && <ScopedNotification theme={msg.theme}>{msg.text}</ScopedNotification>}
    </div>
  );
}

// auto_number (ENG #6) — options.prefix (≤20 chars) + options.padding (0..12).
function AutoNumberFields({ value, onChange }: { value: AutoNumberCfg; onChange: (v: AutoNumberCfg) => void }) {
  const paddingNum = value.padding === "" ? 0 : Math.max(0, Math.min(12, parseInt(value.padding, 10) || 0));
  const preview = `${value.prefix || ""}${String(42).padStart(paddingNum, "0")}`;
  return (
    <div className="flex flex-col gap-2">
      <div className="grid grid-cols-2 gap-2">
        <Input label={t("page.fieldEditor.autoPrefix")} fieldLevelHelp={<FieldHelp helpKey="autoNumber" />}
          value={value.prefix} maxLength={20} placeholder="INV-"
          onChange={(e) => onChange({ ...value, prefix: e.target.value })} />
        <Input label={t("page.fieldEditor.autoPadding")} type="number" min={0} max={12} value={value.padding} placeholder="5"
          onChange={(e) => onChange({ ...value, padding: e.target.value })} />
      </div>
      <p className="slds-text-body_small slds-text-color_weak flex flex-wrap items-center gap-1">
        {t("page.fieldEditor.autoPreview")} <Badge variant="lightest">{preview}</Badge> · {t("page.fieldEditor.autoNote")}
      </p>
    </div>
  );
}

// Delete a field in place (specs/074 F2 lát 6). Soft-delete metadata: typed code required once the table has records.
function DeleteFieldConfirm({ field, entityLabel, count, typed, onTyped, loading, onCancel, onConfirm }: {
  field: FieldMeta; entityLabel: string; count: number | null; typed: string; onTyped: (v: string) => void;
  loading: boolean; onCancel: () => void; onConfirm: () => void;
}) {
  const needsName = typeof count === "number" && count > 0;
  const locked = count === null || (needsName && typed !== field.name);
  let impact: string;
  if (count === null) impact = t("page.fieldEditor.delCounting");
  else if (count === -1) impact = t("page.fieldEditor.delCountFailed");
  else if (count === 0) impact = t("page.fieldEditor.delNoRecords");
  else impact = fill(t("page.fieldEditor.delRecords"), { n: count.toLocaleString("vi-VN"), entity: entityLabel });
  return (
    <InlineConfirm
      tone="error"
      message={fill(t("page.fieldEditor.delTitle"), { label: field.label || field.name })}
      confirmLabel={t("page.fieldEditor.delete")}
      onConfirm={onConfirm}
      onCancel={onCancel}
      busy={loading}
      confirmDisabled={locked}
    >
      <div className="flex flex-col gap-3">
        <p>{t("page.fieldEditor.delBody")}</p>
        <ul className="slds-list_dotted">
          <li>{impact}</li>
        </ul>
        {needsName && (
          <Input
            label={fill(t("page.fieldEditor.delTypeName"), { name: field.name })}
            value={typed}
            placeholder={field.name}
            autoComplete="off"
            spellCheck={false}
            disabled={loading}
            onChange={(e) => onTyped(e.target.value)}
          />
        )}
      </div>
    </InlineConfirm>
  );
}
