"use client";

// BatchCreateForm — multi-row create for any entity (ENG-BATCH #1 FE, ../specs/009-bulk-create/spec.md §8).
// One spreadsheet-style table, driven entirely by field metadata (GET /metadata/fields/:entity) —
// NO per-entity hard-coding, same "one renderer → every entity" rule as LayoutForm/LayoutList.
//
//   • Rows are local state (not react-hook-form — N rows × M fields makes per-row RHF instances
//     overkill); each row's value map has the SAME widget-native shape LayoutForm's create mode
//     seeds (lib/recordPayload's seedValues/buildPayload) so the per-field-type transform on submit
//     (currency → {amount,currencyCode}, number coercion, etc.) is the EXACT function createRecord
//     already uses — no drift, no reimplementation (B14).
//   • Cells reuse the same widgets LayoutForm renders (components/fields/widgets.tsx FieldInput via
//     lib/fieldSpec's metaToSpec) — one widget registry, not a bespoke bulk-only input set.
//   • Server-computed field types (formula/lookup/rollup/auto_number — lib/recordPayload's
//     READONLY_FIELD_TYPES, mirrors backend computedReadonlyCode) render as a disabled "Tự động"
//     cell and are excluded from the submit payload entirely — never sent, never guessed at.
//   • Client-side validation is intentionally light: only STRUCTURAL guards (≥1 row, ≤ MAX_ROWS)
//     block the submit button. Per-field correctness (required/type/pattern/unique/relation…) is
//     NOT re-implemented here — the server is the single source of truth and its response is what
//     drives the per-row highlight, exactly per ../specs/009-bulk-create/spec.md's two-phase contract.
//   • results[].index → row mapping: `records` is built with `rows.map(...)` (no filtering/
//     reordering), so `results[i].index === i` lines up 1:1 with `rows[i]` by construction.
//
// Chrome (Sidebar/Topbar) is provided by the (app) route-group's AppShell (E3 W3a) — this
// component renders no shell of its own, matching LayoutListScreen/ImportScreen/LayoutFormScreen.
//
// SLDS 2.264.1 (specs/038 T025): page-headers base · scoped-notifications (limits, errors, result) ·
// radio-button-group (send mode) · data-tables/inline-edit with every cell open for editing (no
// "batch create" blueprint — RESEARCH §5: compose inline-edit with the editors open) · docked-form-footer.

import { useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { clsx } from "clsx";
import { CheckCircle2, CircleX, Copy, FileText, Lock, Plus, Trash2, TriangleAlert } from "lucide-react";
import {
  ensureAuth,
  listFields,
  createRecordsBulk,
  ApiError,
  type FieldMeta,
  type BulkMode,
  type BulkResult,
  type BulkRowResult,
} from "@/lib/api";
import { buildPayload, seedValues, SYSTEM_FIELDS, READONLY_FIELD_TYPES } from "@/lib/recordPayload";
import { lockedRequiredNotice } from "@/lib/fieldAcl";
import { metaToSpec } from "@/lib/fieldSpec";
import { FieldHelp, FieldInput } from "@/components/fields/widgets";
import { clearedDependents } from "@/lib/dependentPicklist";
import { useEntityLabel } from "@/lib/metadata/hooks";
import { getHelp } from "@/lib/help-content";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { DockedFormFooter, ErrorPopover } from "@/components/slds/DockedFormFooter";
import { PageHeader } from "@/components/slds/PageHeader";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";

type LoadState = "loading" | "ok" | "error";

// BULK_MAX_ROWS default (../specs/009-bulk-create/spec.md §2, env BULK_MAX_ROWS on the server). The
// server is the real authority — if an admin lowers the env cap, a live 422 BULK_TOO_LARGE
// (details.max) still wins; this is just the client-side soft guard matching the documented
// default so most users never round-trip to discover the cap.
const MAX_ROWS = 100;

// Per-fieldType column width (px) — wide enough for the SLDS control's own chrome (icon + text,
// the currency addon, the counter's − / + buttons, the date + time pair) without wasting
// horizontal space on a 10-column table. Falls back to 180 for any type not listed.
const FIELD_COL_WIDTH: Record<string, number> = {
  text: 200, rich_text: 240, number: 150, currency: 180, boolean: 80,
  date: 170, datetime: 300, email: 220, picklist: 180, multi_select: 220,
  relation: 240, user: 220, polymorphic_relation: 320, rating: 180, composite: 320, file: 200, image: 200,
  formula: 150, lookup: 150, rollup: 150, auto_number: 150,
};
const colWidth = (f: FieldMeta) => FIELD_COL_WIDTH[f.fieldType] ?? 180;
// Fixed columns: errors + row number, per-row status (post-submit), duplicate/remove actions.
const ERR_COL = 60;
const STATUS_COL = 230;
const ACTIONS_COL = 80;

let _rowKeySeq = 0;
function newRowKey(): string {
  _rowKeySeq += 1;
  return `row-${_rowKeySeq}`;
}

interface RowState {
  key: string; // stable React key — independent of position so add/remove/duplicate never reshuffle
  values: Record<string, unknown>; // widget-native values keyed by field.name
}

export interface BatchCreateFormProps {
  entity: string;
  // Called after every submit with the server result. The form itself stays on-screen showing
  // the per-row outcome; when EVERY row succeeded it also resets the grid to fresh seed rows so
  // a second Enter cannot silently re-create the same batch (dual-review 2026-09-01 MEDIUM).
  onDone?: (result: BulkResult) => void;
  onCancel?: () => void;
}

export function BatchCreateForm({ entity, onDone, onCancel }: BatchCreateFormProps) {
  const entityLabel = useEntityLabel(entity);
  // F7 SPA-ify: client-side push, no full document reload (plain router — no dirty-form guard
  // is wired on the batch table today, so the guarded variant would be a no-op).
  const router = useRouter();
  const goList = () => router.push(`/${entity}`);
  const handleCancel = onCancel ?? goList;

  // queue #304: `/:entity/batch?pf_<field>=<value>` (the related list's "Thêm nhiều") starts every row
  // linked to the parent — same seed path as LayoutForm's ?pf_ on a NEW form. Read once.
  const [prefill] = useState<Record<string, unknown> | undefined>(() => {
    if (typeof window === "undefined") return undefined;
    const out: Record<string, unknown> = {};
    for (const [k, v] of new URLSearchParams(window.location.search)) {
      if (k.startsWith("pf_") && k.length > 3) out[k.slice(3)] = v;
    }
    return Object.keys(out).length ? out : undefined;
  });

  const [loadState, setLoadState] = useState<LoadState>("loading");
  const [loadErr, setLoadErr] = useState("");
  const [fields, setFields] = useState<FieldMeta[]>([]);
  const [rows, setRows] = useState<RowState[]>([]);
  const [mode, setMode] = useState<BulkMode>("atomic");
  const [submitting, setSubmitting] = useState(false);
  const [submitErr, setSubmitErr] = useState("");
  const [retryUnsafe, setRetryUnsafe] = useState(false);
  const [result, setResult] = useState<BulkResult | null>(null);
  const [capNotice, setCapNotice] = useState("");
  const [lockedNotice, setLockedNotice] = useState<string | null>(null);

  // ── Load field metadata; seed 2 starter rows ──
  useEffect(() => {
    let cancelled = false;
    (async () => {
      setLoadState("loading");
      setLoadErr("");
      try {
        await ensureAuth();
        // `readOnly` is the field-ACL lock for THIS caller's role (queue #12/#13). Dropped here,
        // at the single load site, so the header row, the grid template and buildPayload all agree:
        // drawing a live column for a locked field costs the user the whole create, because
        // CheckWriteFields refuses on the PRESENCE of the locked key, not on its value.
        const served = await listFields(entity);
        const flds = served.filter((f) => !SYSTEM_FIELDS.has(f.name) && !f.readOnly);
        if (cancelled) return;
        setFields(flds);
        // queue #146: a dropped locked field that is required and not engine-filled refuses EVERY
        // row (RECORD_REQUIRED_LOCKED) — warn before twenty rows are typed.
        setLockedNotice(lockedRequiredNotice(served));
        const editable = flds.filter((f) => !READONLY_FIELD_TYPES.has(f.fieldType));
        setRows([
          { key: newRowKey(), values: seedValues(editable, prefill, true) },
          { key: newRowKey(), values: seedValues(editable, prefill, true) },
        ]);
        setLoadState("ok");
      } catch (e) {
        if (cancelled) return;
        setLoadErr(e instanceof ApiError ? e.message : e instanceof Error ? e.message : t("page.recordForm.loadFallback"));
        setLoadState("error");
      }
    })();
    return () => { cancelled = true; };
  }, [entity, prefill]);

  const editableFields = useMemo(
    () => fields.filter((f) => !READONLY_FIELD_TYPES.has(f.fieldType)),
    [fields],
  );
  const requiredCount = useMemo(() => editableFields.filter((f) => f.isRequired).length, [editableFields]);
  const fieldsByName = useMemo(() => Object.fromEntries(fields.map((f) => [f.name, f])), [fields]);

  // Fixed-layout table: the columns keep their widths and the container scrolls sideways.
  const tableWidth = ERR_COL + STATUS_COL + ACTIONS_COL + fields.reduce((sum, f) => sum + colWidth(f), 0);

  // A prior result's row indices only stay meaningful while `rows` keeps the same order/length —
  // any structural mutation (add/remove/duplicate) invalidates it. Editing a cell's VALUE does
  // NOT clear it: the row is still "the one that needs fixing" until the next submit.
  const clearResult = () => { setResult(null); setSubmitErr(""); setRetryUnsafe(false); };

  const addRow = () => {
    if (rows.length >= MAX_ROWS) {
      setCapNotice(tf("page.batch.cap", { max: MAX_ROWS }));
      return;
    }
    setCapNotice("");
    clearResult();
    setRows((prev) => [...prev, { key: newRowKey(), values: seedValues(editableFields, prefill, true) }]);
  };

  const duplicateRow = (key: string) => {
    if (rows.length >= MAX_ROWS) {
      setCapNotice(tf("page.batch.cap", { max: MAX_ROWS }));
      return;
    }
    setCapNotice("");
    clearResult();
    setRows((prev) => {
      const idx = prev.findIndex((r) => r.key === key);
      if (idx === -1) return prev;
      const clone: RowState = { key: newRowKey(), values: { ...prev[idx].values } };
      return [...prev.slice(0, idx + 1), clone, ...prev.slice(idx + 1)];
    });
  };

  const removeRow = (key: string) => {
    if (rows.length <= 1) return; // always keep ≥1 row
    setCapNotice("");
    clearResult();
    setRows((prev) => prev.filter((r) => r.key !== key));
  };

  const setCell = (rowKey: string, field: string, value: unknown) => {
    setRows((prev) => prev.map((r) => {
      if (r.key !== rowKey) return r;
      const values = { ...r.values, [field]: value };
      // specs/052: same rule as the record form — a new parent blanks the children it no longer allows.
      return { ...r, values: { ...values, ...clearedDependents(fieldsByName, values, field) } };
    }));
  };

  // Convenience action shown after a mixed partial result: drop the rows the server already
  // created so a resubmit of the remaining (fixed) rows can't accidentally create duplicates.
  const removeCreatedRows = () => {
    if (!result) return;
    const createdIdx = new Set(result.results.filter((r) => r.status === "created").map((r) => r.index));
    if (createdIdx.size === 0) return;
    setRows((prev) => {
      const next = prev.filter((_, i) => !createdIdx.has(i));
      return next.length > 0 ? next : prev;
    });
    clearResult();
  };

  const handleSubmit = async () => {
    setSubmitting(true);
    setSubmitErr("");
    setRetryUnsafe(false);
    try {
      await ensureAuth();
      // Straight in-order map — no filtering/reordering — keeps records[i] ↔ rows[i] ↔
      // results[i].index aligned by construction (../specs/009-bulk-create/spec.md §2).
      const records = rows.map((r) => buildPayload(editableFields, r.values));
      const res = await createRecordsBulk(entity, records, mode);
      setResult(res);
      if (res.failed === 0) {
        // All rows landed: clear the grid back to seed rows. Keeping the filled rows + a live
        // submit button made double-submit create the whole batch twice on entities with no
        // unique field (dual-review 2026-09-01 MEDIUM). The success summary stays visible.
        setRows([
          { key: newRowKey(), values: seedValues(editableFields, prefill, true) },
          { key: newRowKey(), values: seedValues(editableFields, prefill, true) },
        ]);
      }
      onDone?.(res);
    } catch (e) {
      // Retry-safety (api-conventions.md §8 MED-1): mode=atomic is always safe to resubmit as-is
      // (clean rollback, 0 rows written). mode=partial is NOT safe when the failure is an INFRA
      // error (5xx / network reject) reaching this catch — earlier rows in the batch are each an
      // independently-committed transaction, so some may already exist. A genuine validation-time
      // 4xx (e.g. BULK_EMPTY/BULK_TOO_LARGE/BULK_INVALID_MODE) never wrote anything either way.
      if (mode === "partial") {
        const infra = !(e instanceof ApiError) || e.status >= 500;
        if (infra) setRetryUnsafe(true);
      }
      setSubmitErr(e instanceof ApiError ? e.message : e instanceof Error ? e.message : t("page.recordForm.errNetwork"));
    } finally {
      setSubmitting(false);
    }
  };

  const resultByIndex = useMemo(() => {
    const m = new Map<number, BulkRowResult>();
    if (result) for (const r of result.results) m.set(r.index, r);
    return m;
  }, [result]);

  const canRemoveCreated = !!result && result.mode === "partial" && result.created > 0 && result.failed > 0;

  const title = tf("page.batch.title", { entity: entityLabel });
  const header = (
    <PageHeader
      icon={{ glyph: FileText, name: "record", label: entityLabel }}
      title={title}
      // DP-1.6: no slug line (builder talk) — the counts the old footer carried.
      meta={[
        tf("page.batch.counts", { rows: rows.length, max: MAX_ROWS, fields: editableFields.length }),
        requiredCount > 0 && tf("page.recordForm.requiredCount", { n: requiredCount }),
      ]
        .filter(Boolean)
        .join(" · ")}
    />
  );

  if (loadState !== "ok") {
    return (
      <div className="slds-p-around_medium">
        {header}
        <div className="slds-m-top_medium">
          {loadState === "loading" ? (
            <div className="relative h-40" aria-busy>
              <Spinner container />
            </div>
          ) : (
            <ScopedNotification theme="error">
              <strong>{t("page.batch.loadFailed")}</strong> — {loadErr}
            </ScopedNotification>
          )}
        </div>
      </div>
    );
  }

  const modeHelp = getHelp("bulkCreateMode");
  return (
    // Fills <main>: the table scrolls inside (header row pinned), the docked footer is fixed to the
    // viewport bottom, so the column reserves its 3rem (slds-p-bottom_xx-large).
    <div className="slds-p-horizontal_medium slds-p-top_medium slds-p-bottom_xx-large flex min-h-0 flex-1 flex-col gap-3">
      {header}

      {lockedNotice && <ScopedNotification theme="warning">{lockedNotice}</ScopedNotification>}

      {/* Known limitations — honest, not hidden (relation forward-ref + server-computed types). */}
      <ScopedNotification>{t("page.batch.limits")}</ScopedNotification>

      <RadioButtonGroup
        label={t("page.batch.mode")}
        fieldLevelHelp={modeHelp && <FieldHelp text={modeHelp.short} label={modeHelp.term} />}
        options={[
          { value: "atomic", label: t("page.batch.modeAtomic") },
          { value: "partial", label: t("page.batch.modePartial") },
        ]}
        value={mode}
        onValueChange={(m) => {
          setMode(m as BulkMode);
          clearResult();
        }}
      />

      {capNotice && <ScopedNotification theme="warning">{capNotice}</ScopedNotification>}

      {/* Submit error (network / BULK_EMPTY / BULK_TOO_LARGE / infra 5xx) */}
      {submitErr && (
        <ScopedNotification theme="error">
          {submitErr}
          {retryUnsafe && (
            <>
              <br />
              {t("page.batch.retryUnsafe")}
            </>
          )}
        </ScopedNotification>
      )}

      {/* Result summary */}
      {result && (
        <ScopedNotification theme={result.failed > 0 ? "warning" : "success"}>
          {tf("page.batch.result", { created: result.created, failed: result.failed, skipped: result.skipped ?? 0 })}
          {canRemoveCreated && (
            <>
              {" "}
              <Button onClick={removeCreatedRows}>{t("page.batch.removeCreated")}</Button>
            </>
          )}
        </ScopedNotification>
      )}

      {/* data-tables/inline-edit with every cell already open for editing (RESEARCH §5 — no
          "batch create" blueprint). No role=grid: the cells hold live inputs, so the table keeps
          native table semantics instead of promising grid arrow-key navigation. Row actions sit
          next to the row number (as before the migration) so they stay in view on a wide table. */}
      <div className="slds-table_edit_container slds-is-relative min-h-0 overflow-auto">
        <table
          className="slds-table slds-table_bordered slds-table_edit slds-table_fixed-layout"
          aria-label={t("page.batch.tableLabel")}
          style={{ minWidth: tableWidth }}
        >
          <thead>
            <tr className="slds-line-height_reset">
              <th scope="col" className="slds-cell_action-mode sticky top-0 z-10" style={{ width: ERR_COL }}>
                <div className="slds-th__action">
                  <span className="slds-assistive-text">{t("page.batch.colErrors")}</span>
                </div>
              </th>
              <th scope="col" className="slds-cell_action-mode sticky top-0 z-10" style={{ width: ACTIONS_COL }}>
                <div className="slds-th__action">
                  <span className="slds-assistive-text">{t("page.batch.colActions")}</span>
                </div>
              </th>
              {fields.map((f) => {
                const readonly = READONLY_FIELD_TYPES.has(f.fieldType);
                const label = f.label ?? f.name;
                // A server-computed column says so in its title; its cells read "Tự động" + lock.
                const title = readonly ? `${label} (${t("page.batch.auto")})` : label;
                return (
                  <th key={f.name} scope="col" className="slds-cell_action-mode sticky top-0 z-10" style={{ width: colWidth(f) }}>
                    <div className="slds-th__action">
                      {f.isRequired && !readonly && (
                        <abbr className="slds-required" title={t("slds.formElement.required")}>
                          *
                        </abbr>
                      )}
                      <span className="slds-truncate" title={title}>
                        {label}
                      </span>
                    </div>
                  </th>
                );
              })}
              <th scope="col" className="slds-cell_action-mode sticky top-0 z-10" style={{ width: STATUS_COL }}>
                <div className="slds-th__action">
                  <span className="slds-truncate">{t("page.batch.colStatus")}</span>
                </div>
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row, i) => {
              const rr = resultByIndex.get(i);
              const n = i + 1;
              const failedMsg = rr?.status === "failed" ? rowMessage(rr) : undefined;
              return (
                <tr key={row.key} className="slds-hint-parent">
                  <td className="slds-cell-edit slds-cell-error">
                    {/* inline-edit "Error - Row level on save": the reason at the row's start, where the
                        eye is — the status column sits past every field column on a wide table. */}
                    {failedMsg && (
                      <ErrorPopover label={tf("page.batch.rowError", { n })} className="slds-m-horizontal_xxx-small" portal>
                        <p>{failedMsg}</p>
                      </ErrorPopover>
                    )}
                    <span className="slds-row-number slds-text-body_small slds-text-color_weak slds-m-left_x-small" />
                  </td>
                  <td className="slds-cell-edit">
                    <span className="slds-grid">
                      <ButtonIcon icon={Copy} label={tf("page.batch.duplicate", { n })} onClick={() => duplicateRow(row.key)} />
                      <ButtonIcon
                        icon={Trash2}
                        label={tf("page.batch.remove", { n })}
                        onClick={() => removeRow(row.key)}
                        disabled={rows.length <= 1}
                      />
                    </span>
                  </td>
                  {fields.map((f) => {
                    const readonly = READONLY_FIELD_TYPES.has(f.fieldType);
                    // The failing cell carries the reason too (inline-edit "Cell edit — Error": the
                    // input is described by the message line under it).
                    const fieldErr = rr?.details?.field === f.name ? failedMsg : undefined;
                    return readonly ? (
                      <td key={f.name} className="slds-cell-edit" aria-readonly="true">
                        <span className="slds-grid slds-grid_align-spread slds-grid_vertical-align-center" title={t("page.batch.autoTitle")}>
                          <span className="slds-truncate slds-text-color_weak">{t("page.batch.autoCell")}</span>
                          <Lock aria-hidden="true" className="slds-icon slds-icon_x-small slds-icon-text-light nx-lucide" />
                        </span>
                      </td>
                    ) : (
                      <td key={f.name} className={clsx("slds-cell-edit", fieldErr && "slds-has-error")}>
                        <FieldInput
                          field={metaToSpec(f)}
                          value={row.values[f.name]}
                          onChange={(v) => setCell(row.key, f.name, v)}
                          siblings={{ values: row.values, fieldsByName }}
                          error={fieldErr}
                          a11yLabel={tf("page.batch.cellLabel", { field: f.label ?? f.name, n })}
                        />
                      </td>
                    );
                  })}
                  <td className="slds-cell-edit">
                    <StatusCell row={rr} />
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      <div>
        <Button iconLeft={Plus} onClick={addRow} disabled={rows.length >= MAX_ROWS}>
          {t("page.batch.addRow")}
        </Button>
      </div>

      <DockedFormFooter
        onCancel={handleCancel}
        onSave={handleSubmit}
        saving={submitting}
        cancelLabel={t("page.batch.backToList")}
        saveLabel={submitting ? t("page.batch.submitting") : tf("page.batch.submit", { n: rows.length })}
      />
    </div>
  );
}

// The human reason for a failed/skipped row (mapped error code, else the server's text).
function rowMessage(row: BulkRowResult) {
  return row.code ? errorMessage(new ApiError(422, row.code, row.error ?? "")) : (row.error ?? t("page.batch.unknownError"));
}

function StatusCell({ row }: { row?: BulkRowResult }) {
  if (!row) return <span className="slds-text-color_weak">—</span>;
  if (row.status === "created") {
    return (
      <Badge theme="success" iconLeft={CheckCircle2}>
        {t("page.batch.created")}
      </Badge>
    );
  }
  const msg = rowMessage(row);
  const skipped = row.status === "skipped";
  return (
    <span className="slds-grid slds-grid_vertical-align-center" title={msg}>
      <span className={`slds-icon_container ${skipped ? "slds-icon-utility-warning" : "slds-icon-utility-error"} slds-m-right_xx-small`}>
        {skipped ? (
          <TriangleAlert aria-hidden="true" className="slds-icon slds-icon_x-small slds-icon-text-warning nx-lucide" />
        ) : (
          <CircleX aria-hidden="true" className="slds-icon slds-icon_x-small slds-icon-text-error nx-lucide" />
        )}
      </span>
      <span className={clsx("slds-truncate", !skipped && "slds-text-color_error")}>{msg}</span>
    </span>
  );
}
