"use client";

// LayoutForm — the dynamic create/edit form (doc ../specs/028-layout-l2-renderer/spec.md §4.1, EPIC 2.5 T5).
//
// Renders `layout.views.edit.sections[]` through SectionGrid → CellRenderer → FieldInput, with NO
// per-entity hard-coding. State is react-hook-form (Founder D3) validated by a zod schema generated
// dynamically from the field metadata (buildLayoutSchema, T4). One renderer → every entity.
//
//   • create mode (no recordId): empty defaults, POST /:entity.
//   • edit mode   (recordId):    prefill from getRecord, PUT /:entity/:id with `version` (RM-24
//                                optimistic lock) → 409 surfaces a conflict banner, never a silent
//                                overwrite.
//   • G1 dirty-state guard: warns before leaving the page while the form has unsaved edits
//     (formState.isDirty + beforeunload).
//   • 422 mapping (lib/saveErrors mapSaveError): RECORD_REQUIRED_MISSING carries details.field →
//     setError on that field; a before-save rule veto (specs/048) puts each violation under its
//     input, the rest in the banner; every other value error (type/email/range/too_long/pattern/
//     picklist) → form-level banner.
//   • States: loading spinner · saving (disabled) · field errors · 409 conflict · network.
//
// SLDS 2.264.1 (specs/038 T024): the RecordFormPage template — page-headers base, one
// expandable-section per layout section with the form-element/record-detail "edit-stacked" grid
// (SectionFields), scoped-notifications for 409/form-level errors, docked-form-footer (Huỷ ·
// Lưu & tạo mới · Lưu). Quick-create (`modal`) = the template's modal variant (contracts/shell.md):
// modals/base with Huỷ / Tạo in slds-modal__footer, one dialog from the spinner to the form.
// A failed submit (zod or a 422 on a rendered field) names the fields to fix in the form-level
// notification and moves focus to the first invalid control: the fields are bridged with setValue
// (no registered ref), so react-hook-form cannot focus them itself.

import { Fragment, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useForm } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import Link from "next/link";
import { studioHref } from "@/lib/studioRoutes";
import { activeProjectSlug } from "@/lib/studioContext";
import { Boxes, FileText } from "lucide-react";
import {
  ensureAuth,
  getLayout,
  listFields,
  getRecord,
  createRecord,
  updateRecord,
  ApiError,
  cellField,
  isSpacerCell,
  type EntityLayout,
  type FieldMeta,
  type ApiRecord,
  type LayoutSection,
} from "@/lib/api";
import { buildLayoutSchema } from "@/lib/layoutSchema";
import { mapSaveError } from "@/lib/saveErrors";
import { buildPayload, seedValues, cloneRecordData, SYSTEM_FIELDS } from "@/lib/recordPayload";
import { createImage, fieldVisible, layoutConditions, visibleSections } from "@/lib/visibleWhen";
import { lockedRequiredNotice } from "@/lib/fieldAcl";
import { recordLabel } from "@/lib/recordLabel";
import { useEntityLabel } from "@/lib/metadata/hooks";
import { SectionFields, sectionFieldNames } from "@/components/layout/SectionGrid";
import { useNavigationBlocker } from "@/lib/navigation-blocker";
import { useAuth } from "@/lib/auth/AuthProvider";
import { getHelp } from "@/lib/help-content";
import { t, tf } from "@/lib/i18n";
import { usePreviewReadOnly } from "@/lib/previewMode";
import { Button, buttonClassName } from "@/components/slds/Button";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { RecordFormPage, type RecordFormNotificationTone, type RecordFormSection } from "@/components/slds/templates/RecordFormPage";
import { FieldHelp } from "@/components/fields/widgets";

type LoadState = "loading" | "ok" | "error";

