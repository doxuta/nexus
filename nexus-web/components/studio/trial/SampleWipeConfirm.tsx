"use client";

// In-page confirmation of "Xoá dữ liệu mẫu" (specs/077 f5-sample-wipe, risk 15). It SNAPSHOTS the counts when it
// opens and sends that number as `expectTotal`: a live re-read (window focus, invalidate) while the box is open only
// adds a "changed" line, it never rewrites the number being confirmed — otherwise the server's 409 guard could
// never fire and rows the builder had not seen would be hard-deleted. A 409 re-snapshots and stays open.
import { useRef, useState } from "react";
import { ApiError, getSampleDataSummary, wipeSampleData, type SampleSummary, type SampleWipeResult } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { InlineConfirm } from "../InlineConfirm";

const TOP = 5;

interface Snapshot {
  total: number;
  binned: number;
  rows: { entity: string; label: string; count: number }[];
}

/** Counts of the sample rows in scope (`scope` = one table; undefined = every table in `summary`). */
function snap(summary: SampleSummary, scope?: string): Snapshot {
  const rows = summary.entities
    .filter((e) => e.count > 0 && (!scope || e.entity === scope))
    .map((e) => ({ entity: e.entity, label: e.label || e.entity, count: e.count, inBin: e.inBin }));
  return {
    total: rows.reduce((n, r) => n + r.count, 0),
    binned: rows.reduce((n, r) => n + r.inBin, 0),
    rows: rows.sort((a, b) => b.count - a.count),
  };
}

export interface SampleWipeConfirmProps {
  /** Latest read of the tally (may change while the box is open). */
  summary: SampleSummary;
  /** Tables of the app; a whole-app wipe sends exactly the tables that hold sample rows. */
  entityNames: string[];
  /** One table only; omitted = every table of the app. */
  scope?: string;
  onDone: (result: SampleWipeResult) => void;
  onCancel: () => void;
}

export function SampleWipeConfirm({ summary, entityNames, scope, onDone, onCancel }: SampleWipeConfirmProps) {
  const [shot, setShot] = useState(() => snap(summary, scope));
  const [message, setMessage] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const inFlight = useRef(false);
  const changed = snap(summary, scope).total !== shot.total;

  const run = async () => {
    if (inFlight.current) return; // a double click is one call
    inFlight.current = true;
    setBusy(true);
    setMessage(null);
    try {
      const entities = scope ? [scope] : shot.rows.map((r) => r.entity);
      onDone(await wipeSampleData(shot.total, entities));
    } catch (e) {
      const code = e instanceof ApiError ? e.code : "";
      if (code === "SAMPLE_WIPE_STALE") {
        // Nothing was deleted: re-take the numbers and let the builder look again.
        try {
          setShot(snap(await getSampleDataSummary(entityNames), scope));
        } catch {
          /* the next confirm earns the 409 again */
        }
        setMessage(t("studio.build.trial.wipe.stale"));
      } else if (code === "SAMPLE_BUSY") setMessage(t("studio.build.trial.wipe.busy"));
      else if (code === "SAMPLE_WIPE_CONFIRM_REQUIRED") setMessage(t("studio.build.trial.wipe.required"));
      else setMessage(t("studio.build.trial.wipe.error"));
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  };

  if (shot.total === 0)
    return (
      <div>
        <InlineConfirm tone="info" message={t("studio.build.trial.wipe.nothing")} confirmLabel={t("studio.build.trial.wipe.open")} confirmDisabled onConfirm={() => {}} onCancel={onCancel} />
      </div>
    );

  const head = shot.rows.slice(0, TOP);
  const more = shot.rows.length - head.length;
  const question =
    scope && shot.rows.length === 1
      ? tf("studio.build.trial.wipe.confirmOne", { n: shot.total, table: shot.rows[0].label, m: shot.binned })
      : tf("studio.build.trial.wipe.confirm", { n: shot.total, k: shot.rows.length, m: shot.binned });
  return (
    <div>
      <InlineConfirm
        tone="error"
        message={question}
        confirmLabel={busy ? t("studio.build.trial.wipe.running") : tf("studio.build.trial.wipe.run", { n: shot.total })}
        onConfirm={() => void run()}
        onCancel={onCancel}
        busy={busy}
      >
        <ul className="slds-has-dividers_bottom">
          {head.map((r) => (
            <li key={r.entity} className="slds-item" data-testid={`sample-wipe-row-${r.entity}`}>
              {tf("studio.build.trial.wipe.row", { table: r.label, n: r.count })}
            </li>
          ))}
          {more > 0 && <li className="slds-item slds-text-color_weak">{tf("studio.build.trial.wipe.more", { n: more })}</li>}
        </ul>
        {changed && !message && <p className="slds-m-top_x-small slds-text-color_weak">{t("studio.build.trial.wipe.changed")}</p>}
        {message && (
          <p role="alert" className="slds-m-top_x-small slds-text-color_error">
            {message}
          </p>
        )}
      </InlineConfirm>
    </div>
  );
}
