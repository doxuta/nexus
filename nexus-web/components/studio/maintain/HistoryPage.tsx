"use client";

// HistoryPage — bao-tri/lich-su (specs/079 f7-lich-su-dong-thoi-gian, US5): the publish history as a timeline.
// One row per version the customer has had, newest first; "Khách đang dùng" sits on the head version only, an
// applied-at-once entry (versionNo null) says "Áp ngay" and carries "Hoàn tác". Each row can unfold a short change list.
// The timeline is a ruled list with a 9px dot (the plan's fallback to ActivityTimeline marker="dot": the SLDS timeline
// tile + connector are the wrong shape for a version log). Not delivered ⇒ no versions; old server ⇒ one honest line.
import { Circle } from "lucide-react";
import Link from "next/link";
import { useEffect, useRef, useState } from "react";
import type { PublishVersion } from "@/lib/api";
import { EmptyState } from "@/components/slds/EmptyState";
import { Button } from "@/components/slds/Button";
import { Spinner } from "@/components/slds/Spinner";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { isMounted, studioHref } from "@/lib/studioRoutes";
import { docTypeLabel } from "@/lib/versionDiff";
import { ChangeList } from "../ChangeList";
import { RollbackPlanConfirm } from "./RollbackPlanConfirm";
import { StatusText } from "../StatusText";
import { usePublishState } from "../usePublishState";
import { RevertConfirm } from "./RevertConfirm";
import { SchemaRetry } from "./SchemaRetry";
import { isEngineMissing, usePublishVersion, usePublishVersions } from "./queries";

const PAGE = 20;
const PREVIEW = 8;
const when = (iso: string) => new Date(iso).toLocaleString("vi-VN");

function noteOf(v: PublishVersion): string {
  if (v.note) return v.note;
  if (v.kind === "baseline") return t("studio.maintain.history.baseline");
  if (v.kind === "rollback") return v.rollbackOf != null ? tf("studio.maintain.history.rollbackTo", { m: v.rollbackOf }) : t("studio.maintain.history.rollbackOld");
  return tf("studio.maintain.history.changes", { n: v.docCount });
}

function Changes({ slug, id }: { slug: string; id: string }) {
  const q = usePublishVersion(slug, id);
  if (q.isPending) return <Spinner size="x-small" inline />;
  if (q.isError) return <p className="slds-text-color_weak">{t("studio.maintain.history.detailFailed")}</p>;
  const rows = q.data.docs.map((d) => ({
    op: (d.op === "create" ? "add" : d.op === "delete" ? "remove" : "change") as "add" | "remove" | "change",
    text: `${docTypeLabel(d.docType)} · ${d.label}`,
    key: `${d.docType}:${d.docKey}`,
  }));
  if (rows.length === 0) return <p className="slds-text-color_weak">{t("studio.maintain.history.noteOnly")}</p>;
  return (
    <div>
      <ChangeList items={rows.slice(0, PREVIEW)} />
      {rows.length > PREVIEW && <p className="slds-text-color_weak">{tf("studio.maintain.history.moreChanges", { n: rows.length - PREVIEW })}</p>}
      <p className="slds-m-top_x-small"><Link href={studioHref(slug, "bao-tri/lich-su/:id", { id })}>{t("studio.maintain.history.viewDiff")}</Link></p>
    </div>
  );
}

interface RollbackHost { openId: string | null; pending: number | null | undefined; seconds?: number; open: (id: string) => void; close: () => void; done: () => void }

