"use client";

// VersionPage — bao-tri/lich-su/:id (specs/079 f7-lich-su-dong-thoi-gian, US5): one published version with what each
// document looked like before and after. The "Loại" / "Bảng" filters live in the URL (?loai= ?bang=), so a link keeps them.
// "Bảng" only shows when the server tells which table a document belongs to. A path the stored copies cannot answer is
// still listed (under "Chi tiết"), never dropped. Secrets arrive masked and are masked again by key name (lib/versionDiff).
import Link from "next/link";
import { useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { Button } from "@/components/slds/Button";
import { Select } from "@/components/slds/Select";
import { EmptyState } from "@/components/slds/EmptyState";
import { Spinner } from "@/components/slds/Spinner";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { ApiError, type PublishVersionDoc } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { docChangeRows, docTypeLabel, filterDocs, type ChangeRow } from "@/lib/versionDiff";
import { StatusText } from "../StatusText";
import { usePublishState } from "../usePublishState";
import { RollbackPlanConfirm } from "./RollbackPlanConfirm";
import { SchemaRetry } from "./SchemaRetry";
import { usePublishVersion } from "./queries";

const MARK_WORD = { "+": "studio.publish.mark.add", "~": "studio.publish.mark.change", "−": "studio.publish.mark.remove", "=": "studio.publish.mark.same" } as const;
const show = (v: unknown) => (v === undefined ? t("studio.maintain.version.empty") : typeof v === "string" ? v : JSON.stringify(v));

function DocBlock({ doc }: { doc: PublishVersionDoc }) {
  const rows = docChangeRows(doc);
  const known = rows.filter((r) => r.found);
  const lost = rows.filter((r) => !r.found);
  return (
    <section aria-label={doc.label} className="slds-m-top_medium">
      <p>
        <span className="font-semibold">{doc.label}</span> <span className="slds-text-color_weak">· {docTypeLabel(doc.docType)}</span>{" "}
        {doc.applyStatus === "failed" && <StatusText tone="error">{tf("studio.maintain.version.applyFailed", { msg: doc.applyError ?? "" })}</StatusText>}
        {doc.applyStatus === "pending" && <StatusText tone="warn">{t("studio.maintain.version.applyPending")}</StatusText>}
      </p>
      {known.length > 0 && <Rows rows={known} />}
      {lost.length > 0 && (
        <details className="slds-m-top_x-small">
          <summary>{t("studio.maintain.version.details")}</summary>
          <ul>{lost.map((r) => <li key={r.path} className="break-all"><code>{r.path}</code> <span className="slds-text-color_weak">{t("studio.maintain.version.pathMissing")}</span></li>)}</ul>
        </details>
      )}
    </section>
  );
}

function Rows({ rows }: { rows: ChangeRow[] }) {
  return (
    <div className="slds-scrollable_x">
      <table className="slds-table slds-table_cell-buffer slds-table_bordered">
        <thead>
          <tr className="slds-line-height_reset">
            <th scope="col" />
            <th scope="col">{t("studio.maintain.version.before")}</th>
            <th scope="col">{t("studio.maintain.version.after")}</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.path} className={r.mark === "=" ? "slds-text-color_weak" : undefined}>
              <th scope="row" className="break-all">
                <span aria-hidden="true" className="slds-m-right_x-small">{r.mark}</span>
                <span className="slds-assistive-text">{t(MARK_WORD[r.mark])}</span>
                <code className="slds-text-body_small">{r.path}</code>
              </th>
              <td className="break-words">{show(r.before)}</td>
              <td className="break-words">{show(r.after)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function VersionPage({ slug, id, loai, bang }: { slug: string; id: string; loai?: string; bang?: string }) {
  const router = useRouter();
  const q = usePublishVersion(slug, id);
  const state = usePublishState(slug);
  const [rolling, setRolling] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const back = <p className="slds-m-bottom_small"><Link href={studioHref(slug, "bao-tri/lich-su")}>{`← ${t("studio.maintain.version.back")}`}</Link></p>;
  if (q.isPending) return <div className="slds-is-relative slds-p-around_xx-large"><Spinner size="medium" /></div>;
  if (q.isError) {
    if (q.error instanceof ApiError && q.error.code === "VERSION_NOT_FOUND") {
      return <div className="slds-p-around_medium">{back}<EmptyState plain title={t("studio.maintain.version.notFound")} /></div>;
    }
    return <div className="slds-p-around_medium">{back}<ScopedNotification theme="error">{errorMessage(q.error)}</ScopedNotification></div>;
  }
  const v = q.data;
  const docs = v.docs ?? [];
  const types = [...new Set(docs.map((d) => d.docType))];
  const tables = [...new Set(docs.map((d) => (d as { entityName?: string }).entityName).filter((x): x is string => !!x))];
  const shown = filterDocs(docs, { loai, bang });
  const go = (next: { loai?: string; bang?: string }) =>
    router.replace(studioHref(slug, "bao-tri/lich-su/:id", { id }, { loai: next.loai || undefined, bang: next.bang || undefined }));
  const title = v.versionNo == null ? t("studio.maintain.version.immediateTitle") : tf("studio.maintain.version.title", { n: v.versionNo });
  return (
    <div className="slds-p-around_medium">
      {back}
      <h2 className="slds-text-heading_medium">{title}</h2>
      <p className="slds-text-color_weak">
        {v.createdByLabel || v.createdByName} · <time dateTime={v.createdAt}>{new Date(v.createdAt).toLocaleString("vi-VN")}</time>
        {v.kind === "rollback" && v.rollbackOf != null && ` · ${tf("studio.maintain.version.rollbackFrom", { n: v.rollbackOf })}`}
      </p>
      {v.note && <p className="slds-m-top_x-small break-words">{v.note}</p>}
      {/* only once the state says which version runs: unknown head must not offer a rollback onto the running one */}
      {v.versionNo != null && state.data?.delivered === true && state.data.headVersion != null && v.versionNo !== state.data.headVersion.versionNo && (
        <div className="slds-m-top_small">
          {rolling ? (
            <RollbackPlanConfirm project={slug} version={{ id: v.id, versionNo: v.versionNo }} pendingCount={state.data?.pendingCount} propagationSeconds={state.data?.propagationSeconds || undefined}
              onCancel={() => { setRolling(false); setTimeout(() => trigger.current?.focus(), 0); }} onDone={() => { setRolling(false); router.push(studioHref(slug, "bao-tri/lich-su")); }} />
          ) : (
            <Button ref={trigger} variant="neutral" aria-label={tf("studio.maintain.rollback.submitFor", { n: v.versionNo })} onClick={() => setRolling(true)}>{t("studio.maintain.rollback.submit")}</Button>
          )}
        </div>
      )}
      <div className="slds-m-top_x-small">
        <SchemaRetry project={slug} versionId={id} status={v.schemaStatus} label={t("studio.maintain.history.retryDocs")} onDone={() => void q.refetch()} />
      </div>
      {docs.length === 0 ? (
        <p className="slds-m-top_medium slds-text-color_weak">{t("studio.maintain.version.noteOnly")}</p>
      ) : (
        <>
          <div className="flex flex-wrap gap-3 slds-m-top_medium">
            <Select label={t("studio.maintain.version.type")} value={loai ?? ""} placeholder={false} onChange={(e) => go({ loai: e.target.value, bang })}
              options={[{ value: "", label: t("studio.maintain.version.all") }, ...types.map((x) => ({ value: x, label: docTypeLabel(x) }))]} />
            {tables.length > 0 && (
              <Select label={t("studio.maintain.version.table")} value={bang ?? ""} placeholder={false} onChange={(e) => go({ loai, bang: e.target.value })}
                options={[{ value: "", label: t("studio.maintain.version.all") }, ...tables.map((x) => ({ value: x, label: x }))]} />
            )}
          </div>
          {shown.length === 0 ? <p className="slds-m-top_medium slds-text-color_weak">{t("studio.maintain.version.noMatch")}</p> : shown.map((d) => <DocBlock key={`${d.docType}:${d.docKey}`} doc={d} />)}
        </>
      )}
    </div>
  );
}
