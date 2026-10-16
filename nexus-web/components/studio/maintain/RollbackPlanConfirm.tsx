"use client";

// RollbackPlanConfirm — "Quay lại bản này" confirmed IN the page, never in a modal (specs/079 f7-lich-su-quay-lai,
// specs/063 S7-7). Lists every document the rollback touches (not just "are you sure?"), the things the builder must
// know first (signing keys that go back, rules that switch back on and send data out, someone else's applied-at-once
// edit that gets overwritten, roles/permissions that do not go back) and, when the target never captured a type, makes
// the builder accept that it stays as it is. The plan is never cached; the request carries the head version and token of
// the plan on screen, and a refusal that means "the plan moved" reloads it instead of resending the old numbers.
// The host (history timeline / version page) owns the pending-draft count and passes it in; null/undefined = not known.
import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { ApiError, getRollbackPlan, rollbackToVersion, type PublishVersion, type RollbackPlan } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { rollbackErrorKey } from "@/lib/rollbackPlan";
import { toast } from "@/components/ui/Toaster";
import { Checkbox } from "@/components/slds/Checkbox";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { Textarea } from "@/components/slds/Textarea";
import { ChangeList, type ChangeItem } from "../ChangeList";
import { InlineConfirm } from "../InlineConfirm";
import { useProjectQuery } from "../useProjectQuery";

const NOTE_MAX = 500;
// ponytail: one retry after a fixed pause; the server's publish lock is held for seconds, not minutes.
const BUSY_RETRY_MS = 1500;
const OP: Record<RollbackPlan["changes"][number]["op"], ChangeItem["op"]> = { create: "add", update: "change", delete: "remove" };
const OP_WORD: Record<RollbackPlan["changes"][number]["op"], I18nKey> = {
  create: "studio.maintain.rollback.op.create", update: "studio.maintain.rollback.op.update", delete: "studio.maintain.rollback.op.delete",
};
const TYPE_LABEL: Record<string, I18nKey> = {
  workflow: "draftBar.type.workflow", report: "draftBar.type.report", dashboard: "draftBar.type.dashboard",
  app: "draftBar.type.app", layout: "draftBar.type.layout", branding: "draftBar.type.branding",
  field: "draftBar.type.field", entity: "draftBar.type.entity", relationship: "draftBar.type.relationship",
};
const typeLabel = (docType: string) => (TYPE_LABEL[docType] ? t(TYPE_LABEL[docType]) : docType);
const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

export interface RollbackPlanConfirmProps {
  /** Project code: the plan is read through useProjectQuery so a token of another project never fetches it. */
  project: string;
  /** The version to go back to (a numbered one; an applied-at-once entry is reverted, not rolled back to). */
  version: Pick<PublishVersion, "id" | "versionNo">;
  /** Drafts waiting to be published; null/undefined while loading, failed or absent = unknown, which also locks the button (FR-081). */
  pendingCount: number | null | undefined;
  /** From publish state; the "up to N seconds" clause is dropped when absent. */
  propagationSeconds?: number;
  onCancel: () => void;
  /** Called with the number of the version the rollback created. */
  onDone?: (newVersionNo: number) => void;
}

function Warn({ testId, theme, heading, children }: { testId: string; theme: "warning" | "info"; heading: string; children: React.ReactNode }) {
  return (
    <div data-testid={testId} className="slds-m-top_small">
      <ScopedNotification theme={theme}>
        <p className="slds-text-title">{heading}</p>
        {children}
      </ScopedNotification>
    </div>
  );
}

