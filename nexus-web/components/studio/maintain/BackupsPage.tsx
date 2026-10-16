"use client";

// BackupsPage — Bảo trì › Sao lưu (bao-tri/sao-luu, ctx `nha`; specs/079 f7-sao-luu-bang, spec 068 FE).
// The platform operator's list of ONE tenant's backups: "Sao lưu ngay" in place, a table that updates itself every
// 3 s while a backup runs (and stops when it is done, failed or the tab is hidden), and "Tải" for the zip. The tenant
// id comes from useBackupTenant, never from the URL. Nothing here may write into a customer's build session: the route
// is `nha` and only an operator token reaches it. "Khôi phục" / "Hoàn tác" go to the restore page (f7-khoi-phuc-chay). The downloaded Blob is never kept in a cache or storage.
import { useRouter } from "next/navigation";
import { useQueryClient } from "@tanstack/react-query";
import { HardDriveDownload } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { toast } from "@/components/ui/Toaster";
import { createTenantBackup, deleteTenantBackup, downloadTenantBackup, listTenantBackups, type TenantBackup } from "@/lib/api";
import { isEngineMissing } from "@/lib/engineGap";
import { formatBytes } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { maintainErrorText } from "@/lib/maintainErrors";
import { backupFileStem } from "@/lib/maintenance";
import { studioHref } from "@/lib/studioRoutes";
import { BackupScheduleForm } from "./BackupScheduleForm";
import { BackupCreateForm } from "./BackupCreateForm";
import { BackupTable } from "./BackupTable";
import { MaintenanceControl } from "./MaintenanceControl";
import { useBackupTenant, useHomeQuery, type BackupTenant } from "./useBackupTenant";

const POLL_MS = 3000;
const PAGE = 20;

export function BackupsPage({ slug }: { slug: string }) {
  const bt = useBackupTenant(slug);
  if (bt.state === "loading") return <div className="slds-is-relative slds-p-around_xx-large"><Spinner size="medium" /></div>;
  if (bt.state === "error") return <div className="slds-p-around_medium"><ScopedNotification theme="error">{t("studio.gate.cannotConfirm")}</ScopedNotification></div>;
  if (bt.state === "missing") return <div className="slds-p-around_medium"><p className="nx-lede">{t("studio.maintain.backup.missingProject")}</p></div>;
  // keyed by tenant: another project starts from an empty page, never the previous list.
  return <BackupsBody key={bt.tenantId} bt={bt} />;
}

