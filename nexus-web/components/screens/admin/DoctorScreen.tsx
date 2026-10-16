"use client";

// Khám sức khoẻ — the drift scanner (DOCTOR-CORE, ../specs/008-nexus-doctor/spec.md §2.4; drawn as a checklist by
// specs/079 f7-kham-ve-lai). ONE action scans every derived layer, the report IS the diff, fixes are idempotent so
// recovery = run again. Flow: "Khám sức khoẻ" → POST /admin/doctor/scan (read-only) → one ruled row per kind of drift
// → "Sửa" (a question in the row first when the fix has consequences) → POST /admin/doctor/fix → per-check outcome
// → an automatic second scan: a clean second report is the proof, not the fix response.
import { Activity, Eraser, Wrench, X } from "lucide-react";
import { Fragment, useEffect, useRef, useState } from "react";
import {
  doctorScan, doctorFix, doctorFlushCaches, ensureAuth, ApiError,
  type DoctorFinding, type DoctorScanData, type DoctorFixResult,
} from "@/lib/api";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { DoctorCleanRow, DoctorFindingList } from "@/components/studio/maintain/DoctorFindingList";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { StatusText } from "@/components/studio/StatusText";
import { cleanRow, clockTime, deliveredAdvice, doctorLede, findingRows, fixableIds, type DeliveredState } from "@/lib/doctorView";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

// Vietnamese label + description per check, from the dictionary (page.doctor.check.*). Exported so tests pin the copy.
// Lookup is prefix-tolerant (metaFor) because the BE ships ids like "A1_MISSING_GEN_COLUMN" — unknown future ids
// degrade to the raw id, so new checks render honestly before this map learns them.
const CHECK_IDS = ["A1", "A2A", "A3", "A5", "A6", "B4", "B5", "B1", "B2", "C1", "C2", "C4", "D1", "D2", "D3", "C8", "C9", "C10", "C11", "C12"] as const;
export const CHECK_META: Record<string, { label: string; desc: string }> = Object.fromEntries(
  CHECK_IDS.map((id) => [
    id,
    { label: t(`page.doctor.check.${id}.label` as I18nKey), desc: t(`page.doctor.check.${id}.desc` as I18nKey) },
  ]),
);

export function metaFor(checkId: string): { label: string; desc: string } {
  return (
    CHECK_META[checkId] ??
    CHECK_META[checkId.split("_")[0]] ??
    { label: checkId, desc: t("page.doctor.unknownCheck") }
  );
}

/** `**bold**` markers of the lede sentence → <b>. */
function Lede({ text }: { text: string }) {
  return (
    <>
      {text.split("**").map((part, i) => (i % 2 === 1 ? <b key={i} className="tabular-nums">{part}</b> : <Fragment key={i}>{part}</Fragment>))}
    </>
  );
}

const say = (e: unknown, fallback: I18nKey) => (e instanceof ApiError ? errorMessage(e) : t(fallback));

type Ask = { kind: "one"; finding: DoctorFinding } | { kind: "all" };

