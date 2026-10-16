"use client";

// PublishReview — review and publish the pending drafts (specs/073 nhap1a, UI-ARCH 5.3 + 6.4, FR-065/-111).
// Reads publish/state (usePublishState, token-gated) and publish/changes itself. Popover anchored on `anchor` from 768px,
// a Modal below. Refusals answered in place: DRAFTS_CHANGED reloads, NOTHING_TO_PUBLISH refreshes the state (no red),
// PUBLISH_CONFLICT offers "Ghi đè" through an InlineConfirm (force:true is only sent after it), PUBLISH_BUSY retries once,
// PUBLISH_INVALID prints per line. Wiring into ProjectHeader is nhap1b.
import { useCallback, useEffect, useState, type ReactElement } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ApiError, discardAllDrafts, discardDraft, listDraftChanges, publishDrafts, retrySchemaOps, type DraftChange, type PublishState } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { activeProjectSlug } from "@/lib/studioContext";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { Modal } from "@/components/slds/Modal";
import { Popover } from "@/components/slds/Popover";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { Textarea } from "@/components/slds/Textarea";
import { ChangeList, type ChangeItem } from "./ChangeList";
import { InlineConfirm } from "./InlineConfirm";
import { usePublishState } from "./usePublishState";

const NOTE_MAX = 500;
const TYPE_LABEL: Record<string, I18nKey> = {
  workflow: "draftBar.type.workflow", report: "draftBar.type.report", dashboard: "draftBar.type.dashboard",
  app: "draftBar.type.app", layout: "draftBar.type.layout", branding: "draftBar.type.branding",
  field: "draftBar.type.field", entity: "draftBar.type.entity", relationship: "draftBar.type.relationship",
};
/** A pending delete of a field / table: runs AFTER the publish commits, listed apart (specs/063 S7-9). */
const isSchemaDelete = (c: DraftChange) => c.op === "delete" && (c.docType === "field" || c.docType === "entity");
const typeLabel = (docType: string) => (TYPE_LABEL[docType] ? t(TYPE_LABEL[docType]) : docType);
const OP: Record<DraftChange["op"], ChangeItem["op"]> = { create: "add", update: "change", delete: "remove" };

interface Conflict { docType: string; docKey: string; label: string }

export interface PublishReviewProps {
  /** The element the popover hangs from (the "Xuất bản" button). */
  anchor: ReactElement;
  onClose: () => void;
}

/** Below 768px the popover becomes a Modal. */
function useNarrow(): boolean {
  const [narrow, setNarrow] = useState(false);
  useEffect(() => {
    const mq = window.matchMedia?.("(max-width: 767px)");
    if (!mq) return;
    const sync = () => setNarrow(mq.matches);
    sync();
    mq.addEventListener("change", sync);
    return () => mq.removeEventListener("change", sync);
  }, []);
  return narrow;
}

export function PublishReview({ anchor, onClose }: PublishReviewProps) {
  const narrow = useNarrow();
  const body = <ReviewBody onClose={onClose} />;
  if (narrow) {
    return (
      <Modal open onOpenChange={(o) => { if (!o) onClose(); }} title={t("studio.publish.title")} size="medium">
        {body}
      </Modal>
    );
  }
  return (
    <Popover open onOpenChange={(o) => { if (!o) onClose(); }} trigger={anchor} title={t("studio.publish.title")} size="large" side="bottom" align="end">
      {body}
    </Popover>
  );
}

