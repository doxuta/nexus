"use client";

// "Xoá vĩnh viễn" of one record in the Studio recycle bin (specs/078 lát u7-du-lieu-thung-rac-xoa-han, FR-058, FR-131).
// Two phases inside one InlineConfirm, never a modal. Phase 1 runs the DRY RUN as soon as the question opens and shows what
// would go in business words; the confirm button exists only once that report arrived for THIS record and nothing blocks it.
// Phase 2 sends {dryRun:false}, and only if the customer scope is still the one the report was read in.
import { useEffect, useMemo, useRef, useState } from "react";
import { listFields, purgeRecord, type RecordPurgeReport } from "@/lib/api";
import { purgeRefusal, purgeSummary, type PurgeLabels } from "@/lib/operateData";
import { t, tf } from "@/lib/i18n";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { InlineConfirm } from "@/components/studio/InlineConfirm";

type Phase = { kind: "loading" } | { kind: "ready"; report: RecordPurgeReport } | { kind: "error"; text: string; final: boolean };

export function RecordPurgeConfirm({ entity, id, label, entityLabel, assertScope, onClose, onPurged, onGone, onDenied }: {
  entity: string;
  id: string;
  label: string;
  entityLabel: (name: string) => string;
  /** FR-131: asked right before the destructive call, live (not at open time). */
  assertScope: () => boolean;
  onClose: () => void;
  /** The record (and the children the report named) are gone for good. */
  onPurged: (report: RecordPurgeReport) => void;
  /** The server says the record is no longer in the bin: drop the row. */
  onGone: () => void;
  /** 403 / no route: stop offering the action for the rest of the session. */
  onDenied: () => void;
}) {
  const [phase, setPhase] = useState<Phase>({ kind: "loading" });
  const [attempt, setAttempt] = useState(0);
  const [sending, setSending] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [fieldNames, setFieldNames] = useState<Record<string, string>>({});
  const inFlight = useRef(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; };
  }, []);

  const labels = useMemo<PurgeLabels>(() => ({ entity: entityLabel, field: (e, f) => fieldNames[`${e}.${f}`] ?? f }), [entityLabel, fieldNames]);

  const refuse = (e: unknown) => {
    const r = purgeRefusal(e, labels);
    if (r.hide) onDenied();
    if (r.gone) onGone();
    return r;
  };

  useEffect(() => {
    let live = true;
    purgeRecord(entity, id, { dryRun: true })
      .then((report) => {
        if (!live) return;
        // A report about another record must never arm the button.
        if (report.entity !== entity || report.id !== id) {
          setPhase({ kind: "error", text: t("studio.operate.recycle.err.generic"), final: false });
          return;
        }
        setPhase({ kind: "ready", report });
        // Field names of the records that only lose a link, so the sentence says "Khách hàng", not `account_id`.
        for (const en of new Set(report.cleared.map((c) => c.entity))) {
          listFields(en)
            .then((fs) => { if (live) setFieldNames((m) => ({ ...m, ...Object.fromEntries(fs.map((f) => [`${en}.${f.name}`, f.label ?? f.name])) })); })
            .catch(() => {});
        }
      })
      .catch((e) => {
        if (!live) return;
        const r = refuse(e);
        setPhase({ kind: "error", text: r.text, final: !!(r.hide || r.gone) });
      });
    return () => { live = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- one dry run per opened row / retry
  }, [entity, id, attempt]);

  const doPurge = async () => {
    if (inFlight.current || phase.kind !== "ready") return;
    // The customer changed between the two phases => no second call at all.
    if (!assertScope()) { onClose(); return; }
    inFlight.current = true;
    setSending(true);
    setErr(null);
    try {
      const done = await purgeRecord(entity, id, { dryRun: false });
      if (alive.current) onPurged(done);
    } catch (e) {
      if (!alive.current) return;
      const r = refuse(e);
      setErr(r.text);
      if (r.hide || r.gone) setPhase({ kind: "error", text: r.text, final: true });
      setSending(false);
    } finally {
      inFlight.current = false;
    }
  };

  if (phase.kind === "loading") {
    return <div className="slds-is-relative slds-p-around_medium"><Spinner size="small" label={t("studio.operate.recycle.checking")} /></div>;
  }
  if (phase.kind === "error") {
    return (
      <ScopedNotification theme="error">
        {phase.text}{" "}
        {!phase.final && <Button variant="base" onClick={() => { setPhase({ kind: "loading" }); setAttempt((n) => n + 1); }}>{t("studio.operate.recycle.retry")}</Button>}
        <Button variant="base" onClick={onClose}>{t("studio.confirm.cancel")}</Button>
      </ScopedNotification>
    );
  }
  const s = purgeSummary(phase.report, labels);
  return (
    <InlineConfirm
      tone="error"
      message={tf("studio.operate.recycle.purgeMsg", { label })}
      confirmLabel={t("studio.operate.recycle.purge")}
      onConfirm={() => void doPurge()}
      onCancel={onClose}
      busy={sending}
      confirmDisabled={s.blocked.length > 0}
    >
      <p>{s.headline}</p>
      {s.cleared.map((c) => <p key={c} className="slds-m-top_xx-small">{c}</p>)}
      {s.blobsNote && <p className="slds-m-top_xx-small slds-text-color_weak">{s.blobsNote}</p>}
      {s.blocked.length > 0 && (
        <ScopedNotification theme="warning">
          {s.blocked.map((b) => <span key={b} className="slds-show">{b}</span>)}
          <span className="slds-show">{t("studio.operate.recycle.blockedHint")}</span>
        </ScopedNotification>
      )}
      {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
    </InlineConfirm>
  );
}