export function DoctorScreen({ deliveredState = "unknown" }: { deliveredState?: DeliveredState }) {
  const [scan, setScan] = useState<DoctorScanData | null>(null);
  const [scanning, setScanning] = useState(false);
  // checkIds currently being fixed (null = idle). One batch at a time: every fix button disables.
  const [fixing, setFixing] = useState<Set<string> | null>(null);
  const [fixResults, setFixResults] = useState<DoctorFixResult[] | null>(null);
  // scannedAt of the automatic post-fix re-scan — proves the fix landed (idempotency check).
  const [rescannedAt, setRescannedAt] = useState<string | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [ask, setAsk] = useState<Ask | null>(null);
  const [flushing, setFlushing] = useState(false);
  const [flushMsg, setFlushMsg] = useState<string | null>(null);
  const resultsHead = useRef<HTMLHeadingElement>(null);

  // After a fix the focus lands on the results heading, not on a button that just disappeared.
  useEffect(() => {
    if (fixResults && rescannedAt) resultsHead.current?.focus();
  }, [fixResults, rescannedAt]);

  async function runScan() {
    setScanning(true);
    setErr(null);
    setFixResults(null);
    setRescannedAt(null);
    setAsk(null);
    try {
      await ensureAuth();
      setScan(await doctorScan());
    } catch (e) {
      setErr(say(e, "studio.maintain.kham.scanFailed"));
    } finally {
      setScanning(false);
    }
  }

  async function runFix(checkIds: string[], confirm?: boolean) {
    setFixing(new Set(checkIds));
    setErr(null);
    setAsk(null);
    try {
      const results = await doctorFix(checkIds, confirm);
      setFixResults(results);
      // Scan again RIGHT AWAY. Failures resume by simply fixing again from the fresh report.
      const fresh = await doctorScan();
      setScan(fresh);
      setRescannedAt(fresh.scannedAt);
    } catch (e) {
      setErr(say(e, "studio.maintain.kham.fixFailed"));
    } finally {
      setFixing(null);
    }
  }

  async function runFlush() {
    setFlushing(true);
    setErr(null);
    setFlushMsg(null);
    try {
      await ensureAuth();
      const res = await doctorFlushCaches();
      setFlushMsg(
        res.flushed.length > 0
          ? tf("page.doctor.flushDone", { list: res.flushed.join(", "), note: res.note })
          : tf("page.doctor.flushNone", { note: res.note }),
      );
    } catch (e) {
      setErr(say(e, "studio.maintain.kham.flushFailed"));
    } finally {
      setFlushing(false);
    }
  }

  const rows = scan ? findingRows(scan.findings) : [];
  const safeIds = scan ? fixableIds(scan.findings) : [];
  const advice = deliveredAdvice(deliveredState);
  const busy = fixing !== null;

  // A fix that has consequences (needs confirm) or reaches real users at once asks IN the row first.
  function onFix(f: DoctorFinding) {
    if (f.autoFixSafe && advice === "") void runFix([f.checkId]);
    else setAsk({ kind: "one", finding: f });
  }
  function onConfirm() {
    if (ask?.kind === "one") void runFix([ask.finding.checkId], ask.finding.autoFixSafe ? undefined : true);
    else if (ask?.kind === "all") void runFix(safeIds);
  }
  const askOne = ask?.kind === "one"
    ? {
        checkId: ask.finding.checkId,
        tone: "warning" as const,
        message: [
          ask.finding.autoFixSafe ? "" : tf("studio.maintain.kham.fixAsk", { label: metaFor(ask.finding.checkId).label, desc: metaFor(ask.finding.checkId).desc }),
          advice,
        ].filter(Boolean).join(" "),
      }
    : null;

  return (
    <div className="slds-p-around_medium">
      <p className="nx-lede" aria-live="polite">
        <Lede text={doctorLede(scan)} />
      </p>
      <div className="flex flex-wrap items-center gap-2 slds-m-top_small">
        <Button variant={scan ? "neutral" : "brand"} iconLeft={Activity} onClick={runScan} disabled={busy || flushing || scanning}>
          {scanning ? t("page.doctor.scanning") : t(scan ? "studio.maintain.kham.again" : "studio.maintain.kham.scan")}
        </Button>
        <Button variant="neutral" iconLeft={Eraser} onClick={runFlush} disabled={busy || scanning || flushing}>
          {flushing ? t("studio.maintain.kham.flushing") : t("studio.maintain.kham.flush")}
        </Button>
      </div>

      {err && (
        <div className="slds-m-top_small">
          <ScopedNotification theme="error" role="alert">
            <span className="flex items-start gap-2">
              <span className="min-w-0 flex-1 break-words">{err}</span>
              <ButtonIcon icon={X} label={t("studio.maintain.kham.dismiss")} variant="inverse" onClick={() => setErr(null)} />
            </span>
          </ScopedNotification>
        </div>
      )}

      {/* The honest outcome of "Làm mới bộ nhớ đệm", including the replica note. */}
      <div aria-live="polite">
        {flushMsg && <p className="slds-text-body_small slds-text-color_weak slds-m-top_small">{flushMsg}</p>}
      </div>

      {fixResults && (
        <section className="slds-m-top_large">
          <h3 ref={resultsHead} tabIndex={-1} className="slds-text-heading_small">{t("studio.maintain.kham.results")}</h3>
          <ul className="slds-has-dividers_bottom">
            {fixResults.map((r) => (
              <li key={r.checkId} className="slds-item">
                <div className="flex flex-wrap items-baseline gap-x-3">
                  <span className="slds-text-title_bold">{metaFor(r.checkId).label}</span>
                  <StatusText tone={r.failed > 0 ? "error" : "success"}>
                    {tf("studio.maintain.kham.resultLine", { fixed: r.fixed, skipped: r.skipped, failed: r.failed })}
                  </StatusText>
                </div>
                {r.errors.length > 0 && (
                  <ul className="slds-list_dotted slds-text-color_error slds-text-body_small">
                    {r.errors.map((msg, i) => <li key={i} className="break-words">{msg}</li>)}
                  </ul>
                )}
              </li>
            ))}
          </ul>
          {rescannedAt && <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{tf("page.doctor.rescanned", { time: clockTime(rescannedAt) })}</p>}
        </section>
      )}

      {scanning && !scan && (
        <div className="slds-is-relative slds-p-around_xx-large"><Spinner size="medium" /></div>
      )}

      {scan && (
        <section className="slds-m-top_large">
          {rows.length === 0 ? (
            <DoctorCleanRow text={cleanRow(scan, CHECK_IDS.length) ?? ""} />
          ) : (
            <>
              {safeIds.length > 0 && (
                <div className="slds-m-bottom_small">
                  <Button variant="neutral" iconLeft={Wrench} disabled={busy} onClick={() => (advice === "" ? void runFix(safeIds) : setAsk({ kind: "all" }))}>
                    {busy ? t("page.doctor.fixing") : t("studio.maintain.kham.fixAll")}
                  </Button>
                  {ask?.kind === "all" && (
                    <div className="slds-m-top_small">
                      <InlineConfirm
                        tone="warning"
                        message={`${tf("studio.maintain.kham.fixAllAsk", { n: safeIds.length })} ${advice}`}
                        confirmLabel={tf("studio.maintain.kham.fixAllOk", { n: safeIds.length })}
                        onConfirm={onConfirm}
                        onCancel={() => setAsk(null)}
                        busy={busy}
                      />
                    </div>
                  )}
                </div>
              )}
              <DoctorFindingList
                rows={rows}
                metaFor={metaFor}
                busy={busy}
                fixing={fixing}
                ask={askOne}
                onFix={onFix}
                onConfirm={onConfirm}
                onCancel={() => setAsk(null)}
              />
            </>
          )}
        </section>
      )}
    </div>
  );
}
