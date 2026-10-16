"use client";

// Vận hành › Dữ liệu › <bảng> › Nhập CSV / Tạo nhiều (specs/078 lát u7-du-lieu-nhap-csv, FR-060..FR-062).
// The customer's own wizard and batch grid, mounted on the work surface: they leave through the Studio router
// (never window.location / the customer's /<bang> route) and, without the create grant, are replaced by a notice.
import { useRouter } from "next/navigation";
import { BatchCreateForm } from "@/components/layout/BatchCreateForm";
import { ImportScreen } from "@/components/screens/ImportScreen";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { t } from "@/lib/i18n";
import { usePermissions } from "@/lib/metadata/permissions";
import { studioHref } from "@/lib/studioRoutes";

function CreateGate({ bang, children }: { bang: string; children: React.ReactNode }) {
  const { can, isLoading } = usePermissions();
  if (isLoading) return null;
  if (!can("create", bang)) {
    return (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="warning">{t("studio.operate.noCreate")}</ScopedNotification>
      </div>
    );
  }
  return <>{children}</>;
}

const listHref = (slug: string, bang: string) => studioHref(slug, "van-hanh/du-lieu/:bang", { bang });

export function OperateImport({ slug, bang }: { slug: string; bang: string }) {
  const router = useRouter();
  const back = () => router.push(listHref(slug, bang));
  return (
    <CreateGate bang={bang}>
      <ImportScreen entity={bang} onDone={back} onCancel={back} />
    </CreateGate>
  );
}

export function OperateBatch({ slug, bang }: { slug: string; bang: string }) {
  const router = useRouter();
  const back = () => router.push(listHref(slug, bang));
  return (
    <CreateGate bang={bang}>
      {/* onDone fires after every submit; leave only when every row landed, so failed rows stay on screen. */}
      <BatchCreateForm entity={bang} onCancel={back} onDone={(r) => { if (r.failed === 0) back(); }} />
    </CreateGate>
  );
}
