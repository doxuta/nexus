"use client";

// "Trao đổi" tab (GET/POST /:entity/:id/notes · DELETE …/:noteId) — free-text conversation on ANY
// record (note_handler.go; docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 1). SLDS: the composer is the
// publisher blueprint (publishers/base, active), the notes are the feed blueprint (feeds/base:
// ul.slds-feed__list > li.slds-feed__item > article.slds-post). Rules pinned by NotesTab.test.tsx:
//   • body renders as PLAIN TEXT via React's default escaping — the backend stores raw text and
//     does NOT sanitize HTML (layering: escape at the render layer). NEVER dangerouslySetInnerHTML.
//   • the delete button shows on EVERY note — authorship/delete-grant is server-side; a 403
//     surfaces its real message instead of the FE guessing permissions.
//   • author = createdByName when the BE resolved it, else the raw short id — never a fake name.
//   • readOnly (specs/051 FR-009 — the record is seen only through a manual share): no composer, no
//     delete, no "write the first" CTA; the feed stays readable. Every write there is refused by
//     the server, so offering the control would only teach the reader to click into an error.
import { useEffect, useId, useRef, useState } from "react";
import { MessageSquare, Pencil, X } from "lucide-react";
import {
  ensureAuth, listRecordNotes, createRecordNote, deleteRecordNote, ApiError, type RecordNote,
} from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { relTime } from "@/lib/workflowRuns";
import { t } from "@/lib/i18n";
import { Avatar } from "@/components/slds/Avatar";
import { Button } from "@/components/slds/Button";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { shortId } from "./HistoryTab";

// BE limit is 5000 runes (not bytes); the counter appears when the draft gets close.
const NOTE_MAX = 5000;
const COUNTER_FROM = 4000;

