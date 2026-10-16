"use client";

// ImportScreen — CSV import wizard (EPIC 3 A8) on SLDS 2.264.1 (specs/038 T026).
// Design read: a form-style wizard for the customer face. Blueprints: page-headers/base (title)
// + progress-indicator/base (the 4 steps) inside one `cards` card + file-selector/base (upload,
// drag & drop) + combobox/base (column → field mapping, dedup field) + data-tables/base (preview,
// per-row errors) + expandable-section (per-row errors) + scoped-notifications (errors, outcome).
// 4-step flow:
//   Step 1: Upload CSV file (parse header row client-side).
//   Step 2: Map CSV columns → entity fields (auto-guess by name match) + optional dedup field
//           (BE skips duplicate rows instead of aborting the batch on a unique collision). A column
//           mapped to a relation gets "Khớp theo" — which field of the target its cells name
//           (specs/061 FR-012; default = the relation's name field, ids always work).
//   Step 3: Preview — the server runs the whole import as a dry run (specs/061 FR-010): the first
//           10 rows with every wrong cell marked, how many rows will go in, and every row error.
//   Step 4: Run import → result (total/inserted/failed, duplicates split out, per-row errors).
//
// Backend endpoint: POST /:entity/import (multipart/form-data).
// On partial success the result shows per-row errors in a collapsible table.
// Chrome (global header / navigation) is provided by the (app) route-group's AppShell.

import { useEffect, useRef, useState } from "react";
import clsx from "clsx";
import { ArrowLeft, ArrowRight, CircleAlert, Play, Upload } from "lucide-react";
import { ensureAuth, listFields, importRecords, ApiError, type FieldMeta, type ImportResult } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { useEntityLabel } from "@/lib/metadata/hooks";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { Checkbox } from "@/components/slds/Checkbox";
import { Combobox, type ComboboxOption } from "@/components/slds/Combobox";
import { ExpandableSection } from "@/components/slds/ExpandableSection";
import { FileSelector } from "@/components/slds/FileSelector";
import { Icon } from "@/components/slds/Icon";
import { PageHeader } from "@/components/slds/PageHeader";
import { ProgressIndicator } from "@/components/slds/ProgressIndicator";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";

type Step = "upload" | "map" | "preview" | "result";
type RowError = NonNullable<ImportResult["errors"]>[number];

const STEPS: { id: Step; label: I18nKey; title: I18nKey }[] = [
  { id: "upload", label: "page.import.step.upload", title: "page.import.upload.title" },
  { id: "map", label: "page.import.step.map", title: "page.import.map.title" },
  { id: "preview", label: "page.import.step.preview", title: "page.import.preview.title" },
  { id: "result", label: "page.import.step.result", title: "page.import.result.title" },
];

const PREVIEW_ROWS = 10;

// onDone/onCancel (specs/078 T085): the Studio work surface passes its own router push; the customer
// face omits them and keeps the full-document hop to the list.
export function ImportScreen({ entity, onDone, onCancel }: { entity: string; onDone?: () => void; onCancel?: () => void }) {
  const goList = () => { window.location.href = `/${entity}`; };

  return <ImportWizard entity={entity} onDone={onDone ?? goList} onCancel={onCancel ?? onDone ?? goList} />;
}