export interface LayoutFormProps {
  entity: string;
  // Present → edit mode (prefill + PUT with version). Absent → create mode (POST).
  recordId?: string;
  // Called with the created/updated record id after a successful save.
  onSaved?: (id: string) => void;
  // Called when the user cancels / closes without saving.
  onCancel?: () => void;
  // Quick-create (QuickCreateModal): the form IS the dialog (RecordFormPage modal variant). The host
  // mounts it to open and unmounts it to close; Esc / X / Huỷ go through onCancel (dirty guard).
  modal?: boolean;
  // Programmatic prefill for quick-create (STUDIO-DISCOVERABILITY 1.4c) — same semantics as the
  // ?pf_<field>= URL params (seeded into a NEW form, user reviews before saving), for callers that
  // have no URL context (e.g. the related-list quick-create seeds the parent FK). Wins over URL
  // params when non-empty. Read once on mount — remount to change it (QuickCreateModal unmounts
  // when closed, so each open picks up the current value).
  prefill?: Record<string, unknown>;
}

// Collect every distinct field name the edit layout references (string cells + object cells,
// minus spacers), in layout order. The zod schema + seed are scoped to exactly these.
function layoutFieldNames(sections: LayoutSection[] | undefined): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const s of sections ?? []) {
    for (const cell of s.fields) {
      if (isSpacerCell(cell)) continue;
      const fc = cellField(cell);
      if (fc && !seen.has(fc.field)) {
        seen.add(fc.field);
        out.push(fc.field);
      }
    }
  }
  return out;
}

