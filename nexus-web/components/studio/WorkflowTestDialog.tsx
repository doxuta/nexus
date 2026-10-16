"use client";

// WorkflowTestDialog (W6 §3.4) — dry-run a SAVED rule against a real record. Since specs/074 lát 11 it opens IN the rule's page
// (a section, no dialog); the file keeps its name for the tests that import it.
// POST /admin/workflows/:id/test — ZERO side effect (§9.4): no mutation, no email/webhook/
// notification, no run row. The dialog says so explicitly. Trace rendering model is pure
// (lib/workflowRuns.shapeTraceStep — unit-tested); this file only lays it out.
// Blueprints: `modals` base; record picker = `input` (search) + `radio-group` over the entity's top
// rows (listed before anything is typed, like the old picker — the kit Lookup only searches once a
// term is typed); the record the dry run used collapses into a `pills` pill; trace =
// `scoped-notifications` verdict + a list of steps with `badges`.
// The radio is only a PENDING pick, committed by "Chạy thử": native radios tick the next record on
// every arrow press, so committing on change would unmount the list after one ↓ and leave records
// 3..n out of keyboard reach (WCAG 2.1.1).

import { useEffect, useMemo, useState } from "react";
import { Bolt, Filter, FlaskConical, Search, Zap } from "lucide-react";
import { listRecords, testWorkflow, ApiError, type ApiRecord, type FieldMeta } from "@/lib/api";
import { isBeforeTrigger } from "@/lib/workflowBefore";
import { BeforeTestPanel, beforeSimOf, type BeforeTestDraft } from "./before/BeforeTestPanel";
import { errorMessage } from "@/lib/errorMessages";
import { recordLabel } from "@/lib/recordLabel";
import { shapeTraceStep, type WorkflowTestResult, type TraceVerdict } from "@/lib/workflowRuns";
import { t } from "@/lib/i18n";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { fill } from "@/components/slds/DataTable";
import { Icon } from "@/components/slds/Icon";
import { Input } from "@/components/slds/Input";
import { Pill } from "@/components/slds/Pill";
import { RadioGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

const VERDICT_THEME: Record<TraceVerdict, "success" | "warning" | "error" | undefined> = {
  pass: "success",
  fail: "warning",
  blocked: "error",
  skipped: undefined,
};

const KIND_ICON = { trigger: Bolt, condition: Filter, action: Zap } as const;

export function WorkflowTestDialog({
  onClose,
  workflowId,
  workflowName,
  entityName,
  calendarNames,
  trigger,
  fields = [],
  saveOn,
}: {
  /** Closes the section (the host unmounts it, so every open starts fresh). */
  onClose: () => void;
  workflowId: string;
  workflowName: string;
  entityName: string;
  /** Working calendars by id — a set_deadline pinned to one (queue #79) names it in the trace. */
  calendarNames?: Record<string, string>;
  /** The saved rule's trigger — a before rule gets the BeforeTestPanel (which write to rehearse). */
  trigger?: string;
  /** The entity's fields, for the simulated changes of a before rule. */
  fields?: FieldMeta[];
  /** condition.saveOn of a before_save rule: picks the write rehearsed first. */
  saveOn?: string;
}) {
  // Record picker state
  const [q, setQ] = useState("");
  const [debouncedQ, setDebouncedQ] = useState("");
  // Last finished search, tagged with the query it answers: "searching" is derived (the current
  // query has no answer yet) instead of being set inside the effect.
  const [found, setFound] = useState<{ key: string; records: ApiRecord[]; err: string | null } | null>(null);
  // pendingId = the ticked radio; selected = the record the dry run used (shown as the pill).
  const [pendingId, setPendingId] = useState<string | null>(null);
  const [selected, setSelected] = useState<{ id: string; label: string } | null>(null);

  // specs/048: a before rule rehearses a simulated write (op + changes) instead of just a record.
  const before = isBeforeTrigger(trigger);
  const [draft, setDraft] = useState<BeforeTestDraft>({
    op: saveOn === "create" ? "create" : "update", rows: [], withEntityRules: false,
  });
  // A simulated create has no stored record to start from; every other rehearsal does.
  const needsRecord = !(before && trigger === "before_save" && draft.op === "create");

  // Test run state
  const [running, setRunning] = useState(false);
  const [runErr, setRunErr] = useState<string | null>(null);
  const [result, setResult] = useState<WorkflowTestResult | null>(null);

  // NOTE: no reset-on-open effect needed — the parent mounts this dialog conditionally
  // ({testOpen && <WorkflowTestDialog …>}), so every open starts from fresh state.

  useEffect(() => {
    const timer = setTimeout(() => setDebouncedQ(q.trim()), 250);
    return () => clearTimeout(timer);
  }, [q]);

  // Search records of the rule's entity (top rows when query is empty).
  const query = debouncedQ ? `q=${encodeURIComponent(debouncedQ)}&limit=8` : "limit=8";
  const searchKey = `${entityName}?${query}`;
  useEffect(() => {
    if (!entityName || selected) return;
    let cancelled = false;
    listRecords(entityName, query).then(
      (recs) => { if (!cancelled) setFound({ key: searchKey, records: recs, err: null }); },
      (e) => { if (!cancelled) setFound((prev) => ({ key: searchKey, records: prev?.records ?? [], err: errorMessage(e) })); },
    );
    return () => { cancelled = true; };
  }, [entityName, query, searchKey, selected]);
  const records = found?.records ?? [];
  const searchErr = found?.key === searchKey ? found.err : null;
  const searching = !selected && found?.key !== searchKey;

  const labels = new Map(records.map((r) => [r.id, recordLabel(r, [])]));
  // A pick the current search no longer lists is not runnable (the radio for it is gone).
  const pending = pendingId && labels.has(pendingId) ? { id: pendingId, label: labels.get(pendingId)! } : null;
  const target = selected ?? pending;
  const runnable = needsRecord ? !!target : true;

  async function runTest() {
    if (!runnable) return;
    if (target) setSelected(target);
    setRunning(true); setRunErr(null); setResult(null);
    try {
      const res = before
        ? await testWorkflow(workflowId, needsRecord ? target?.id : undefined, beforeSimOf(draft, fields))
        : await testWorkflow(workflowId, target!.id);
      setResult(res);
    } catch (e) {
      setRunErr(e instanceof ApiError ? errorMessage(e) : t("page.workflow.test.failed"));
    } finally {
      setRunning(false);
    }
  }

  // Vetoes the rehearsed write would hit — read once from the trace, shown above the steps.
  const blocked = result
    ? result.trace.flatMap((s) => (s.detail?.type === "veto" && s.detail.wouldBlock === true ? [String(s.detail.message ?? "")] : []))
    : [];

  const steps = useMemo(() => (result ? result.trace.map((s) => shapeTraceStep(s, calendarNames)) : []), [result, calendarNames]);

  return (
    <div className="flex flex-col gap-3" role="group" aria-label={fill(t("page.workflow.test.title"), { name: workflowName })}>
      <p className="slds-text-color_weak">{t("page.workflow.test.tagline")}</p>
        {before && trigger && (
          <BeforeTestPanel trigger={trigger} fields={fields} draft={draft} onDraft={(d) => { setDraft(d); setResult(null); }} />
        )}

        {/* Record picker */}
        {!needsRecord ? null : !selected ? (
          <>
            <Input
              label={t("page.workflow.test.record")}
              iconLeft={Search}
              loading={searching}
              value={q}
              autoFocus
              placeholder={fill(t("page.workflow.test.searchPh"), { entity: entityName })}
              onChange={(e) => setQ(e.target.value)}
            />
            {searchErr ? (
              <ScopedNotification theme="error">{searchErr}</ScopedNotification>
            ) : !searching && records.length === 0 ? (
              <p className="slds-text-color_weak">{t("page.workflow.test.noRecords")}</p>
            ) : (
              <RadioGroup
                label={t("page.workflow.test.pick")}
                options={records.map((r) => ({ value: r.id, label: `${labels.get(r.id)} · ${r.id.slice(0, 8)}` }))}
                value={pendingId ?? ""}
                onValueChange={setPendingId}
              />
            )}
          </>
        ) : (
          <div className="slds-form-element">
            <span className="slds-form-element__label">{t("page.workflow.test.record")}</span>
            <div className="slds-form-element__control">
              <Pill
                label={`${selected.label} · ${selected.id.slice(0, 8)}`}
                removeLabel={t("page.workflow.test.unpick")}
                onRemove={() => { setSelected(null); setResult(null); setRunErr(null); }}
              />
            </div>
          </div>
        )}

        {runErr && <ScopedNotification theme="error">{runErr}</ScopedNotification>}

        {/* Trace result */}
        {result && (
          <>
            <ScopedNotification theme={result.matched ? "success" : "light"}>
              {result.matched ? t("page.workflow.test.matched") : t("page.workflow.test.notMatched")}
            </ScopedNotification>
            {blocked.map((m, i) => (
              <ScopedNotification key={i} theme="error">{fill(t("workflow.test.before.wouldBlock"), { message: m })}</ScopedNotification>
            ))}
            <ol className="slds-has-dividers_around-space">
              {steps.map((s) => (
                <li key={s.step} className="slds-item">
                  <div className="slds-media slds-media_center">
                    <div className="slds-media__figure">
                      <Icon icon={KIND_ICON[s.kind]} size="x-small" label={fill(t("page.workflow.test.step"), { n: s.step })} />
                    </div>
                    <div className="slds-media__body slds-truncate">
                      <span className="slds-text-title_bold">{s.title}</span>
                    </div>
                    <div className="slds-no-flex">
                      <Badge theme={VERDICT_THEME[s.verdict]} variant={s.verdict === "skipped" ? "lightest" : "default"}>
                        {s.verdictLabel}
                      </Badge>
                    </div>
                  </div>
                  {s.rows.length > 0 && (
                    <dl className="slds-list_horizontal slds-wrap slds-m-top_x-small">
                      {s.rows.map((row) => (
                        <div key={row.label} className="contents">
                          <dt className="slds-item_label slds-text-color_weak">{row.label}</dt>
                          <dd className={row.mono ? "slds-item_detail slds-text-font_monospace slds-hyphenate" : "slds-item_detail slds-hyphenate"}>
                            {row.value}
                          </dd>
                        </div>
                      ))}
                    </dl>
                  )}
                  {s.payload && (
                    <pre className="slds-box slds-box_xx-small slds-theme_shade slds-text-font_monospace slds-m-top_x-small max-h-36 overflow-auto">
                      {s.payload}
                    </pre>
                  )}
                  {s.warning && <ScopedNotification theme="warning" className="slds-m-top_x-small">{s.warning}</ScopedNotification>}
                  {s.error && <p className="slds-text-color_error slds-text-font_monospace slds-hyphenate slds-m-top_x-small">{s.error}</p>}
                </li>
              ))}
            </ol>
          </>
        )}
      <div className="slds-grid slds-grid_align-end">
        <Button onClick={onClose}>{t("page.workflow.test.close")}</Button>
        <Button variant="brand" className="slds-m-left_x-small" iconLeft={FlaskConical} onClick={runTest} disabled={!runnable || running}>
          {running ? t("page.workflow.test.running") : result ? t("page.workflow.test.rerun") : t("page.workflow.test.run")}
        </Button>
      </div>
    </div>
  );
}