function Row({ slug, v, head, onChanged, rb }: { slug: string; v: PublishVersion; head: number | null; onChanged: () => void; rb: RollbackHost }) {
  const [open, setOpen] = useState(false);
  const [undo, setUndo] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const undoBtn = useRef<HTMLButtonElement>(null);
  const wasUndo = useRef(false);
  // Keyboard users: focus returns to "Hoàn tác" when the inline confirm closes.
  useEffect(() => {
    if (wasUndo.current && !undo) undoBtn.current?.focus();
    wasUndo.current = undo;
  }, [undo]);
  const direct = v.versionNo == null;
  const current = !direct && v.versionNo === head;
  const tone = current ? "success" : direct ? "warn" : "weak";
  return (
    <li className="slds-item">
      <div className="flex flex-wrap items-start gap-x-3 gap-y-1">
        <span className={`nx-status nx-status_${tone} mt-1`} aria-hidden="true"><Circle size={9} fill="currentColor" strokeWidth={0} /></span>
        <div className="min-w-[12rem] flex-1">
          <p>
            <span className="font-semibold">{direct ? t("studio.maintain.history.immediate") : tf("studio.maintain.history.versionNo", { n: v.versionNo ?? 0 })}</span>{" "}
            {current && <StatusText tone="success">{t("studio.maintain.history.current")}</StatusText>}
          </p>
          <p className="break-words">{noteOf(v)}</p>
          <p className="slds-text-body_small slds-text-color_weak">
            {v.createdByName} · <time dateTime={v.createdAt} title={v.createdAt}>{when(v.createdAt)}</time>
          </p>
          <SchemaRetry project={slug} versionId={v.id} status={v.schemaStatus} label={t("studio.maintain.history.retry")} onDone={onChanged} />
          {open && <div className="slds-m-top_x-small"><Changes slug={slug} id={v.id} /></div>}
          {rb.openId === v.id && v.versionNo != null && (
            <div className="slds-m-top_small">
              <RollbackPlanConfirm project={slug} version={{ id: v.id, versionNo: v.versionNo }} pendingCount={rb.pending} propagationSeconds={rb.seconds} onCancel={() => { rb.close(); setTimeout(() => trigger.current?.focus(), 0); }} onDone={rb.done} />
            </div>
          )}
          {undo && <div className="slds-m-top_x-small"><RevertConfirm project={slug} versionId={v.id} onCancel={() => setUndo(false)} onDone={() => { setUndo(false); onChanged(); }} /></div>}
        </div>
        <div className="flex gap-2">
          <Button variant="neutral" aria-expanded={open} onClick={() => setOpen((x) => !x)}>
            {t(open ? "studio.maintain.history.hide" : "studio.maintain.history.show")}
          </Button>
          {direct && <Button ref={undoBtn} variant="neutral" hidden={undo} onClick={() => setUndo(true)}>{t("studio.maintain.history.undo")}</Button>}
          {!direct && head != null && !current && rb.openId !== v.id && <Button ref={trigger} variant="neutral" aria-label={tf("studio.maintain.rollback.submitFor", { n: v.versionNo ?? 0 })} onClick={() => rb.open(v.id)}>{t("studio.maintain.rollback.submit")}</Button>}
        </div>
      </div>
    </li>
  );
}

export function HistoryPage({ slug }: { slug: string }) {
  const state = usePublishState(slug);
  const delivered = state.data?.delivered === true;
  const [limit, setLimit] = useState(PAGE);
  const versions = usePublishVersions(slug, delivered, limit);
  const [rollId, setRollId] = useState<string | null>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const focusTimer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(focusTimer.current), []);
  const audit = studioHref(slug, "van-hanh/nguoi-dung/nhat-ky");

  if (state.isPending || (delivered && versions.isPending)) return <div className="slds-is-relative slds-p-around_xx-large"><Spinner size="medium" /></div>;
  if (versions.isError && isEngineMissing(versions.error)) {
    return (
      <div className="slds-p-around_large">
        <p className="nx-lede">{t("studio.maintain.history.missing")}</p>
        <p className="slds-m-top_small"><Link href={audit}>{t("studio.maintain.history.audit")}</Link></p>
      </div>
    );
  }
  if (state.isError || versions.isError) {
    const err = state.error ?? versions.error;
    return (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{errorMessage(err)}</ScopedNotification>
        <div className="slds-m-top_small"><Button variant="neutral" onClick={() => { void state.refetch(); void versions.refetch(); }}>{t("studio.query.retry")}</Button></div>
      </div>
    );
  }
  if (!delivered) {
    return (
      <div className="slds-p-around_medium">
        <EmptyState plain title={t("studio.route.history")} description={t("studio.maintain.history.empty")}
          action={isMounted("dung/giao-cho-khach") ? <Link className="slds-button slds-button_brand" href={studioHref(slug, "dung/giao-cho-khach")}>{t("studio.maintain.history.deliver")}</Link> : undefined} />
      </div>
    );
  }
  const rb: RollbackHost = {
    openId: rollId,
    // a failed or slow state read stays "unknown" (locks the button), never 0
    pending: state.data?.pendingCount,
    seconds: state.data?.propagationSeconds || undefined,
    open: setRollId,
    close: () => setRollId(null),
    // focus goes to the list, not <body>, once the confirm block unmounts
    done: () => { setRollId(null); focusTimer.current = setTimeout(() => listRef.current?.focus(), 0); },
  };
  const list = versions.data?.versions ?? [];
  const total = versions.data?.total ?? list.length;
  return (
    <div className="slds-p-around_medium">
      <ul ref={listRef} tabIndex={-1} aria-label={t("studio.maintain.history.list")} className="slds-has-dividers_bottom-space">
        {list.map((v) => <Row key={v.id} slug={slug} v={v} head={state.data?.headVersion?.versionNo ?? null} onChanged={() => { void state.refetch(); void versions.refetch(); }} rb={rb} />)}
      </ul>
      {list.length < total && <div className="slds-m-top_small"><Button variant="neutral" onClick={() => setLimit((l) => l + PAGE)}>{t("studio.maintain.history.more")}</Button></div>}
    </div>
  );
}