export function LayoutForm({ entity, recordId, onSaved, onCancel, modal = false, prefill }: LayoutFormProps) {
  const isEdit = recordId !== undefined;
  const entityLabel = useEntityLabel(entity);
  // DP-1.2: zero-field empty state routes admins into the field builder, plain users get honest copy.
  const { user } = useAuth();
  const isAdminUser = !!user?.isAdmin;
  const canOpenStudio = isAdminUser && !!user?.isPlatformOperator; // only the builder sees the way into Studio
  // specs/063 S7-3: a draft-preview tab shows the form but cannot save it (the server answers 403 PREVIEW_READ_ONLY).
  const previewReadOnly = usePreviewReadOnly();

  // §7.1 clone: `/:entity/new?cloneFrom=<id>` prefills a NEW form from an existing record (id/system/
  // computed-readonly fields stripped, see cloneRecordData). Read once from the URL — only in
  // full-page create mode (never edit, never the quick-create modal, whose URL belongs to the page
  // under it). SSR-safe via the window guard in the lazy initializer.
  const [cloneFromId, setCloneFromId] = useState<string | null>(() => {
    if (isEdit || modal || typeof window === "undefined") return null;
    return new URLSearchParams(window.location.search).get("cloneFrom");
  });

  // ENG-RELVIEW "+ Tạo": `/:entity/new?pf_<field>=<value>` prefills specific fields on a NEW
  // form — e.g. a related list seeds the parent FK (`?pf_customer=<parentId>`) so the child
  // is born already linked. Values ride the SAME seed path as clone (seedValues/reverseValue),
  // are scoped to fields the edit layout actually renders, and the user reviews before saving.
  // Deliberately NOT stripped from the URL: "Lưu & tạo mới" re-prefills the FK, which is the
  // desired flow when adding several children to one parent. Read once (lazy initializer).
  const [prefillData] = useState<Record<string, unknown> | null>(() => {
    // Prop prefill (quick-create) takes the same seed path as ?pf_ and wins when present.
    if (!isEdit && prefill && Object.keys(prefill).length > 0) return prefill;
    if (isEdit || modal || typeof window === "undefined") return null;
    const out: Record<string, unknown> = {};
    for (const [k, v] of new URLSearchParams(window.location.search)) {
      if (k.startsWith("pf_") && k.length > 3) out[k.slice(3)] = v;
    }
    return Object.keys(out).length ? out : null;
  });

  const [loadState, setLoadState] = useState<LoadState>("loading");
  const [loadErr, setLoadErr] = useState("");
  const [layout, setLayout] = useState<EntityLayout | null>(null);
  const [allFields, setAllFields] = useState<FieldMeta[]>([]);
  const [record, setRecord] = useState<ApiRecord | null>(null);

  // Submit-time state.
  const [saving, setSaving] = useState(false);
  const [formErr, setFormErr] = useState<string[]>([]);       // form-level banner lines (non-field 422 / network)
  const [conflict, setConflict] = useState<number | null>(null); // 409: server's currentVersion
  // "Lưu & tạo mới" (§7.1, create-only): which button fired the submit. A ref (not state) so the
  // click sets it synchronously before RHF's handleSubmit runs the async validator.
  const submitModeRef = useRef<"save" | "saveNew">("save");
  // Transient confirmation after a "Lưu & tạo mới" (record created, form reset for the next entry).
  const [savedNote, setSavedNote] = useState("");
  // Focus the first invalid control after a failed submit: bumped by onInvalid and by a 422 that
  // lands on a field; the effect below runs once the errors are on screen.
  const formRef = useRef<HTMLFormElement>(null);
  const [focusErrorTick, setFocusErrorTick] = useState(0);
  const focusedTick = useRef(0);

  // The set of fields the form actually renders (referenced by the edit layout). Built once
  // layout + fields land; seeds RHF state so every rendered cell has a value to show.
  // Contract §2.3 floor-rule: a cell may TIGHTEN `required` (never loosen). Overlay cell.required
  // onto the field meta here so the zod schema + "N bắt buộc" footer enforce what the * shows —
  // previously the cell toggle only painted an asterisk (decoration without enforcement).
  const renderedFields = useMemo<FieldMeta[]>(() => {
    if (!layout) return [];
    const sections = layout.views.edit?.sections;
    const names = layoutFieldNames(sections);
    const tightened = new Set<string>();
    for (const s of sections ?? []) {
      for (const cell of s.fields) {
        const fc = cellField(cell);
        if (fc?.required) tightened.add(fc.field);
      }
    }
    const byName = new Map(allFields.map((f) => [f.name, f]));
    return names
      .filter((n) => !SYSTEM_FIELDS.has(n))
      .map((n) => byName.get(n))
      .filter((f): f is FieldMeta => !!f)
      .map((f) => (tightened.has(f.name) && !f.isRequired ? { ...f, isRequired: true } : f));
  }, [layout, allFields]);

  // The fields the user may actually WRITE — drives the zod schema and the submit payload.
  // field-lock-signal: a field the caller's role may view but not edit (`readOnly`, field-ACL
  // can_edit=0) is rendered as text by CellRenderer and must NOT be in either: validating it is
  // meaningless (no input to fix), and submitting it is exactly the write the server refuses —
  // buildPayload's update mode would resend it (or null it) and every save came back
  // 403 FIELD_FORBIDDEN. The zod object strips keys outside the schema, so the locked value
  // stays in RHF state for display and never reaches the payload.
  const formFields = useMemo<FieldMeta[]>(
    () => renderedFields.filter((f) => !f.readOnly),
    [renderedFields],
  );

  // The fields as the form DRAWS them, for placing a refused save (mapSaveError): a cell the layout
  // marks read-only (LayoutBuilder toggle) is drawn as text exactly like a field-ACL lock
  // (mergeFieldSpec ORs the two), so it has no input to carry an error and its sentence belongs in
  // the notification. Placement only — the schema and payload keep formFields as they are.
  const drawnFields = useMemo<FieldMeta[]>(() => {
    const readonlyCells = new Set<string>();
    for (const s of layout?.views.edit?.sections ?? []) {
      for (const cell of s.fields) {
        const fc = cellField(cell);
        if (fc?.readonly) readonlyCells.add(fc.field);
      }
    }
    return renderedFields.map((f) => (readonlyCells.has(f.name) && !f.readOnly ? { ...f, readOnly: true } : f));
  }, [layout, renderedFields]);

  const fieldsByName = useMemo(
    () => Object.fromEntries(allFields.map((f) => [f.name, f])),
    [allFields],
  );

  // specs/054 FR-7 (#258): conditional cells. `conds` = target → visibleWhen across the edit view;
  // the JUDGE is the image, never the raw widgets: create = the POST body as buildPayload sends it
  // + the fields' defaults (what the server stores); edit = the record values on the form.
  // `visibleOf` is the ONE filter feeding the schema, the payload and the drawn sections. A hidden
  // field's draft stays in RHF state (hide → show brings back what was typed, Q1) but is neither
  // validated nor sent — and edit's buildPayload must never see it (it nulls empty optionals).
  const conds = useMemo(() => layoutConditions(layout?.views.edit?.sections ?? []), [layout]);
  const imageOf = (vals: Record<string, unknown>): Record<string, unknown> =>
    conds.size === 0
      ? vals
      : isEdit
        ? buildPayload(renderedFields, vals, "update")
        : createImage(renderedFields, buildPayload(formFields, vals, "create"));
  const visibleOf = (vals: Record<string, unknown>) => {
    if (conds.size === 0) return { image: vals, fields: formFields };
    const image = imageOf(vals);
    return { image, fields: formFields.filter((f) => fieldVisible(f.name, conds, image)) };
  };

  const {
    handleSubmit,
    reset,
    setError,
    setValue,
    watch,
    getValues,
    formState: { errors, isDirty },
  } = useForm<Record<string, unknown>>({
    // Schema built per validation from the fields visible on THESE values (a hidden field is not
    // validated); with no conditions that is just formFields. See visibleOf above.
    resolver: (vals, ctx, opts) => zodResolver(buildLayoutSchema(visibleOf(vals).fields))(vals, ctx, opts),
    // Validate on submit, then keep re-validating touched fields (reward-early / punish-late).
    mode: "onSubmit",
    reValidateMode: "onChange",
    defaultValues: {},
  });

  // ── Load layout + fields + (edit) record in parallel; seed the form ──
  useEffect(() => {
    let cancelled = false;
    setLoadState("loading");
    setLoadErr("");
    setLayout(null);
    setAllFields([]);
    setRecord(null);
    setFormErr([]);
    setConflict(null);

    (async () => {
      try {
        await ensureAuth();
        // Fetch the edit record (edit mode) OR the clone source (create + ?cloneFrom). Both use
        // getRecord; which one is decided below when seeding.
        const fetchId = isEdit && recordId ? recordId : cloneFromId;
        const [lay, flds, rec] = await Promise.all([
          getLayout(entity),
          listFields(entity),
          fetchId ? getRecord(entity, fetchId) : Promise.resolve(null),
        ]);
        if (cancelled) return;
        if (!lay) throw new ApiError(404, "LAYOUT_NOT_FOUND", t("page.recordForm.layoutMissing"));
        setLayout(lay);
        setAllFields(flds);
        // Clone mode: `rec` is the SOURCE record, not one we're editing — stay in create mode
        // (record=null → POST). Edit mode: `rec` IS the record being edited.
        setRecord(isEdit ? rec : null);

        // Seed defaults from the record (edit), the stripped clone source (create + cloneFrom), or
        // empty defaults (plain create). Scope to the fields the edit layout references so RHF state
        // matches the rendered inputs.
        const names = layoutFieldNames(lay.views.edit?.sections);
        const byName = new Map(flds.map((f) => [f.name, f]));
        const scoped = names
          .filter((n) => !SYSTEM_FIELDS.has(n))
          .map((n) => byName.get(n))
          .filter((f): f is FieldMeta => !!f);
        let seedData = isEdit
          ? rec?.data
          : rec
            // create + clone: strip id/system/computed; explicit ?pf_ prefill wins per-key.
            ? { ...cloneRecordData(scoped, rec.data), ...prefillData }
            // plain create: ?pf_ prefill (ENG-RELVIEW "+ Tạo") or empty defaults.
            : prefillData ?? undefined;
        // field-lock-signal, CREATE only: a clone/prefill value for a field this caller may not
        // edit (`readOnly`) is never submitted (the create guard refuses any locked key), so
        // showing it as read-only text would promise a value the record will not have. Seed the
        // cell empty ("—") so the form tells the truth. Edit mode keeps the stored value: that IS
        // the record's value, and it is displayed, never resent.
        if (!isEdit && seedData) {
          seedData = Object.fromEntries(
            Object.entries(seedData).filter(([k]) => !byName.get(k)?.readOnly),
          );
        }
        reset(seedValues(scoped, seedData, !isEdit));
        setLoadState("ok");
      } catch (e) {
        if (cancelled) return;
        setLoadErr(e instanceof ApiError ? e.message : e instanceof Error ? e.message : t("page.recordForm.loadFallback"));
        setLoadState("error");
      }
    })();
    return () => { cancelled = true; };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [entity, recordId]);

  // ── G1 dirty-state guard ──
  // Two layers, one source of truth (isDirty && !saving):
  //   • beforeunload  → native prompt for hard reload / close tab / external URL (kept as-is).
  //   • NavigationBlocker → in-app confirm for client-side nav: <a> clicks (sidebar/breadcrumb/
  //     profile, caught by the provider's capture-phase listener) and the guarded cancel/back below.
  const { setBlocked, confirmLeave } = useNavigationBlocker();

  const dirtyRef = useRef(false);
  dirtyRef.current = isDirty && !saving;

  // Arm/disarm the global blocker whenever the dirty/saving state changes. A successful submit
  // calls reset() → isDirty=false → this fires setBlocked(false), so post-save nav never nags.
  useEffect(() => {
    setBlocked(isDirty && !saving);
    return () => setBlocked(false); // unmount (route change completed) → always disarm.
  }, [isDirty, saving, setBlocked]);

  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return;
      e.preventDefault();
      e.returnValue = ""; // required for Chrome to show the native prompt
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, []);

  // Auto-dismiss the "đã tạo · tiếp tục" confirmation after a few seconds (subtle, MOTION 3).
  useEffect(() => {
    if (!savedNote) return;
    const t = setTimeout(() => setSavedNote(""), 3500);
    return () => clearTimeout(t);
  }, [savedNote]);

  // Cancel/close button (Huỷ / X) → same in-app confirm as link navigation (shared copy via blocker).
  const guardedCancel = () => {
    if (!confirmLeave()) return; // confirmLeave returns true when clean or user agrees to leave
    onCancel?.();
  };

  // Watch every form value so SectionGrid renders controlled inputs (see the bridge note below).
  const values = watch();
  const { image: visImage, fields: visibleFields } = visibleOf(values);
  const visibleNames = new Set(visibleFields.map((f) => f.name));

  // Map a RHF error object to a plain message string for SectionGrid. Rebuilt on every render, NOT
  // memoised on `errors`: react-hook-form mutates that object in place (setError after a server
  // refusal), so a memo keyed on its identity kept the empty list it computed while the request was
  // in flight — server errors never turned an input red (found live, specs/048 FE-1 acceptance).
  const errorMessages: Record<string, string | undefined> = {};
  for (const [k, v] of Object.entries(errors)) {
    if (!visibleNames.has(k) && formFields.some((f) => f.name === k)) continue; // stale error of a now-hidden field
    const msg = (v as { message?: string } | undefined)?.message;
    if (msg) errorMessages[k] = msg;
  }

  const errorCount = Object.keys(errorMessages).length;
  useEffect(() => {
    if (focusErrorTick === focusedTick.current || errorCount === 0) return;
    focusedTick.current = focusErrorTick;
    // First error in layout order; its invalid input/combobox, else (rating stars) its first button.
    const box = formRef.current?.querySelector(".slds-has-error");
    const target =
      box?.querySelector<HTMLElement>('[aria-invalid="true"]') ??
      box?.querySelector<HTMLElement>(".slds-form-element__control button:not(:disabled)");
    target?.focus();
  }, [focusErrorTick, errorCount]);
  const focusFirstError = () => setFocusErrorTick((n) => n + 1);

  // `values` (watch(), above) is the live values object; with setValue it bridges RHF ↔ the
  // SectionGrid value/onChange API.

  // Drop ?cloneFrom from the URL (after the first prefill / after "Lưu & tạo mới") so the blank
  // follow-up form — and any manual reload — is genuinely blank, not a re-clone.
  const stripCloneFromUrl = () => {
    if (typeof window === "undefined") return;
    const url = new URL(window.location.href);
    if (url.searchParams.has("cloneFrom")) {
      url.searchParams.delete("cloneFrom");
      window.history.replaceState({}, "", url.pathname + url.search);
    }
  };

  const onValid = async (raw: Record<string, unknown>) => {
    // Visibility is judged on the FULL RHF values (same image as render + resolver), never on `raw`:
    // zod strips keys outside the schema, so a read-only controller would vanish from `raw`.
    const judged = visibleOf(getValues()).fields;
    setSaving(true);
    setFormErr([]);
    setConflict(null);
    setSavedNote("");
    try {
      // "update" makes an emptied optional field travel as an explicit null. PUT is a PARTIAL
      // update, so the previous behaviour (drop the key) meant a user could change a value but
      // never REMOVE one — clearing a field and saving looked successful and changed nothing.
      const payload = buildPayload(judged, raw, isEdit && record ? "update" : "create");
      if (isEdit && record) {
        await updateRecord(entity, record.id, payload, record.version);
        onSaved?.(record.id);
      } else {
        const created = await createRecord(entity, payload);
        if (submitModeRef.current === "saveNew") {
          // §7.1 "Lưu & tạo mới": stay on the form for rapid entry — reset to a BLANK new form for
          // the same fields, confirm briefly. Drop any clone context (URL + state) so the follow-up
          // form is a genuine blank "Tạo" form, not a lingering "Nhân bản" (header + banner clear too).
          stripCloneFromUrl();
          setCloneFromId(null);
          reset(seedValues(renderedFields, undefined, !isEdit));
          setSavedNote(tf("page.recordForm.savedNext", { entity: entityLabel }));
        } else {
          onSaved?.(created.id);
        }
      }
    } catch (e) {
      // 409 optimistic-lock conflict → dedicated banner (reload to merge).
      if (e instanceof ApiError && (e.status === 409 || e.code === "RECORD_VERSION_CONFLICT")) {
        setConflict(typeof e.details.currentVersion === "number" ? e.details.currentVersion : null);
        return;
      }
      // Every other refusal goes through the shared mapper (lib/saveErrors, specs/048): an error
      // lands under an input only if that input is RENDERED and writable here — setError() on an
      // input that doesn't exist stopped the spinner with nothing on screen — and the rest (a
      // before-save rule's sentence, a field the layout left out, a field drawn read-only by the
      // role or by the layout, 503 "busy") goes to the form-level notification. A veto may name
      // several fields: each gets its own line.
      // A hidden field has no input either: an error naming one (FE/BE disagreement) goes to the banner.
      const hiddenNow = new Set(formFields.filter((f) => !judged.includes(f)).map((f) => f.name));
      const { fieldErrors, banner } = mapSaveError(e, drawnFields.filter((f) => !hiddenNow.has(f.name)), allFields);
      for (const [name, message] of Object.entries(fieldErrors)) setError(name, { type: "server", message });
      if (Object.keys(fieldErrors).length > 0) focusFirstError();
      setFormErr(banner);
    } finally {
      setSaving(false);
    }
  };

  // Reload the latest server record into the form, discarding local edits (used by the 409 banner).
  const reloadFromServer = async () => {
    if (!isEdit || !recordId) return;
    setConflict(null);
    setSaving(true);
    try {
      await ensureAuth();
      const fresh = await getRecord(entity, recordId);
      setRecord(fresh);
      reset(seedValues(renderedFields, fresh.data)); // every rendered cell, locked ones included
    } catch (e) {
      setFormErr([e instanceof ApiError ? e.message : t("page.recordForm.errReload")]);
    } finally {
      setSaving(false);
    }
  };

  const headerTitle = isEdit
    ? record ? tf("page.recordForm.titleEdit", { record: recordLabel(record, allFields) }) : t("page.recordForm.titleEditLoading")
    : cloneFromId ? tf("page.recordForm.titleClone", { entity: entityLabel }) : tf("page.recordForm.titleCreate", { entity: entityLabel });

  const sections = visibleSections(layout?.views.edit?.sections ?? [], visImage);
  const guardedBack = onCancel ? guardedCancel : undefined;

  // Modal: the dialog is open while mounted (the host unmounts it to close), so Radix's own close
  // request is ignored and Esc / X / Huỷ all reach onCancel = the dirty-state guard.
  const variant = modal ? ({ variant: "modal", open: true, onOpenChange: () => {} } as const) : ({ variant: "page" } as const);

  // Non-form states. Modal: the same RecordFormPage element as the loaded form (`status` in place of
  // the form), so React keeps ONE dialog from the spinner to the form: one focus trap, and focus
  // goes back to the opener on close.
  const shell = (body: ReactNode) =>
    modal ? (
      <RecordFormPage {...variant} title={headerTitle} sections={[]} status={body} onSubmit={(e) => e.preventDefault()} onCancel={guardedCancel} />
    ) : (
      <div className="slds-p-around_medium mx-auto w-full max-w-[1080px]">{body}</div>
    );

  if (loadState === "loading") {
    return shell(
      <div className="relative h-40" aria-busy>
        <Spinner container label={t("page.recordForm.loading")} />
      </div>,
    );
  }

  if (loadState === "error") {
    return shell(
      <ScopedNotification theme="error">
        <strong>{t("page.recordForm.loadFailed")}</strong> — {loadErr}
      </ScopedNotification>,
    );
  }

  // DP-1.2: entity has NO user-editable fields yet (fresh from Studio) — a form here would
  // render engineer noise ("Thiếu trường id") with a live-but-useless create button. Show an
  // honest next-step instead; admins get a deep-link into the field builder.
  if (renderedFields.length === 0) {
    return shell(
      <div className="slds-card slds-p-around_large">
        <EmptyState
          icon={Boxes}
          title={tf("page.recordForm.noFieldsTitle", { entity: entityLabel })}
          description={isAdminUser ? t("page.recordForm.noFieldsAdmin") : t("page.recordForm.noFieldsUser")}
          action={
            <div className="slds-m-top_medium flex flex-wrap justify-center gap-2">
              {canOpenStudio && (
                <Link href={studioHref(activeProjectSlug(), "dung/du-lieu/:bang", { bang: entity })} className={buttonClassName("brand")}>
                  {t("page.recordForm.noFieldsAdminCta")}
                </Link>
              )}
              {/* The modal's footer already has Huỷ. */}
              {guardedBack && !modal && <Button onClick={guardedBack}>{t("page.recordForm.back")}</Button>}
            </div>
          }
        />
      </div>,
    );
  }

  // Form-level message (one scoped notification, most urgent first): 409 conflict (reload to merge,
  // never a silent overwrite) · non-field 422 / network · "Lưu & tạo mới" confirmation · clone context.
  let notification: { tone: RecordFormNotificationTone; body: ReactNode } | undefined;
  // queue #146: a required field this caller may see but not write, that the engine does not fill,
  // refuses every create (RECORD_REQUIRED_LOCKED) — say it before the user types. Read from ALL
  // served fields, not the rendered ones: a layout that omits the field strands the create too.
  const lockedNotice = isEdit ? null : lockedRequiredNotice(allFields);
  if (conflict !== null) {
    const help = getHelp("recordConflict409");
    notification = {
      tone: "warning",
      body: (
        <>
          <strong>{t("page.recordForm.conflictTitle")}</strong>
          {help && <FieldHelp text={help.short} label={help.term} />}{" "}
          {tf("page.recordForm.conflictBody", { n: conflict })}{" "}
          <Button variant="neutral" onClick={reloadFromServer} disabled={saving}>
            {t("page.recordForm.conflictReload")}
          </Button>
        </>
      ),
    };
  } else if (formErr.length > 0) {
    notification = {
      tone: "error",
      // One line per message; the template wraps the body in a <p>, so line breaks, not a list.
      body: formErr.map((m, i) => (
        <Fragment key={i}>
          {i > 0 && <br />}
          {m}
        </Fragment>
      )),
    };
  } else if (Object.keys(errorMessages).length > 0) {
    // After a failed submit (the errors only exist from then on): which fields, in layout order.
    const bad = renderedFields.filter((f) => errorMessages[f.name]);
    notification = {
      tone: "error",
      body: tf("page.recordForm.fixFields", { n: bad.length, fields: bad.map((f) => f.label ?? f.name).join(", ") }),
    };
  } else if (previewReadOnly) {
    notification = { tone: "info", body: t("draft.preview.writeLocked") };
  } else if (lockedNotice) {
    notification = { tone: "warning", body: lockedNotice };
  } else if (isEdit && record?.sharedWithMe) {
    // specs/051 FR-009: reached through a share only (e.g. a pasted /edit link) — the save will be
    // refused with 403 RECORD_SHARED_READ_ONLY, so say it before the user types.
    notification = { tone: "info", body: t("page.record.share.readOnly") };
  } else if (isEdit && record?.canEdit === false) {
    // #334: a pasted /edit link to a record outside the caller's edit scope — the save would be
    // refused 403 FORBIDDEN (#18), so Lưu is off (submitDisabled below) and this says why.
    notification = { tone: "info", body: t("page.record.editScope.readOnly") };
  } else if (savedNote) {
    notification = { tone: "success", body: savedNote };
  } else if (cloneFromId) {
    notification = { tone: "info", body: t("page.recordForm.cloneNotice") };
  }

  // SectionFields is bridged to react-hook-form: values from watch() (re-renders on every change),
  // changes via setValue (mark dirty + re-validate), errors mapped to messages.
  const bodyProps = {
    mode: "edit" as const,
    fieldsByName,
    values,
    onChange: (field: string, v: unknown) => setValue(field, v, { shouldDirty: true, shouldValidate: true }),
    errors: errorMessages,
  };
  const formSections: RecordFormSection[] =
    sections.length === 0
      ? [{ id: "empty", title: t("page.recordForm.sectionFallback"), body: <p className="slds-text-color_weak">{t("page.recordForm.noSections")}</p> }]
      : sections.map((s, si) => ({
          id: `${si}-${s.title ?? ""}`,
          title: s.title || t("page.recordForm.sectionFallback"),
          body: <SectionFields section={s} sectionIndex={si} {...bodyProps} />,
          // A field error inside latches the section open so the message stays visible.
          hasError: sectionFieldNames(s).some((n) => errorMessages[n]),
        }));

  // Header meta: field counts + unsaved marker; DP-1.6: the tech line (slug · version) is builder
  // talk — admins only.
  const requiredCount = visibleFields.filter((f) => f.isRequired).length;
  const meta = [
    isAdminUser && `${entity} · ${isEdit ? tf("page.recordForm.metaVersion", { n: record?.version ?? "—" }) : t("page.recordForm.metaCreate")}`,
    tf("page.recordForm.fieldCount", { n: renderedFields.length }),
    requiredCount > 0 && tf("page.recordForm.requiredCount", { n: requiredCount }),
    isDirty && t("page.recordForm.unsaved"),
  ]
    .filter(Boolean)
    .join(" · ");

  const page = (
    <RecordFormPage
      {...variant}
      formRef={formRef}
      icon={{ glyph: FileText, name: "record" }}
      objectLabel={entityLabel}
      title={headerTitle}
      meta={meta}
      sections={formSections}
      notification={notification}
      saving={saving || previewReadOnly}
      // #18: the server answers this save 403 (canEdit is false for a share-only record too).
      submitDisabled={isEdit && record?.canEdit === false}
      submitLabel={isEdit ? t("page.recordForm.saveEdit") : tf("page.recordForm.titleCreate", { entity: entityLabel })}
      onSubmit={(e) => {
        // Enter in a field and the footer's Lưu both land here — always the PRIMARY save.
        submitModeRef.current = "save";
        void handleSubmit(onValid, focusFirstError)(e);
      }}
      onCancel={guardedCancel}
      extraActions={
        // §7.1: "Lưu & tạo mới" — secondary action, CREATE mode only, full-page form only (the
        // quick-create modal stays lightweight [Huỷ][Tạo]). Saves, then resets to a blank new
        // form for rapid multi-entry. type="button" + a PROGRAMMATIC handleSubmit, so the implicit
        // Enter-key submit maps to the primary save, never this secondary path.
        !isEdit && !modal ? (
          <Button
            onClick={() => {
              submitModeRef.current = "saveNew";
              void handleSubmit(onValid, focusFirstError)();
            }}
            disabled={saving || previewReadOnly}
          >
            {t("page.recordForm.saveNew")}
          </Button>
        ) : undefined
      }
    />
  );
  return modal ? page : <div className="slds-p-around_medium mx-auto w-full max-w-[1080px]">{page}</div>;
}
