"use client";

// BackupScheduleForm — "Lịch tự sao lưu" of one tenant (specs/079 f7-sao-luu-lich-xoa): a toggle whose hour is the
// server's real `hourLocal`, how many scheduled copies to keep (1–30, checked here and again by the server), and
// when the last/next one ran/is due. Saved explicitly with "Lưu". Draws nothing when the engine has no schedule route.
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Toggle } from "@/components/slds/Toggle";
import { toast } from "@/components/ui/Toaster";
import { ApiError, getTenantBackupSchedule, putTenantBackupSchedule, type TenantBackupSchedule } from "@/lib/api";
import { isEngineMissing } from "@/lib/engineGap";
import { formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { maintainErrorText } from "@/lib/maintainErrors";
import { useQuery } from "@tanstack/react-query";

const KEEP_MIN = 1;
const KEEP_MAX = 30;

export function BackupScheduleForm({ tenantId }: { tenantId: string }) {
  const key = ["backup-schedule", tenantId];
  const q = useQuery({ queryKey: key, queryFn: () => getTenantBackupSchedule(tenantId), retry: false });
  if (q.isError) {
    return isEngineMissing(q.error) ? null : <ScopedNotification theme="error">{t("backup.schedule.loadFailed")}</ScopedNotification>;
  }
  if (!q.data) return null;
  // keyed by the saved values: a refetch after save resets the draft to what the server holds.
  return <Body key={`${q.data.enabled}-${q.data.keepScheduled}`} s={q.data} tenantId={tenantId} queryKey={key} />;
}

function Body({ s, tenantId, queryKey }: { s: TenantBackupSchedule; tenantId: string; queryKey: unknown[] }) {
  const qc = useQueryClient();
  const [enabled, setEnabled] = useState(s.enabled);
  const [keep, setKeep] = useState(String(s.keepScheduled));
  const [busy, setBusy] = useState(false);
  const [fieldError, setFieldError] = useState<string | null>(null);
  const [problem, setProblem] = useState<string | null>(null);

  async function save() {
    const n = Number(keep);
    if (!Number.isInteger(n) || n < KEEP_MIN || n > KEEP_MAX) {
      setFieldError(t("studio.maintain.err.BACKUP_SCHEDULE_INVALID"));
      return;
    }
    setFieldError(null);
    setProblem(null);
    setBusy(true);
    try {
      qc.setQueryData(queryKey, await putTenantBackupSchedule(tenantId, enabled, n));
      toast.success(t("backup.schedule.saved"));
    } catch (e) {
      if (e instanceof ApiError && e.code === "BACKUP_SCHEDULE_INVALID") setFieldError(maintainErrorText(e));
      else setProblem(maintainErrorText(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="flex flex-col gap-2" aria-label={t("studio.maintain.schedule.title")}>
      <h2 className="slds-text-heading_medium">{t("studio.maintain.schedule.title")}</h2>
      <Toggle
        label={tf("backup.schedule.toggle", { hour: String(s.hourLocal).padStart(2, "0") })}
        help={t("backup.schedule.toggleHelp")}
        checked={enabled}
        disabled={busy}
        onChange={(e) => setEnabled(e.target.checked)}
      />
      <div className="flex flex-wrap items-end gap-3">
        <Input
          label={t("backup.schedule.keep")}
          type="number"
          min={KEEP_MIN}
          max={KEEP_MAX}
          value={keep}
          disabled={busy}
          error={fieldError ?? undefined}
          onChange={(e) => setKeep(e.target.value)}
        />
        <Button variant="brand" disabled={busy} onClick={() => void save()}>{t("studio.maintain.schedule.save")}</Button>
      </div>
      {problem && <ScopedNotification theme="error">{problem}</ScopedNotification>}
      <p className="slds-text-body_small slds-text-color_weak">
        {s.lastScheduledAt ? tf("backup.schedule.last", { time: formatDateTime(s.lastScheduledAt) }) : t("backup.schedule.never")}
        {s.enabled && s.nextDueAt && ` · ${tf("backup.schedule.next", { time: formatDateTime(s.nextDueAt) })}`}
      </p>
    </section>
  );
}
