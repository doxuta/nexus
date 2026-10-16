"use client";

// Vận hành › Dữ liệu › one record (specs/078 lát u7-du-lieu-ban-ghi, FR-054..FR-056, FR-132, FR-133): view, create, edit and
// clone, drawn exactly as the customer's staff see them (CustomerPreviewFrame holds LayoutDetail / LayoutForm). The page adds
// only the way back and one H2; every other word is the customer's own screen.
//   • the table name from the URL is checked against the project's table list BEFORE any data call (useKnownEntity);
//   • LayoutDetail gets trackVisit={false}: this browser is shared between customers, so a record label never goes to localStorage;
//   • LayoutForm reads ?cloneFrom= and ?pf_<field>= from the address bar once at mount, so the form is remounted (key) when they change.
import Link from "next/link";
import { LayoutDetail } from "@/components/layout/LayoutDetail";
import { LayoutForm } from "@/components/layout/LayoutForm";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { CustomerPreviewFrame } from "@/components/studio/CustomerPreviewFrame";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { useKnownEntity } from "@/lib/operateData";
import { isMounted, studioHref } from "@/lib/studioRoutes";

export interface RecordPageProps {
  slug: string;
  /** Table name from the URL (already NAME_RE-checked by parseStudioPath). */
  bang: string;
  /** Record id; undefined = the "new" page. */
  id?: string;
  /** ?sua=1 */
  edit?: boolean;
  cloneFrom?: string;
  /** The address-bar query of the new page (pf_ keys, cloneFrom): changing it remounts the form. */
  search?: string;
}

export function RecordPage({ slug, bang, id, edit = false, cloneFrom, search = "" }: RecordPageProps) {
  const router = useGuardedRouter();
  const known = useKnownEntity(slug, bang);
  const tables = studioHref(slug, "van-hanh/du-lieu");
  const list = studioHref(slug, "van-hanh/du-lieu/:bang", { bang });
  const one = (rid: string) => studioHref(slug, "van-hanh/du-lieu/:bang/:id", { bang, id: rid });

  if (known.state === "loading") {
    return (
      <div className="slds-is-relative slds-p-around_xx-large">
        <Spinner size="medium" />
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
      <div className="slds-p-around_large">
        <p className="nx-lede">{t("studio.operate.data.tableNotFound")}</p>
        <p className="slds-m-top_small">
          <Link href={tables}>{`← ${t("studio.operate.data.allTables")}`}</Link>
        </p>
      </div>
    );
  }

  const label = known.label || bang;
  const recycle = isMounted("van-hanh/du-lieu/:bang/thung-rac") ? (
    <Link href={studioHref(slug, "van-hanh/du-lieu/:bang/thung-rac", { bang })}>{t("studio.operate.record.openRecycle")}</Link>
  ) : null;

  return (
    <div className="slds-p-around_medium">
      <p className="slds-m-bottom_x-small">
        <Link href={list}>{`← ${tf("studio.operate.record.back", { name: label })}`}</Link>
      </p>
      <h2 className="slds-text-heading_medium slds-m-bottom_small">{id ? tf("studio.operate.record.title", { name: label }) : t("studio.operate.record.titleNew")}</h2>
      <CustomerPreviewFrame caption={t("studio.operate.record.caption")}>
        {id && !edit ? (
          <LayoutDetail
            key={`${bang}/${id}`}
            entity={bang}
            recordId={id}
            trackVisit={false}
            notFoundNote={
              <>
                {t("studio.operate.record.notFound")} {recycle}
              </>
            }
            onEdit={() => router.push(studioHref(slug, "van-hanh/du-lieu/:bang/:id", { bang, id }, { sua: "1" }))}
            onClone={() => router.push(studioHref(slug, "van-hanh/du-lieu/:bang/moi", { bang }, { cloneFrom: id }))}
            onDeleted={() => router.push(list)}
          />
        ) : (
          <LayoutForm
            key={`${bang}/${id ?? "new"}/${cloneFrom ?? ""}/${search}`}
            entity={bang}
            recordId={id}
            onSaved={(rid) => router.push(one(rid))}
            onCancel={() => router.push(list)}
          />
        )}
      </CustomerPreviewFrame>
    </div>
  );
}