function BackupsBody({ bt }: { bt: BackupTenant }) {
  const qc = useQueryClient();
  const tenantId = bt.tenantId as string;
  const listKey = ["home", bt.slug, tenantId, "backups"];
  const list = useHomeQuery(bt, ["backups"], (id) => listTenantBackups(id, { limit: PAGE }), {
    refetchInterval: (q) => ((q.state.data as { data: TenantBackup[] } | undefined)?.data.some((b) => b.status === "running") ? POLL_MS : false),
  });
  const [more, setMore] = useState<TenantBackup[]>([]);
  const [cursor, setCursor] = useState<string | null | undefined>(undefined);
  const [form, setForm] = useState(false);
  const [creating, setCreating] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const [downloadingId, setDownloadingId] = useState<string | null>(null);
  const router = useRouter();
  const createBtn = useRef<HTMLButtonElement>(null);
  const abort = useRef<AbortController | null>(null);
  useEffect(() => () => abort.current?.abort(), []);

  const first = list.data?.data ?? [];
  const rows = [...first, ...more.filter((m) => !first.some((f) => f.id === m.id))];
  const next = cursor === undefined ? list.data?.meta.nextCursor ?? null : cursor;

  async function create(note: string) {
    setCreating(true);
    setProblem(null);
    try {
      await createTenantBackup(tenantId, note);
      setForm(false);
      await qc.invalidateQueries({ queryKey: listKey });
      void qc.invalidateQueries({ queryKey: ["projects"] });
      requestAnimationFrame(() => createBtn.current?.focus());
    } catch (e) {
      setProblem(maintainErrorText(e));
    } finally {
      setCreating(false);
    }
  }

  async function download(b: TenantBackup) {
    abort.current?.abort();
    const ac = new AbortController();
    abort.current = ac;
    setDownloadingId(b.id);
    setProblem(null);
    try {
      // the name is built from the code of THIS page at the moment of the click (FR-103)
      const { sizeBytes } = await downloadTenantBackup(tenantId, b.id, `${backupFileStem(bt.slug, b.createdAt)}.zip`, ac.signal);
      toast.success(tf("studio.maintain.backup.downloaded", { size: formatBytes(sizeBytes) }));
    } catch (e) {
      if (!ac.signal.aborted) setProblem(maintainErrorText(e));
    } finally {
      if (abort.current === ac) setDownloadingId(null);
    }
  }

  async function remove(b: TenantBackup) {
    setProblem(null);
    try {
      await deleteTenantBackup(tenantId, b.id);
      setMore((m) => m.filter((x) => x.id !== b.id));
      await qc.invalidateQueries({ queryKey: listKey });
      toast.success(t("studio.maintain.backup.deleted"));
      requestAnimationFrame(() => createBtn.current?.focus());
    } catch (e) {
      setProblem(maintainErrorText(e));
    }
  }

  async function loadMore() {
    if (!next) return;
    try {
      const r = await listTenantBackups(tenantId, { limit: PAGE, after: next });
      setMore((m) => [...m, ...r.data.filter((x) => !m.some((p) => p.id === x.id))]);
      setCursor(r.meta.nextCursor);
    } catch (e) {
      setProblem(maintainErrorText(e));
    }
  }

  if (list.isError && isEngineMissing(list.error)) {
    return <div className="slds-p-around_medium"><p className="nx-lede">{t("studio.maintain.backup.engineMissing")}</p></div>;
  }

  return (
    <div className="slds-p-around_medium flex flex-col gap-3">
      <p className="nx-lede">{t("studio.maintain.backup.lede")}</p>
      <div className="flex items-center gap-3">
        <h2 className="slds-text-heading_medium">{t("studio.maintain.backup.title")}</h2>
        {!form && (
          <Button ref={createBtn} className="ml-auto" variant="brand" iconLeft={HardDriveDownload} onClick={() => setForm(true)}>
            {t("backup.create")}
          </Button>
        )}
      </div>
      {form && <BackupCreateForm busy={creating} onSubmit={(n) => void create(n)} onCancel={() => setForm(false)} />}
      {problem && <ScopedNotification theme="error">{problem}</ScopedNotification>}
      {list.isPending ? (
        <div className="slds-is-relative slds-p-around_xx-large"><Spinner size="medium" /></div>
      ) : list.isError ? (
        <div>
          <ScopedNotification theme="error">{maintainErrorText(list.error)}</ScopedNotification>
          <div className="slds-m-top_small"><Button variant="neutral" onClick={() => void list.refetch()}>{t("studio.query.retry")}</Button></div>
        </div>
      ) : rows.length === 0 ? (
        <div>
          <h3 className="slds-text-heading_small">{t("backup.empty.title")}</h3>
          <p className="slds-text-color_weak">{t("backup.empty.desc")}</p>
        </div>
      ) : (
        <>
          <BackupTable rows={rows} downloadingId={downloadingId} onDownload={(b) => void download(b)} onRestore={(b) => router.push(studioHref(bt.slug, "bao-tri/sao-luu/khoi-phuc/:id", { id: b.id }))} onDelete={(b) => void remove(b)} />
          {next && <div><Button variant="neutral" onClick={() => void loadMore()}>{t("studio.maintain.backup.moreAfter")}</Button></div>}
        </>
      )}
      <MaintenanceControl tenantId={tenantId} isHome={bt.isHome} until={list.data?.meta.maintenanceUntil ?? null} onChanged={() => void qc.invalidateQueries({ queryKey: listKey })} />
      <BackupScheduleForm tenantId={tenantId} />
      <p className="slds-text-body_small slds-text-color_weak">{t("studio.maintain.backup.privacy")}</p>
    </div>
  );
}
