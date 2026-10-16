"use client";

// MaintenanceControl — "Khoá ghi" for one customer (specs/079 f7-sao-luu-khoa-ghi, spec 068 slice B). Customer staff can
// still read but cannot save while locked. Lock and unlock both go through InlineConfirm; the deadline comes from the
// backup list's meta (no extra call). Never drawn for the home workshop (the server answers 422 MAINTENANCE_SELF).
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { clearTenantMaintenance, setTenantMaintenance } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { maintainErrorText } from "@/lib/maintainErrors";
import { InlineConfirm } from "../InlineConfirm";

const MIN = 1;
const MAX = 120;

export function MaintenanceControl({ tenantId, isHome, until, onChanged }: { tenantId: string; isHome: boolean; until: string | null; onChanged: () => void }) {
  const qc = useQueryClient();
  const [minutes, setMinutes] = useState("30");
  const [step, setStep] = useState<"idle" | "lock" | "unlock">("idle");
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const [touched, setTouched] = useState(false);
  // the deadline passes by itself: re-render at that moment so the line disappears without a reload
  const [expired, setExpired] = useState<string | null>(null);
  useEffect(() => {
    if (!until) return;
    const ms = new Date(until).getTime() - Date.now();
    const id = setTimeout(() => setExpired(until), Math.min(2 ** 30, Math.max(0, ms)));
    return () => clearTimeout(id);
  }, [until]);
  if (isHome) return null;

  const n = /^\d+$/.test(minutes.trim()) ? Number(minutes) : NaN;
  const valid = n >= MIN && n <= MAX;
  const locked = !!until && until !== expired;

  async function run(fn: () => Promise<unknown>) {
    setBusy(true);
    setProblem(null);
    try {
      await fn();
      setStep("idle");
      onChanged();
      void qc.invalidateQueries({ queryKey: ["projects"] });
    } catch (e) {
      setStep("idle");
      setProblem(maintainErrorText(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="flex flex-col gap-2" aria-labelledby="mc-title">
      <h3 id="mc-title" className="slds-text-heading_small">{t("studio.maintain.maintenanceLock.title")}</h3>
      <p className="slds-text-color_weak">{t("studio.maintain.maintenanceLock.lede")}</p>
      {problem && <ScopedNotification theme="error">{problem}</ScopedNotification>}
      {step === "lock" ? (
        <InlineConfirm
          tone="warning"
          message={tf("studio.maintain.maintenanceLock.confirmLock", { n })}
          confirmLabel={tf("studio.maintain.maintenanceLock.confirmLockYes", { n })}
          busy={busy}
          onCancel={() => setStep("idle")}
          onConfirm={() => void run(() => setTenantMaintenance(tenantId, n))}
        />
      ) : step === "unlock" ? (
        <InlineConfirm
          tone="info"
          message={t("studio.maintain.maintenanceLock.confirmUnlock")}
          confirmLabel={t("studio.maintain.maintenanceLock.confirmUnlockYes")}
          busy={busy}
          onCancel={() => setStep("idle")}
          onConfirm={() => void run(() => clearTenantMaintenance(tenantId))}
        />
      ) : locked ? (
        <div className="flex items-center gap-3">
          <p>{tf("studio.maintain.maintenanceLock.locked", { time: formatTime(until as string) })}</p>
          <Button variant="neutral" onClick={() => setStep("unlock")}>{t("studio.maintain.maintenanceLock.unlock")}</Button>
        </div>
      ) : (
        <form
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            setTouched(true);
            if (valid) setStep("lock");
          }}
        >
          <Input
            label={t("studio.maintain.maintenanceLock.minutes")}
            inputMode="numeric"
            value={minutes}
            error={touched && !valid ? t("studio.maintain.err.MAINTENANCE_MINUTES_INVALID") : undefined}
            onChange={(e) => setMinutes(e.target.value)}
          />
          <Button type="submit" variant="neutral">{t("studio.maintain.maintenanceLock.lock")}</Button>
        </form>
      )}
    </section>
  );
}
