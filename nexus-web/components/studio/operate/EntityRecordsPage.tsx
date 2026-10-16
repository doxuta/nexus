"use client";

// Vận hành › Dữ liệu › <bảng>: the customer's record list, embedded (specs/078 lát u7-du-lieu-danh-sach-bang, FR-050..FR-055).
// The page draws ONLY the "← Tất cả bảng" row; the H2, "{n} bản ghi" and the actions (Tạo mới, Nhập CSV, Tạo nhiều,
// Xuất CSV, Thùng rác) come from LayoutList through the embedded ObjectHomePage — drawing them here too would give a
// second heading and doubled buttons. A name that is not a table of this project is answered before any data call.
import Link from "next/link";
import { useRouter } from "next/navigation";
import { LayoutList } from "@/components/layout/LayoutList";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { Button } from "@/components/slds/Button";
import { errorMessage } from "@/lib/errorMessages";
import { t } from "@/lib/i18n";
import { operateListHref, useKnownEntity } from "@/lib/operateData";
import { studioHref } from "@/lib/studioRoutes";

/** Shared by every `du-lieu/<bang>/…` page: renders `children` only once `bang` is a known table. */
export function KnownEntityGate({ slug, bang, children }: { slug: string; bang: string; children: () => React.ReactNode }) {
  const known = useKnownEntity(slug, bang);
  const back = (
    <p className="slds-p-horizontal_medium slds-p-top_medium">
      <Link href={studioHref(slug, "van-hanh/du-lieu")}>{t("studio.operate.data.back")}</Link>
    </p>
  );
  if (known.state === "loading") {
    return (
      <div className="slds-is-relative slds-p-around_xx-large">
        <Spinner size="medium" label={t("studio.operate.data.loading")} />
      </div>
    );
  }
  if (known.state === "error") {
    return (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{errorMessage(known.error)}</ScopedNotification>
        <div className="slds-m-top_small">
          <Button variant="neutral" onClick={() => void known.refetch()}>
            {t("studio.query.retry")}
          </Button>
        </div>
      </div>
    );
  }
  if (known.state === "missing") {
    return (
      <div className="slds-p-around_medium">
        <p className="nx-lede">{t("studio.operate.data.missing")}</p>
        {back}
      </div>
    );
  }
  return (
    <>
      {back}
      {children()}
    </>
  );
}

export function EntityRecordsPage({ slug, bang }: { slug: string; bang: string }) {
  const router = useRouter();
  const newHref = studioHref(slug, "van-hanh/du-lieu/:bang/moi", { bang });
  return (
    <KnownEntityGate slug={slug} bang={bang}>
      {() => (
        <>
          <p className="slds-p-horizontal_medium slds-text-color_weak">{t("studio.operate.data.exportNote")}</p>
          <LayoutList
            entity={bang}
            onCreate={() => router.push(newHref)}
            onOpen={(id) => router.push(studioHref(slug, "van-hanh/du-lieu/:bang/:id", { bang, id }))}
            onNavigate={(h) => {
              const to = operateListHref(slug, bang, h);
              if (to) router.push(to);
            }}
          />
        </>
      )}
    </KnownEntityGate>
  );
}