function ImportWizard({ entity, onDone, onCancel }: { entity: string; onDone: () => void; onCancel: () => void }) {
  const entityLabel = useEntityLabel(entity);
  const [step, setStep] = useState<Step>("upload");
  const [fields, setFields] = useState<FieldMeta[]>([]);
  const [file, setFile] = useState<File | null>(null);
  const [fileErr, setFileErr] = useState("");
  const [csvHeaders, setCsvHeaders] = useState<string[]>([]);
  const [csvRows, setCsvRows] = useState<string[][]>([]); // first 20 rows for preview
  const [mapping, setMapping] = useState<Record<string, string>>({}); // csvCol → fieldName
  const [dedupField, setDedupField] = useState(""); // "" = no dedup (param omitted)
  const [runAutomations, setRunAutomations] = useState(false); // opt-in: rules/approvals for imported rows
  // "Khớp theo" per relation field ("" = the server's default: the relation's name field).
  const [matchBy, setMatchBy] = useState<Record<string, string>>({});
  // Fields of each relation target, loaded when a column is first mapped to that relation.
  const [targetFields, setTargetFields] = useState<Record<string, FieldMeta[]>>({});
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<ImportResult | null>(null);
  // The preview's dry run (specs/061): null while it runs or when it could not run.
  const [check, setCheck] = useState<ImportResult | null>(null);
  const [checking, setChecking] = useState(false);
  const checkSeq = useRef(0); // a late answer for an older mapping must not overwrite a newer one
  const [errMsg, setErrMsg] = useState("");

  // Load entity fields on mount.
  useEffect(() => {
    (async () => {
      try {
        await ensureAuth();
        const flds = await listFields(entity);
        // queue #144: a field locked for the caller (readOnly) is not a mapping target — mapped, it
        // fails the WHOLE import FIELD_FORBIDDEN (as in "Tạo nhiều dòng", #13).
        setFields(flds.filter((f) => !SYSTEM_FIELDS.includes(f.name) && !f.readOnly));
      } catch (e) {
        setErrMsg(e instanceof ApiError ? e.message : t("page.import.loadError"));
      }
    })();
  }, [entity]);

  // Relation targets of the mapped columns — load each one's fields once for "Khớp theo". A target
  // the caller may not read yields no choice: the default (name field, else ids) still applies.
  const mappedTargets = Array.from(new Set(
    Object.values(mapping).map((name) => relationTarget(fields.find((f) => f.name === name))).filter(Boolean),
  )) as string[];
  const targetsKey = mappedTargets.join(",");
  const requestedTargets = useRef(new Set<string>());
  useEffect(() => {
    for (const target of targetsKey ? targetsKey.split(",") : []) {
      if (requestedTargets.current.has(target)) continue;
      requestedTargets.current.add(target);
      listFields(target)
        .then((flds) => setTargetFields((prev) => ({ ...prev, [target]: flds })))
        .catch(() => setTargetFields((prev) => ({ ...prev, [target]: [] })));
    }
  }, [targetsKey]);

  // Parse CSV file client-side (first 20 rows + header).
  const parseFile = (f: File) => {
    const reader = new FileReader();
    reader.onload = (ev) => {
      const text = String(ev.target?.result ?? "").replace(/^\uFEFF/, "");
      const sep = csvSeparator(text.split(/\r?\n/, 1)[0] ?? "");
      const all = parseCsv(text, sep);
      const header = all[0] ?? [];
      const rows = all.slice(1, 21); // preview: first 20 data rows
      setCsvHeaders(header);
      setCsvRows(rows);
      // Auto-guess mapping: CSV col → field whose name/label matches (case-insensitive).
      const guessed: Record<string, string> = {};
      for (const col of header) {
        const match = fields.find(
          (ff) =>
            ff.name.toLowerCase() === col.toLowerCase() ||
            (ff.label ?? "").toLowerCase() === col.toLowerCase(),
        );
        if (match) guessed[col] = match.name;
      }
      setMapping(guessed);
      // Default dedup: preselect when the guessed mapping covers EXACTLY ONE unique field.
      const mappedUnique = fields.filter(
        (ff) => ff.isUnique && Object.values(guessed).includes(ff.name) && isDedupable(ff),
      );
      setDedupField(mappedUnique.length === 1 ? mappedUnique[0].name : "");
      setFile(f);
      setStep("map");
    };
    reader.readAsText(f, "utf-8");
  };

  // The file selector hands over whatever was picked OR dropped — a drop skips the picker's
  // `accept` filter, so a spreadsheet would otherwise be parsed as garbage "columns".
  const pickFile = (f: File | undefined) => {
    if (!f) return;
    if (!isCsv(f)) {
      setFileErr(t("page.import.upload.notCsv"));
      return;
    }
    setFileErr("");
    parseFile(f);
  };

  // Dedup candidates: mapped fields only (an unmapped field never has a value in the payload —
  // dedup on it would be a silent no-op) whose CSV value coerces to a string on the BE
  // (record_import.go coerceCell: number/rating/boolean/multi_select/currency/composite don't).
  const mappedNames = new Set(Object.values(mapping).filter(Boolean));
  const dedupCandidates = fields.filter((f) => mappedNames.has(f.name) && isDedupable(f));

  // Re-map one column; if that unmapped the selected dedup field, drop the selection too (a
  // dedup on an unmapped field would send a no-op param).
  const mapColumn = (col: string, fieldName: string) => {
    const next = { ...mapping, [col]: fieldName };
    setMapping(next);
    if (dedupField && !Object.values(next).includes(dedupField)) setDedupField("");
  };
  // Only choices for relations still mapped go on the wire (the server refuses the others).
  const sentMatchBy = Object.fromEntries(
    Object.entries(matchBy).filter(([rel, target]) => target && Object.values(mapping).includes(rel)),
  );

  // Step 3: the server judges every row exactly as the real run will, writing nothing.
  const goPreview = async () => {
    setStep("preview");
    if (!file) return;
    const seq = ++checkSeq.current;
    setCheck(null);
    setChecking(true);
    setErrMsg("");
    try {
      await ensureAuth();
      const res = await importRecords(entity, file, mapping, {
        dedupField: dedupField || undefined, matchBy: sentMatchBy, dryRun: true,
      });
      if (seq === checkSeq.current) setCheck(res);
    } catch (e) {
      if (seq === checkSeq.current) setErrMsg(e instanceof ApiError ? e.message : t("page.import.preview.checkFailed"));
    } finally {
      if (seq === checkSeq.current) setChecking(false);
    }
  };

  const runImport = async () => {
    if (!file) return;
    setRunning(true);
    setErrMsg("");
    try {
      await ensureAuth();
      const res = await importRecords(entity, file, mapping, {
        dedupField: dedupField || undefined, matchBy: sentMatchBy, runAutomations,
      });
      setResult(res);
      setStep("result");
    } catch (e) {
      setErrMsg(e instanceof ApiError ? e.message : t("page.import.failed"));
    } finally {
      setRunning(false);
    }
  };

  const restart = () => {
    setStep("upload");
    setFile(null);
    setFileErr("");
    setCsvHeaders([]);
    setCsvRows([]);
    setMapping({});
    setDedupField("");
    setMatchBy({});
    setResult(null);
    setCheck(null);
  };

  const stepIdx = STEPS.findIndex((s) => s.id === step);
  const fieldLabel = (name: string) => fields.find((f) => f.name === name)?.label ?? name;
  const mappedCols = csvHeaders.filter((c) => mapping[c]);
  const validCount = check ? (check.valid ?? check.total - check.failed) : undefined;

  const fieldOptions: ComboboxOption[] = [
    { value: "", label: t("page.import.map.skip") },
    ...fields.map((f) => ({ value: f.name, label: `${f.label ?? f.name} (${f.name})` })),
  ];
  const dedupOptions: ComboboxOption[] = [
    { value: "", label: t("page.import.dedup.none") },
    ...dedupCandidates.map((f) => ({
      value: f.name,
      label: `${f.label ?? f.name} (${f.name})${f.isUnique ? t("page.import.dedup.unique") : ""}`,
    })),
  ];

  return (
    <div className="slds-p-around_medium mx-auto flex w-full max-w-5xl flex-col gap-3">
      <PageHeader
        icon={{ glyph: Upload, name: "record", label: entityLabel }}
        title={tf("page.import.title", { entity: entityLabel })}
        meta={file?.name}
      />

      <Card title={t(STEPS[stepIdx].title)}>
        <ProgressIndicator
          steps={STEPS.map((s) => ({ id: s.id, label: t(s.label) }))}
          current={stepIdx}
          label={t("page.import.progress")}
          className="slds-m-bottom_large"
        />

        {errMsg && <ScopedNotification theme="error" className="slds-m-bottom_medium">{errMsg}</ScopedNotification>}

        {/* Step 1: Upload */}
        {step === "upload" && (
          <>
            <p className="slds-m-bottom_small">{t("page.import.upload.hint")}</p>
            <FileSelector
              label={t("page.import.upload.label")}
              accept=".csv,text/csv"
              buttonLabel={t("page.import.upload.button")}
              dropLabel={t("page.import.upload.drop")}
              error={fileErr || undefined}
              onFiles={(files) => pickFile(files[0])}
            />
            <div className="slds-m-top_medium">
              <Button onClick={onCancel}>{t("action.cancel")}</Button>
            </div>
          </>
        )}

        {/* Step 2: Map columns */}
        {step === "map" && (
          <>
            <p className="slds-text-color_weak slds-m-bottom_small">
              {tf("page.import.map.meta", { file: file?.name ?? "", cols: csvHeaders.length, rows: csvRows.length })}
            </p>
            <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label={t("page.import.map.table")}>
              <thead>
                <tr className="slds-line-height_reset">
                  <th scope="col" className="w-1/2">
                    <div className="slds-truncate" title={t("page.import.map.colCsv")}>{t("page.import.map.colCsv")}</div>
                  </th>
                  <th scope="col">
                    <div className="slds-truncate" title={t("page.import.map.colField")}>{t("page.import.map.colField")}</div>
                  </th>
                </tr>
              </thead>
              <tbody>
                {csvHeaders.map((col) => {
                  const target = relationTarget(fields.find((f) => f.name === mapping[col]));
                  return (
                    <tr key={col} className="slds-hint-parent">
                      <th scope="row">
                        <div className="slds-truncate" title={col}>{col}</div>
                      </th>
                      <td>
                        <Combobox
                          label={tf("page.import.map.fieldFor", { col })}
                          labelHidden
                          options={fieldOptions}
                          value={mapping[col] ?? ""}
                          onChange={(v) => mapColumn(col, v ?? "")}
                        />
                        {fields.find((f) => f.name === mapping[col])?.fieldType === "user" && (
                          <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("page.import.map.userHelp")}</p>
                        )}
                        {target && (
                          <div className="slds-m-top_x-small">
                            <Combobox
                              label={tf("page.import.map.matchBy", { field: fieldLabel(mapping[col]) })}
                              help={t("page.import.map.matchByHelp")}
                              options={matchOptions(targetFields[target])}
                              value={matchBy[mapping[col]] ?? ""}
                              onChange={(v) => setMatchBy((prev) => ({ ...prev, [mapping[col]]: v ?? "" }))}
                            />
                          </div>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>

            <p className="slds-text-color_weak slds-m-top_small">{t("page.import.map.hint")}</p>

            {/* Dedup selector — the BE skips rows whose value for this field already exists
                (in the DB or earlier in the file) instead of aborting the whole batch on a
                unique-constraint collision (409 IMPORT_BATCH_DUPLICATE). */}
            <div className="slds-m-top_medium slds-size_1-of-1 slds-medium-size_1-of-2">
              <Combobox
                label={t("page.import.dedup.label")}
                help={t("page.import.dedup.help")}
                options={dedupOptions}
                value={dedupField}
                onChange={(v) => setDedupField(v ?? "")}
              />
            </div>

            <div className="slds-m-top_medium">
              <Checkbox
                label={t("page.import.automations.label")}
                help={t("page.import.automations.help")}
                checked={runAutomations}
                onChange={(e) => setRunAutomations(e.target.checked)}
              />
            </div>

            <div className="slds-grid slds-grid_align-spread slds-m-top_medium">
              <Button iconLeft={ArrowLeft} onClick={() => setStep("upload")}>{t("page.import.back")}</Button>
              <Button variant="brand" iconRight={ArrowRight} onClick={goPreview}>
                {t("page.import.toPreview")}
              </Button>
            </div>
          </>
        )}

        {/* Step 3: Preview — the server's dry run marks every wrong cell before anything is written. */}
        {step === "preview" && (
          <>
            {checking && (
              <div className="slds-grid slds-grid_vertical-align-center slds-m-bottom_small">
                <Spinner size="x-small" inline label={t("page.import.preview.checking")} />
                <span className="slds-m-left_x-small slds-text-color_weak">{t("page.import.preview.checking")}</span>
              </div>
            )}
            {check && validCount !== undefined && (
              <ScopedNotification
                theme={validCount === check.total ? "success" : validCount === 0 ? "error" : "warning"}
                className="slds-m-bottom_small"
              >
                {validCount === check.total
                  ? tf("page.import.preview.allValid", { n: check.total })
                  : validCount === 0
                    ? t("page.import.preview.noneValid")
                    : tf("page.import.preview.someInvalid", { bad: check.total - validCount, valid: validCount })}
              </ScopedNotification>
            )}
            <PreviewTable
              headers={csvHeaders}
              rows={csvRows.slice(0, PREVIEW_ROWS)}
              columns={mappedCols}
              mapping={mapping}
              fieldLabel={fieldLabel}
              errors={check?.errors ?? []}
            />
            <p className="slds-text-color_weak slds-m-top_small">
              {tf("page.import.preview.note", { shown: Math.min(PREVIEW_ROWS, csvRows.length), total: csvRows.length })}
            </p>
            {check && (check.errors?.length ?? 0) > 0 && (
              <ExpandableSection title={tf("page.import.result.errors", { n: check.errors!.length })} className="slds-m-top_medium">
                <ImportErrors errors={check.errors!} fieldLabel={fieldLabel} />
              </ExpandableSection>
            )}
            <div className="slds-grid slds-grid_align-spread slds-m-top_medium">
              <Button iconLeft={ArrowLeft} onClick={() => setStep("map")}>{t("page.import.back")}</Button>
              <Button
                variant="brand"
                iconLeft={running ? undefined : Play}
                onClick={runImport}
                disabled={running || checking || validCount === 0}
              >
                {running
                  ? t("page.import.running")
                  : validCount !== undefined
                    ? tf("page.import.preview.runValid", { n: validCount })
                    : t("page.import.run")}
              </Button>
            </div>
          </>
        )}

        {/* Step 4: Result */}
        {step === "result" && result && (
          <ImportOutcome result={result} fieldLabel={fieldLabel} onDone={onDone} onAgain={restart} />
        )}
      </Card>
    </div>
  );
}

function ImportOutcome({
  result, fieldLabel, onDone, onAgain,
}: {
  result: ImportResult;
  fieldLabel: (name: string) => string;
  onDone: () => void;
  onAgain: () => void;
}) {
  // Rows the BE skipped as duplicates (dedup) — reported inside errors[] with
  // code RECORD_DUPLICATE and counted into `failed`. Split them out for honesty.
  const errors = result.errors ?? [];
  const dupCount = errors.filter((e) => e.code === "RECORD_DUPLICATE").length;
  const otherFailed = result.failed - dupCount;
  const stats: { label: string; value: number; tone?: string }[] = [
    { label: t("page.import.result.total"), value: result.total },
    { label: t("page.import.result.inserted"), value: result.inserted, tone: "slds-text-color_success" },
    ...(dupCount > 0 ? [{ label: t("page.import.result.skippedDup"), value: dupCount }] : []),
    {
      label: t(dupCount > 0 ? "page.import.result.otherFailed" : "page.import.result.failed"),
      value: otherFailed,
      tone: otherFailed > 0 ? "slds-text-color_error" : undefined,
    },
  ];
  const summary =
    dupCount > 0 && otherFailed > 0
      ? tf("page.import.result.dupAndFailed", { dup: dupCount, failed: otherFailed })
      : dupCount > 0
        ? tf("page.import.result.dupOnly", { dup: dupCount })
        : tf("page.import.result.failedOnly", { failed: result.failed });

  return (
    <>
      {/* Counts as page-header detail blocks: label above the value. */}
      <ul className="slds-grid slds-wrap slds-m-bottom_medium">
        {stats.map((s) => (
          <li
            key={s.label}
            className={stats.length === 4 ? "slds-size_1-of-2 slds-medium-size_1-of-4" : "slds-size_1-of-3"}
          >
            <div className="slds-text-title slds-truncate" title={s.label}>{s.label}</div>
            <div className={`slds-text-heading_large tabular-nums ${s.tone ?? ""}`}>{s.value}</div>
          </li>
        ))}
      </ul>

      {(result.auditJobsFailed ?? 0) > 0 && (
        <div className="slds-m-bottom_small">
          <ScopedNotification theme="warning">{t("page.import.result.historyMissing")}</ScopedNotification>
        </div>
      )}
      {result.failed === 0 ? (
        <ScopedNotification theme="success">{t("page.import.result.ok")}</ScopedNotification>
      ) : (
        <>
          <ScopedNotification theme="warning">{summary}</ScopedNotification>
          {errors.length > 0 && (
            <ExpandableSection title={tf("page.import.result.errors", { n: errors.length })} className="slds-m-top_medium">
              <ImportErrors errors={errors} fieldLabel={fieldLabel} />
            </ExpandableSection>
          )}
        </>
      )}

      <div className="slds-m-top_medium">
        <Button variant="brand" onClick={onDone}>{t("page.import.result.backToList")}</Button>
        <Button onClick={onAgain}>{t("page.import.result.again")}</Button>
      </div>
    </>
  );
}

// The preview grid: the first rows as the file has them, each cell the server refused marked in
// place with its message (an error names its field; a row error without one marks the row number).
function PreviewTable({
  headers, rows, columns, mapping, fieldLabel, errors,
}: {
  headers: string[];
  rows: string[][];
  columns: string[];
  mapping: Record<string, string>;
  fieldLabel: (name: string) => string;
  errors: RowError[];
}) {
  const cellErr = new Map<string, string>();
  const rowErr = new Map<number, string>();
  for (const e of errors) {
    const key = e.field ? `${e.row}:${e.field}` : "";
    if (key && columns.some((c) => mapping[c] === e.field)) {
      cellErr.set(key, cellErr.has(key) ? `${cellErr.get(key)} · ${e.message}` : e.message);
    } else {
      rowErr.set(e.row, rowErr.has(e.row) ? `${rowErr.get(e.row)} · ${e.message}` : e.message);
    }
  }
  return (
    <div className="slds-scrollable_x">
      <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label={t("page.import.preview.table")}>
        <thead>
          <tr className="slds-line-height_reset">
            <th scope="col">
              <div className="slds-truncate">{t("page.import.preview.colRow")}</div>
            </th>
            {columns.map((c) => (
              <th key={c} scope="col">
                <div className="slds-truncate" title={fieldLabel(mapping[c])}>{fieldLabel(mapping[c])}</div>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row, ri) => {
            const n = ri + 1; // the server numbers data rows from 1, header excluded
            const rowMsg = rowErr.get(n);
            return (
              <tr key={ri} className="slds-hint-parent">
                <th scope="row" className={clsx("tabular-nums", rowMsg && "slds-cell-wrap")}>
                  <div className="slds-grid slds-grid_vertical-align-center">
                    <span>{n}</span>
                    {rowMsg && (
                      <Icon
                        icon={CircleAlert}
                        size="x-small"
                        tone="error"
                        label={tf("page.import.preview.rowError", { row: n })}
                        className="slds-m-left_xx-small"
                      />
                    )}
                  </div>
                  {rowMsg && <div className="slds-text-body_small slds-text-color_error">{rowMsg}</div>}
                </th>
                {columns.map((c) => {
                  const v = row[headers.indexOf(c)] ?? "";
                  const msg = cellErr.get(`${n}:${mapping[c]}`);
                  return (
                    <td key={c} className={clsx(msg && "slds-has-error slds-cell-wrap")}>
                      <div className={clsx("slds-truncate max-w-[200px]", msg && "slds-text-color_error")} title={v}>
                        {msg && <Icon icon={CircleAlert} size="x-small" tone="error" className="slds-m-right_xx-small" />}
                        {v}
                      </div>
                      {msg && <div className="slds-text-body_small slds-text-color_error">{msg}</div>}
                    </td>
                  );
                })}
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// Every row error of a run or a preview: row · field label · message (duplicates tagged).
function ImportErrors({ errors, fieldLabel }: { errors: RowError[]; fieldLabel: (name: string) => string }) {
  return (
    <div className="slds-scrollable_x">
      <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label={t("page.import.result.errorsTable")}>
        <thead>
          <tr className="slds-line-height_reset">
            <th scope="col"><div className="slds-truncate">{t("page.import.result.colRow")}</div></th>
            <th scope="col"><div className="slds-truncate">{t("page.import.result.colField")}</div></th>
            <th scope="col"><div className="slds-truncate">{t("page.import.result.colError")}</div></th>
          </tr>
        </thead>
        <tbody>
          {errors.map((err, i) => {
            const isDup = err.code === "RECORD_DUPLICATE";
            return (
              <tr key={i} className="slds-hint-parent">
                <th scope="row" className="tabular-nums">{err.row}</th>
                <td>{err.field ? fieldLabel(err.field) : "—"}</td>
                <td className="slds-cell-wrap">
                  {isDup && (
                    <Badge theme="warning" className="slds-m-right_x-small">{t("page.import.result.dupTag")}</Badge>
                  )}
                  <span className={isDup ? undefined : "slds-text-color_error"}>{err.message}</span>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// The entity a relation field points at, or "" (not a relation / unmapped).
function relationTarget(f: FieldMeta | undefined): string {
  const target = f?.fieldType === "relation" ? f.options?.relatedEntity : undefined;
  return typeof target === "string" ? target : "";
}

// Types a relation cell may name a target record by (specs/061 FR-012): stored as plain text.
const MATCHABLE_TYPES = new Set(["text", "email", "auto_number"]);

// "Khớp theo" choices: the server's default first, then the target's text/email/number-code fields.
function matchOptions(target: FieldMeta[] | undefined): ComboboxOption[] {
  return [
    { value: "", label: t("page.import.map.matchDefault") },
    ...(target ?? [])
      .filter((f) => MATCHABLE_TYPES.has(f.fieldType))
      .map((f) => ({
        value: f.name,
        label: `${f.label ?? f.name}${f.isUnique || f.fieldType === "auto_number" ? t("page.import.map.matchUnique") : ""}`,
      })),
  ];
}

// The separator of a header line: a Vietnamese-region Excel saves CSV with ';' (specs/061 FR-008 —
// the server splits on the SAME rule, so the columns shown are the columns imported).
function csvSeparator(headerLine: string): "," | ";" {
  const count = (ch: string) => headerLine.split(ch).length - 1;
  return count(";") > count(",") ? ";" : ",";
}

// Parse a whole CSV text per RFC 4180 (#360): a quoted cell may hold the separator, a doubled quote
// and a line break, so records are cut by the quote state, never by splitting on newlines — the
// preview's row numbers then equal the server's (encoding/csv). Blank lines are skipped as it does.
export function parseCsv(text: string, sep: "," | ";" = ","): string[][] {
  const out: string[][] = [];
  let row: string[] = [];
  let cur = "";
  let inQuote = false;
  const endRow = () => {
    row.push(cur.trim());
    cur = "";
    if (row.length > 1 || row[0] !== "") out.push(row);
    row = [];
  };
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (ch === '"') {
      if (inQuote && text[i + 1] === '"') { cur += '"'; i++; }
      else inQuote = !inQuote;
    } else if (inQuote) {
      cur += ch;
    } else if (ch === sep) {
      row.push(cur.trim());
      cur = "";
    } else if (ch === "\n" || ch === "\r") {
      if (ch === "\r" && text[i + 1] === "\n") i++;
      endRow();
    } else {
      cur += ch;
    }
  }
  if (cur !== "" || row.length > 0) endRow();
  return out;
}

// A CSV by name or declared type (Windows often reports text/csv as application/vnd.ms-excel,
// so the extension alone must be enough).
function isCsv(f: File): boolean {
  return /\.csv$/i.test(f.name) || f.type === "text/csv";
}

const SYSTEM_FIELDS = [
  "id", "tenant_id", "team_set_id", "created_by", "modified_by",
  "created_at", "updated_at", "deleted_at",
];

// Field types whose CSV cell does NOT coerce to a string on the BE (record_import.go
// coerceCell) — dedup compares string values only, so offering these would be a silent no-op.
const DEDUP_EXCLUDED_TYPES = new Set([
  "number", "rating", "boolean", "multi_select", "currency", "composite",
]);

// A field is dedup-able when its imported value is a string the BE can compare.
function isDedupable(f: FieldMeta): boolean {
  return !DEDUP_EXCLUDED_TYPES.has(f.fieldType);
}