export function RollbackPlanConfirm({ project, version, pendingCount, propagationSeconds, onCancel, onDone }: RollbackPlanConfirmProps) {
  const qc = useQueryClient();
  const q = useProjectQuery(project, ["rollback-plan", version.id], () => getRollbackPlan(version.id), { fresh: true, refetchOnWindowFocus: false, retry: false });
  const plan = q.data;
  const [note, setNote] = useState("");
  const [accept, setAccept] = useState(false);
  const [info, setInfo] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const inFlight = useRef(false);

  async function submit() {
    if (!plan || inFlight.current) return;
    inFlight.current = true;
    setBusy(true);
    setErr(null);
    setInfo(null);
    try {
      const body = { expectedHeadVersion: plan.headVersionNo, expectedPlanToken: plan.planToken, note: note.trim() || undefined, acceptUntouched: plan.untouchedTypes.length > 0 ? accept : undefined };
      let res;
      try {
        res = await rollbackToVersion(version.id, body);
      } catch (e) {
        if (!(e instanceof ApiError) || e.code !== "PUBLISH_BUSY") throw e;
        await sleep(BUSY_RETRY_MS);
        res = await rollbackToVersion(version.id, body);
      }
      toast.success(tf("studio.maintain.rollback.done", { n: plan.targetVersionNo, m: res.versionNo }));
      emitDraftsChanged("rollback");
      void qc.invalidateQueries();
      onDone?.(res.versionNo);
    } catch (e) {
      const key = e instanceof ApiError ? rollbackErrorKey(e.code) : null;
      if (e instanceof ApiError && (e.code === "HEAD_MOVED" || e.code === "PLAN_CHANGED")) {
        setAccept(false);
        const r = await q.refetch();
        if (r.isError) setErr(t("studio.maintain.rollback.loadFailed"));
        else setInfo(t(key as I18nKey));
      } else if (key) {
        // Fixed sentence + the server's own sentence; the document label only when details.label (E-UI-8) carries one.
        const d = e instanceof ApiError && e.details && typeof e.details === "object" ? (e.details as Record<string, unknown>) : {};
        const name = typeof d.label === "string" && d.label !== "" ? d.label : null;
        const server = e instanceof Error && e.message && e.message !== t(key) ? e.message : "";
        setErr([t(key), server, name ? tf("studio.maintain.rollback.errNamed", { name }) : ""].filter(Boolean).join(" "));
      } else setErr(tf("studio.maintain.rollback.failed", { msg: e instanceof Error ? e.message : String(e) }));
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  }

  const items: ChangeItem[] = (plan?.changes ?? []).map((c) => ({
    op: OP[c.op],
    key: `${c.docType}:${c.docKey}`,
    text: <>{c.label} <span className="slds-text-color_weak">· {typeLabel(c.docType)} · {t(OP_WORD[c.op])}</span></>,
  }));
  const needAccept = (plan?.untouchedTypes.length ?? 0) > 0;
  const rehides = plan?.rehides ?? [];
  const empty = plan !== undefined && plan.changes.length === 0 && rehides.length === 0;
  const pendingKnown = typeof pendingCount === "number";
  const hasPending = pendingKnown && pendingCount > 0;
  const message = propagationSeconds ? tf("studio.maintain.rollback.consequence", { s: propagationSeconds }) : t("studio.maintain.rollback.consequenceNoTime");
  const off = !plan || q.isFetching || q.isError || empty || !pendingKnown || hasPending || (needAccept && !accept);

  return (
    <InlineConfirm tone="warning" message={message} confirmLabel={busy ? t("studio.maintain.rollback.submitting") : t("studio.maintain.rollback.submit")}
      onConfirm={() => void submit()} onCancel={onCancel} busy={busy} confirmDisabled={off}>
      <div>
        {info && <ScopedNotification theme="info" className="slds-m-bottom_small">{info}</ScopedNotification>}
        {q.isError && !plan && <ScopedNotification theme="error" className="slds-m-bottom_small">{t("studio.maintain.rollback.loadFailed")}</ScopedNotification>}
        {!plan && !q.isError && (
          <div className="slds-p-around_small"><Spinner size="x-small" inline label={t("studio.maintain.rollback.loading")} /> {t("studio.maintain.rollback.loading")}</div>
        )}
        {empty && <p className="slds-text-color_weak">{t("studio.maintain.rollback.nothing")}</p>}
        {plan && plan.changes.length > 0 && (
          <>
            <p className="slds-text-title slds-m-bottom_x-small">{tf("studio.maintain.rollback.count", { n: plan.changes.length })}</p>
            {/* ponytail: eight rows tall (2.5rem each); the region scrolls past that. */}
            <div role="region" tabIndex={0} aria-label={tf("studio.maintain.rollback.listLabel", { n: plan.changes.length })} className="slds-scrollable_y" style={{ maxHeight: "20rem" }}>
              <ChangeList items={items} />
            </div>
          </>
        )}
        {plan && plan.secretRestores.length > 0 && (
          <Warn testId="rollback-secrets" theme="warning" heading={t("studio.maintain.rollback.secretHeading")}>
            <p>{t("studio.maintain.rollback.secretBody")}</p>
            <ul className="slds-list_dotted">{plan.secretRestores.map((s) => <li key={s.docKey}>{s.label}</li>)}</ul>
          </Warn>
        )}
        {plan && plan.reactivates.length > 0 && (
          <Warn testId="rollback-reactivates" theme="warning" heading={t("studio.maintain.rollback.reactivateHeading")}>
            <ul className="slds-list_dotted">
              {plan.reactivates.map((r) => (
                <li key={r.docKey}>
                  {r.outbound ? <strong>{r.label}</strong> : r.label}
                  {r.outbound && <span> · {t("studio.maintain.rollback.reactivateOutbound")}</span>}
                </li>
              ))}
            </ul>
          </Warn>
        )}
        {plan && plan.overridesDirect.length > 0 && (
          <Warn testId="rollback-overrides" theme="warning" heading={t("studio.maintain.rollback.overrideHeading")}>
            <ul className="slds-list_dotted">
              {plan.overridesDirect.map((o) => (
                <li key={`${o.docType}:${o.docKey}`}>{o.label} · {tf("studio.maintain.rollback.override", { who: o.byLabel, at: Number.isNaN(Date.parse(o.at)) ? o.at : new Date(o.at).toLocaleString("vi-VN") })}</li>
              ))}
            </ul>
          </Warn>
        )}
        {plan && plan.schemaRestores.length > 0 && (
          <Warn testId="rollback-schema" theme="info" heading={t("studio.maintain.rollback.schemaHeading")}>
            <ul className="slds-list_dotted">
              {plan.schemaRestores.map((s) => (
                <li key={`${s.docType}:${s.docKey}`}>
                  {s.docType === "field"
                    ? tf("studio.maintain.rollback.restoreField", { name: s.label })
                    : s.docType !== "entity"
                      ? <>{s.label} <span className="slds-text-color_weak">· {typeLabel(s.docType)}</span></>
                      : s.sweptCount
                      ? tf("studio.maintain.rollback.restoreEntity", { name: s.label, n: s.sweptCount })
                      : tf("studio.maintain.rollback.restoreEntityNoRecords", { name: s.label })}
                </li>
              ))}
            </ul>
          </Warn>
        )}
        {rehides.length > 0 && (
          <Warn testId="rollback-rehides" theme="info" heading={t("studio.maintain.rollback.rehideHeading")}>
            <ul className="slds-list_dotted">
              {rehides.map((r) => <li key={`${r.docType}:${r.docKey}`}>{r.label} <span className="slds-text-color_weak">· {typeLabel(r.docType)}</span></li>)}
            </ul>
          </Warn>
        )}
        {plan && plan.keptAsIs.length > 0 && (
          <Warn testId="rollback-kept" theme="warning" heading={t("studio.maintain.rollback.keptHeading")}>
            <ul className="slds-list_dotted">{plan.keptAsIs.map((o) => <li key={`${o.docType}:${o.docKey}`}>{o.label}</li>)}</ul>
          </Warn>
        )}
        {plan && needAccept && (
          <div className="slds-m-top_small">
            <ScopedNotification theme="warning">{tf("studio.maintain.rollback.untouched", { types: plan.untouchedTypes.map(typeLabel).join(", ") })}</ScopedNotification>
            <Checkbox label={t("studio.maintain.rollback.acceptUntouched")} checked={accept} onChange={(e) => setAccept(e.target.checked)} />
          </div>
        )}
        {plan && <p className="slds-m-top_small slds-text-color_weak">{t("studio.maintain.rollback.roles")}</p>}
        {hasPending && <ScopedNotification theme="warning" className="slds-m-top_small">{tf("studio.maintain.rollback.pending", { n: pendingCount })}</ScopedNotification>}
        {!pendingKnown && <ScopedNotification theme="warning" className="slds-m-top_small">{t("studio.maintain.rollback.pendingUnknown")}</ScopedNotification>}
        <Textarea className="slds-m-top_small" label={t("studio.maintain.rollback.note")} maxLength={NOTE_MAX} value={note} onChange={(e) => setNote(e.target.value)} />
        {err && <ScopedNotification theme="error" role="alert" className="slds-m-top_small">{err}</ScopedNotification>}
      </div>
    </InlineConfirm>
  );
}
