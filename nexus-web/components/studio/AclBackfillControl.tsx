"use client";

// AclBackfillControl — extracted from AppOverview (specs/073 kh3 T233) so ReadinessChecklist no longer imports the
// screen that kh7 deletes. Same two-step contract as before; only the home changed.
import { useState } from "react";
import { ShieldCheck } from "lucide-react";
import { appAclBackfill, type AclBackfillResult } from "@/lib/api";
import { t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { useDirectApply } from "@/components/shell/useDirectApply";
import { InlineConfirm } from "./InlineConfirm";

const fill = (template: string, values: Record<string, string | number>) =>
  template.replace(/\{(\w+)\}/g, (m, k: string) => (k in values ? String(values[k]) : m));

// ── AclBackfillControl — POST /admin/apps/:id/acl-backfill (TRỤ 2 mục 4 gap-closer) ─────────
// Founder chose "backfill tất cả một lần" NHƯNG phải NHÌN THẤY trước khi làm: bấm → dryRun=true
// → modal liệt kê ĐÚNG danh sách sẽ cấp quyền → xác nhận → chạy thật → kết quả thật
// inline, với TỪNG dòng failed kèm reason (cấm nuốt im lặng — đây là hành động mở rộng quyền).
// Pinned by AclBackfillControl.test.tsx. Chạy lại an toàn: seed idempotent (AddGrants không nhân đôi).
export function AclBackfillControl({
  appId, appLabel, entityLabel = (n) => n, onApplied,
}: {
  appId: string;
  /** Several controls on one page (one per app): the name makes each button's accessible name distinct. */
  appLabel?: string;
  entityLabel?: (name: string) => string;
  onApplied?: () => void;
}) {
  const [previewing, setPreviewing] = useState(false);
  const [preview, setPreview] = useState<AclBackfillResult | null>(null); // non-null = dialog open
  const [applying, setApplying] = useState(false);
  const [result, setResult] = useState<AclBackfillResult | null>(null);
  const [errMsg, setErrMsg] = useState("");
  const { delivered, notice } = useDirectApply({ inline: true });

  const startPreview = async () => {
    if (previewing) return;
    setPreviewing(true);
    setErrMsg("");
    setResult(null);
    try {
      setPreview(await appAclBackfill(appId, true)); // BƯỚC 1: dry-run, zero ghi
    } catch (e) {
      setErrMsg(e instanceof Error ? e.message : t("page.appOverview.acl.errPreview"));
    } finally {
      setPreviewing(false);
    }
  };

  const apply = async () => {
    setApplying(true);
    setErrMsg("");
    try {
      const r = await appAclBackfill(appId, false); // BƯỚC 2: chạy thật, sau xác nhận
      setResult(r);
      setPreview(null);
      if (r.failed.length === 0) {
        toast.success(fill(t("page.appOverview.acl.toastOk"), { n: r.seeded.length }));
      } else {
        toast.warning(fill(t("page.appOverview.acl.toastPartial"), { ok: r.seeded.length, fail: r.failed.length }));
      }
      onApplied?.(); // invalidate readiness → bảng kiểm tự cập nhật
    } catch (e) {
      setErrMsg(e instanceof Error ? e.message : t("page.appOverview.acl.errApply"));
      setPreview(null);
    } finally {
      setApplying(false);
    }
  };

  return (
    <>
      <Button
        variant="base" iconLeft={ShieldCheck} onClick={() => void startPreview()} disabled={previewing}
        aria-label={appLabel ? `${t("page.appOverview.acl.button")} — ${appLabel}` : undefined}
      >
        {t("page.appOverview.acl.button")}
      </Button>
      {previewing && <Spinner size="xx-small" inline />}

      {/* Asked in the page, not in a dialog (specs/074 FR-065); the list below is exactly what Cấp quyền will write. */}
      {preview !== null && (
        <div className="w-full">
          <InlineConfirm
            tone="warning"
            message={
              preview.seeded.length > 0
                ? fill(t("page.appOverview.acl.desc"), { n: preview.seeded.length })
                : t("page.appOverview.acl.none")
            }
            confirmLabel={t("page.appOverview.acl.confirm")}
            cancelLabel={t("page.appOverview.acl.cancel")}
            busy={applying}
            onCancel={() => setPreview(null)}
            onConfirm={() => void apply()}
          >
            {delivered && (
              <p className="slds-m-bottom_small">{notice} <strong>{t("direct.notice")}</strong></p>
            )}
            {(preview.seeded.length > 0 || preview.skipped.length > 0) && (
              <ul className="slds-list_dotted">
                {preview.seeded.map((name) => (
                  <li key={name}>{fill(t("page.appOverview.acl.row"), { label: entityLabel(name) })}</li>
                ))}
                {preview.skipped.length > 0 && (
                  <li>
                    {t("page.appOverview.acl.skipped")} <Badge variant="lightest">{preview.skipped.length}</Badge>
                  </li>
                )}
              </ul>
            )}
          </InlineConfirm>
        </div>
      )}

      {errMsg && (
        <div className="w-full">
          <ScopedNotification theme="error">{errMsg}</ScopedNotification>
        </div>
      )}
      {result && (
        <div className="w-full">
          <ScopedNotification theme={result.failed.length > 0 ? "warning" : "light"}>
            {fill(t("page.appOverview.acl.result"), { n: result.seeded.length })}
            {result.skipped.length > 0 && fill(t("page.appOverview.acl.resultSkipped"), { n: result.skipped.length })}
            {result.failed.length > 0 && fill(t("page.appOverview.acl.resultFailed"), { n: result.failed.length })}.
            {/* Cấm nuốt im lặng: từng dòng lỗi hiện nguyên văn kèm lý do server trả về. */}
            {result.failed.map((f) => (
              <span key={f.entity} className="slds-show slds-m-top_xx-small">
                ✗ {entityLabel(f.entity)}: {f.reason}
              </span>
            ))}
          </ScopedNotification>
        </div>
      )}
    </>
  );
}