/** "Xoá chưa xong · Thử lại" (specs/063 S7-9): a published version whose field / table deletes did not all run. */
function SchemaRetryPanel({ retry, onDone }: { retry: NonNullable<PublishState["schemaRetry"]>; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<string | null>(null);
  async function run() {
    setBusy(true);
    setMsg(null);
    try {
      const res = await retrySchemaOps(retry.versionId);
      setMsg(res.schemaStatus === "done" ? t("studio.publish.retryDone") : t("studio.publish.retryStill"));
      if (res.schemaStatus === "done") toast.success(t("studio.publish.retryDone"));
    } catch (e) {
      // 409 = another tab finished it first: same end state.
      if (e instanceof ApiError && e.code === "SCHEMA_RETRY_NOT_NEEDED") setMsg(t("studio.publish.retryDone"));
      else setMsg(tf("studio.publish.failed", { msg: e instanceof Error ? e.message : String(e) }));
    }
    onDone();
    setBusy(false);
  }
  return (
    <ScopedNotification theme="warning" className="slds-m-top_small">
      <p className="slds-text-title">{tf("studio.publish.retryHeading", { n: retry.versionNo })}</p>
      <p>{t("studio.publish.retryBody")}</p>
      <ul className="slds-list_dotted">
        {retry.ops.map((o) => (
          <li key={`${o.docType}:${o.docKey}`}>{o.label} <span className="slds-text-color_weak">· {typeLabel(o.docType)}</span>{o.error ? ` — ${o.error}` : ""}</li>
        ))}
      </ul>
      {msg && <p role="status">{msg}</p>}
      <Button variant="neutral" disabled={busy} onClick={() => void run()}>{busy ? t("studio.publish.retrying") : t("studio.publish.retry")}</Button>
    </ScopedNotification>
  );
}

function ReviewBody({ onClose }: { onClose: () => void }) {
  const slug = activeProjectSlug();
  const qc = useQueryClient();
  const state = usePublishState(slug);
  const [changes, setChanges] = useState<DraftChange[] | null>(null);
  const [hash, setHash] = useState("");
  const [loadErr, setLoadErr] = useState(false);
  const [note, setNote] = useState("");
  const [conflicts, setConflicts] = useState<Conflict[]>([]);
  const [info, setInfo] = useState<string | null>(null);
  const [err, setErr] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [ask, setAsk] = useState<"force" | "discardAll" | null>(null);

  const reload = useCallback(async () => {
    try {
      const r = await listDraftChanges();
      setChanges(r.changes);
      setHash(r.changesetHash);
      setLoadErr(false);
    } catch {
      setLoadErr(true);
    }
  }, []);
  // First load: setState only in the promise callbacks.
  useEffect(() => {
    let alive = true;
    listDraftChanges().then(
      (r) => { if (alive) { setChanges(r.changes); setHash(r.changesetHash); } },
      () => { if (alive) setLoadErr(true); },
    );
    return () => { alive = false; };
  }, []);

  // One refresh point for everything that moved: the cached state, and every builder listening for draft changes.
  const refreshAll = () => {
    void qc.invalidateQueries({ queryKey: ["project", slug] });
    void state.refetch();
    emitDraftsChanged("publish-review");
  };

  async function publish(force: boolean, retried = false): Promise<void> {
    setBusy(true);
    setErr([]);
    setInfo(null);
    try {
      const res = await publishDrafts(hash, { note: note.trim() || undefined, force });
      toast.success(tf("studio.publish.done", { n: res.versionNo, s: res.propagationSeconds }));
      // The version is live, but a field / table delete did not finish: say so, Thử lại sits in the review.
      if (res.schemaStatus === "failed") toast.error(tf("studio.publish.schemaFailedToast", { n: res.versionNo }));
      refreshAll();
      onClose();
      return;
    } catch (e) {
      if (!(e instanceof ApiError)) {
        setErr([tf("studio.publish.failed", { msg: String(e) })]);
      } else if (e.code === "DRAFTS_CHANGED") {
        setInfo(t("studio.publish.changed"));
        await reload();
        refreshAll();
      } else if (e.code === "NOTHING_TO_PUBLISH") {
        setInfo(t("studio.publish.nothing"));
        await reload();
        refreshAll();
      } else if (e.code === "PUBLISH_BUSY") {
        if (!retried) return publish(force, true);
        setErr([t("studio.publish.busy")]);
      } else if (e.code === "PUBLISH_CONFLICT") {
        setConflicts((e.details as { conflicts?: Conflict[] } | undefined)?.conflicts ?? []);
        await reload();
      } else if (e.code === "PUBLISH_INVALID") {
        const errs = ((e.details as { errors?: { message?: string }[] } | undefined)?.errors ?? []).map((x) => x.message).filter((m): m is string => !!m);
        setErr(errs.length ? errs.map((m) => tf("studio.publish.invalid", { msg: m })) : [tf("studio.publish.invalid", { msg: e.message })]);
      } else {
        setErr([tf("studio.publish.failed", { msg: e.message })]);
      }
    }
    setBusy(false);
    setAsk(null);
  }

  async function discardOne(c: DraftChange) {
    setBusy(true);
    setErr([]);
    try {
      await discardDraft(c.id);
    } catch (e) {
      // 404 = it was already discarded elsewhere: same end state, just refresh.
      if (!(e instanceof ApiError && e.code === "DRAFT_NOT_FOUND")) {
        setErr([tf("studio.publish.failed", { msg: e instanceof Error ? e.message : String(e) })]);
        setBusy(false);
        return;
      }
    }
    setConflicts([]);
    await reload();
    refreshAll();
    setBusy(false);
  }

  async function discardAll() {
    setBusy(true);
    setErr([]);
    try {
      await discardAllDrafts(hash);
      refreshAll();
      onClose();
      return;
    } catch (e) {
      if (e instanceof ApiError && e.code === "DRAFTS_CHANGED") {
        setInfo(t("studio.publish.changed"));
        await reload();
        refreshAll();
      } else {
        setErr([tf("studio.publish.failed", { msg: e instanceof Error ? e.message : String(e) })]);
      }
    }
    setAsk(null);
    setBusy(false);
  }

  const empty = changes !== null && changes.length === 0;
  const discardableCount = (changes ?? []).filter((c) => c.discardable !== false).length;
  const anyConflict = conflicts.length > 0 || (changes ?? []).some((c) => c.conflict);
  const conflictNames = (conflicts.length > 0 ? conflicts : (changes ?? []).filter((c) => c.conflict)).map((c) => c.label).join(", ");
  const toItem = (c: DraftChange): ChangeItem => ({
    op: OP[c.op],
    key: c.id,
    text: (
      <>
        <span className="slds-grid slds-grid_vertical-align-center">
          <span className="slds-truncate" title={c.label}>{c.label} <span className="slds-text-color_weak">· {typeLabel(c.docType)}</span></span>
          {c.conflict && <span className="nx-status nx-status_error slds-m-left_x-small">{t("studio.publish.conflictBadge")}</span>}
          {(c.changedPaths ?? []).length > 0 && (
            <span className="slds-text-body_small slds-text-color_weak slds-truncate slds-m-left_x-small" title={c.changedPaths.join(", ")}>
              {tf("studio.publish.changedPaths", { paths: c.changedPaths.slice(0, 5).join(", ") })}
              {c.changedPaths.length > 5 || c.moreChanged > 0 ? ` ${tf("studio.publish.moreChanged", { n: c.changedPaths.length - 5 + c.moreChanged })}` : ""}
            </span>
          )}
          {c.discardable === false ? (
            // Born live and hidden, no draft: nothing to drop here (delete it in the builder to take it back).
            <span className="slds-text-body_small slds-text-color_weak slds-m-left_auto">{t("studio.status.new")}</span>
          ) : (
            <Button variant="base" className="slds-m-left_auto" disabled={busy} aria-label={tf("studio.publish.discardLabel", { name: c.label })} onClick={() => void discardOne(c)}>
              {t("studio.publish.discard")}
            </Button>
          )}
        </span>
        {(c.warnings ?? []).map((w) => (
          <div key={w} className="slds-text-body_small slds-text-color_weak">{tf("studio.publish.warning", { msg: w })}</div>
        ))}
      </>
    ),
  });
  const items: ChangeItem[] = (changes ?? []).filter((c) => !isSchemaDelete(c)).map(toItem);
  const deleteItems: ChangeItem[] = (changes ?? []).filter(isSchemaDelete).map(toItem);
  const retry = state.data?.schemaRetry ?? null;

  return (
    <div>
      {state.data && (
        <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_small">
          {tf("studio.publish.routing", { types: state.data.draftableTypes.map((d) => (d === "entity" ? t("studio.publish.routing.entityDeleteOnly") : typeLabel(d))).join(", ") })}
        </p>
      )}
      {info && <ScopedNotification theme="info" className="slds-m-bottom_small">{info}</ScopedNotification>}
      {loadErr && <ScopedNotification theme="error" className="slds-m-bottom_small">{t("studio.publish.loadFailed")}</ScopedNotification>}
      {changes === null && !loadErr && (
        <div className="slds-p-around_small"><Spinner size="x-small" inline label={t("studio.publish.loading")} /> {t("studio.publish.loading")}</div>
      )}
      {empty && <p className="slds-text-color_weak">{t("studio.publish.empty")}</p>}
      {changes !== null && !empty && (
        <>
          <p className="slds-text-title slds-m-bottom_x-small">{tf("studio.publish.count", { n: changes.length })}</p>
          {items.length > 0 && <ChangeList items={items} />}
          {deleteItems.length > 0 && (
            <>
              <p className="slds-text-title slds-m-top_small slds-m-bottom_x-small">{t("studio.publish.deletesHeading")}</p>
              <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_x-small">{t("studio.publish.deletesNote")}</p>
              <ChangeList items={deleteItems} />
            </>
          )}
        </>
      )}
      {retry && <SchemaRetryPanel retry={retry} onDone={refreshAll} />}
      {anyConflict && ask !== "force" && (
        <ScopedNotification theme="warning" className="slds-m-top_small">
          {tf("studio.publish.conflictAsk", { names: conflictNames })}{" "}
          <Button variant="base" disabled={busy} onClick={() => setAsk("force")}>{t("studio.publish.conflictForce")}</Button>
        </ScopedNotification>
      )}
      {ask === "force" && (
        <div className="slds-m-top_small">
          <InlineConfirm
            tone="warning"
            message={tf("studio.publish.conflictAsk", { names: conflictNames })}
            confirmLabel={t("studio.publish.conflictForceConfirm")}
            cancelLabel={t("studio.publish.conflictReview")}
            busy={busy}
            onCancel={() => setAsk(null)}
            onConfirm={() => void publish(true)}
          />
        </div>
      )}
      {ask === "discardAll" && (
        <div className="slds-m-top_small">
          <InlineConfirm
            tone="error"
            message={tf("studio.publish.discardAllAsk", { n: discardableCount })}
            confirmLabel={t("studio.publish.discardAllConfirm")}
            busy={busy}
            onCancel={() => setAsk(null)}
            onConfirm={() => void discardAll()}
          />
        </div>
      )}
      <Textarea className="slds-m-top_small" label={t("studio.publish.note")} maxLength={NOTE_MAX} value={note} onChange={(e) => setNote(e.target.value)} />
      {err.length > 0 && (
        <ScopedNotification theme="error" className="slds-m-top_small">
          {err.map((m) => <div key={m}>{m}</div>)}
        </ScopedNotification>
      )}
      <div className="slds-m-top_small slds-grid slds-grid_align-end">
        <Button variant="neutral" onClick={onClose} disabled={busy}>{t("studio.publish.close")}</Button>
        <Button variant="destructive" className="slds-m-left_x-small" disabled={busy || !changes || discardableCount === 0} onClick={() => setAsk("discardAll")}>
          {t("studio.publish.discardAll")}
        </Button>
        <Button variant="brand" className="slds-m-left_x-small" data-testid="publish-submit" disabled={busy || !changes || empty || anyConflict} onClick={() => void publish(false)}>
          {busy ? t("studio.publish.submitting") : t("studio.publish.submit")}
        </Button>
      </div>
    </div>
  );
}