export function NotesTab({ entity, recordId, readOnly = false }: { entity: string; recordId: string; readOnly?: boolean }) {
  const [state, setState] = useState<"loading" | "ok" | "error">("loading");
  const [errMsg, setErrMsg] = useState(""); // initial-load error (state === "error")
  const [actErr, setActErr] = useState(""); // create/delete error — the server's real message
  const [notes, setNotes] = useState<RecordNote[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loadingMore, setLoadingMore] = useState(false);
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [confirmDelId, setConfirmDelId] = useState(""); // two-click delete
  const [deletingId, setDeletingId] = useState("");
  const composerRef = useRef<HTMLTextAreaElement>(null);
  const composerId = useId();

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        await ensureAuth();
        const p = await listRecordNotes(entity, recordId);
        if (cancelled) return;
        setNotes(p.notes);
        setTotal(p.pagination.total);
        setPage(1);
        setState("ok");
      } catch (e) {
        if (cancelled) return;
        setErrMsg(e instanceof ApiError ? e.message : t("page.record.notes.loadErrorFallback"));
        setState("error");
      }
    })();
    return () => { cancelled = true; };
  }, [entity, recordId]);

  const send = async () => {
    if (!draft.trim() || sending) return; // input guard only — server re-validates (RECORD_NOTE_EMPTY)
    setSending(true);
    setActErr("");
    try {
      await ensureAuth();
      const n = await createRecordNote(entity, recordId, draft);
      setNotes((prev) => [n, ...prev]); // newest first — mirrors the list order
      setTotal((x) => x + 1);
      setDraft("");
    } catch (e) {
      // RECORD_NOTE_EMPTY / RECORD_NOTE_TOO_LONG (422) — show the server's message verbatim.
      setActErr(e instanceof ApiError ? e.message : t("page.record.notes.sendError"));
    } finally {
      setSending(false);
    }
  };

  const del = async (id: string) => {
    if (deletingId) return; // aria-disabled button: the guard is ours
    if (confirmDelId !== id) { setConfirmDelId(id); return; }
    setDeletingId(id);
    setActErr("");
    try {
      await ensureAuth();
      await deleteRecordNote(entity, recordId, id);
      setNotes((prev) => prev.filter((n) => n.id !== id));
      setTotal((x) => Math.max(0, x - 1));
    } catch (e) {
      // 403 = not the author and no delete grant — real message, never pre-hidden buttons.
      setActErr(e instanceof ApiError ? e.message : t("page.record.notes.deleteError"));
    } finally {
      setDeletingId("");
      setConfirmDelId("");
    }
  };

  const loadMore = async () => {
    if (loadingMore) return;
    setLoadingMore(true);
    try {
      await ensureAuth();
      const p = await listRecordNotes(entity, recordId, page + 1);
      setNotes((prev) => [...prev, ...p.notes]);
      setTotal(p.pagination.total);
      setPage((x) => x + 1);
    } catch (e) {
      setActErr(e instanceof ApiError ? e.message : t("page.record.history.loadMoreError"));
    } finally {
      setLoadingMore(false);
    }
  };

  if (state === "loading") {
    return (
      <div className="relative h-24" aria-busy="true" aria-label={t("page.record.notes.loading")}>
        <Spinner size="small" label={t("page.record.notes.loading")} />
      </div>
    );
  }

  if (state === "error") {
    return (
      <ScopedNotification theme="error">
        <strong>{t("page.record.notes.loadError")}</strong> <span>{errMsg}</span>
      </ScopedNotification>
    );
  }

  const runes = [...draft].length;

  // One feed column (.slds-feed: 49rem, centred) holds the publisher, the posts and paging.
  return (
    <div className="slds-feed">
      {/* publishers/base, active: textarea + actions row (counter · Gửi). */}
      {!readOnly && <div className="slds-publisher slds-is-active">
        <label htmlFor={composerId} className="slds-assistive-text">
          {t("page.record.notes.composerLabel")}
        </label>
        <textarea
          id={composerId}
          ref={composerRef}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === "Enter") { e.preventDefault(); void send(); }
          }}
          rows={2}
          placeholder={t("page.record.notes.placeholder")}
          className="slds-publisher__input slds-textarea slds-text-longform"
        />
        <div className="slds-publisher__actions slds-grid slds-grid_align-spread slds-grid_vertical-align-center slds-m-top_x-small">
          {runes > COUNTER_FROM ? (
            <span className={runes > NOTE_MAX ? "slds-text-color_error" : "slds-text-color_weak"}>
              {runes.toLocaleString("vi-VN")}/{NOTE_MAX.toLocaleString("vi-VN")}
            </span>
          ) : <span />}
          <Button variant="brand" onClick={() => void send()} disabled={sending || !draft.trim()}>
            {sending ? t("page.record.notes.sending") : t("page.record.notes.send")}
          </Button>
        </div>
      </div>}

      {actErr && <ScopedNotification theme="warning" className="slds-m-top_small">{actErr}</ScopedNotification>}

      {notes.length === 0 ? (
        <EmptyState
          icon={MessageSquare}
          title={t("page.record.notes.emptyTitle")}
          description={readOnly ? t("page.record.notes.emptyTextReadOnly") : t("page.record.notes.emptyText")}
          action={readOnly ? undefined : (
            <Button iconLeft={Pencil} onClick={() => composerRef.current?.focus()}>
              {t("page.record.notes.writeFirst")}
            </Button>
          )}
        />
      ) : (
        <ul className="slds-feed__list slds-m-top_small">
          {notes.map((n) => {
            const author = n.createdByName || shortId(n.createdBy);
            const confirming = confirmDelId === n.id;
            return (
              <li key={n.id} className="slds-feed__item">
                <article className="slds-post">
                  <header className="slds-post__header slds-media">
                    <div className="slds-media__figure">
                      <Avatar name={author} shape="circle" size="medium" />
                    </div>
                    <div className="slds-media__body">
                      <div className="slds-grid slds-grid_align-spread slds-has-flexi-truncate">
                        <p className="slds-truncate">
                          <strong>{author}</strong>
                        </p>
                        {/* ONE <button> for both presses (button-icons bordered x-small ⇄ buttons
                            text-destructive): swapping ButtonIcon for Button remounts the element,
                            focus falls to <body> and a keyboard user's second Enter deletes nothing.
                            aria-disabled, not disabled, while deleting: disabling the focused
                            button would drop focus too when the server refuses. */}
                        {!readOnly && <button
                          type="button"
                          title={t("page.record.notes.delete")}
                          aria-disabled={deletingId === n.id || undefined}
                          className={
                            confirming
                              ? "slds-button slds-button_text-destructive"
                              : "slds-button slds-button_icon slds-button_icon-border slds-button_icon-x-small"
                          }
                          onClick={() => void del(n.id)}
                        >
                          {confirming ? (
                            deletingId === n.id ? t("page.record.deleting") : t("page.record.notes.deleteConfirm")
                          ) : (
                            <>
                              <X aria-hidden="true" className="slds-button__icon slds-button__icon_hint nx-lucide" />
                              <span className="slds-assistive-text">{t("page.record.notes.delete")}</span>
                            </>
                          )}
                        </button>}
                      </div>
                      <p className="slds-text-body_small slds-text-color_weak" title={formatDateTime(new Date(n.createdAt))}>
                        {relTime(n.createdAt)}
                      </p>
                    </div>
                  </header>
                  {/* Plain text BY DESIGN: React escapes; the BE stores raw text unsanitized. */}
                  <div className="slds-post__content slds-text-longform">
                    <p className="slds-hyphenate whitespace-pre-wrap">{n.body}</p>
                  </div>
                </article>
              </li>
            );
          })}
        </ul>
      )}

      {notes.length < total && (
        <Button className="slds-m-top_small" onClick={() => void loadMore()} disabled={loadingMore}>
          {loadingMore
            ? t("page.record.loading")
            : t("page.record.notes.loadMore").replace("{n}", String(total - notes.length))}
        </Button>
      )}
    </div>
  );
}
