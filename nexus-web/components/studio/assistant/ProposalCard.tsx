"use client";

// One assistant proposal as a card (specs/076 ai2, UI-ARCH `cards/base`): a purple lead line, the title, a "where" line,
// the "+ ~ − =" list and the Áp dụng · Bỏ row. Not a chat bubble. Applying is NOT done here: the host passes onApply
// (it owns the write and the cache refresh) and mirrors the outcome back through `state`.
import { useEffect, useRef, useState } from "react";
import { Check, Eye, HelpCircle, Sparkles } from "lucide-react";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { Input } from "@/components/slds/Input";
import { Spinner } from "@/components/slds/Spinner";
import { ChangeList } from "@/components/studio/ChangeList";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { proposedFromItem, useProposalOverlay } from "@/components/studio/assistant/ProposalOverlay";
import { StatusText, type StatusTone } from "@/components/studio/StatusText";
import { getDeleteEntityDraft, getDeleteFieldDraft, needsLiveConfirm, type AiDraftItem } from "@/lib/aiBuilder";
import { proposalDone, proposalView, proposalWarnLink, type ProposalOp } from "@/lib/aiProposalView";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import type { ApplyState } from "@/lib/aiApplyState";
import type { CardMode } from "@/lib/aiApply";
import type { RouteId } from "@/lib/studioRoutes";
import Link from "next/link";

/** The card reads the same state the host persists (lib/aiApplyState). */
export type ProposalState = ApplyState;

/** Draft-aware behaviour of a card on a delivered project (specs/076 ai9). Absent ⇒ the plain Áp dụng · Bỏ card. */
export interface CardDraftProps {
  mode: CardMode;
  onRecheck: () => void;
  /** Reads the drafts now: the labels that would go, or why Hoàn tác is no longer safe. */
  onUndoPlan: () => Promise<{ labels: string[] } | "stale" | "error">;
  onUndo: () => Promise<"undone" | "stale" | "error">;
  /** Whether "Mở bản nháp" can actually open the review (FR-127). */
  canOpenDrafts: boolean;
  onOpenDrafts: () => void;
}

export interface ProposalCardProps {
  item: AiDraftItem;
  state?: ProposalState;
  /** The project is known to be undelivered ⇒ the where line says the customer cannot see it yet. */
  delivered?: boolean;
  asSample: boolean;
  onAsSampleChange: (v: boolean) => void;
  onApply: () => void;
  onDiscard: () => void;
  /** The change came from an older session: Before/After may be stale. */
  reopened?: boolean;
  link: (id: RouteId) => string;
  /** Stable key of this card in the conversation; with it (and a painter in PAINTED) the card can offer "Xem trước". */
  proposalKey?: string;
  draft?: CardDraftProps;
}

const OP_WORD: Record<ProposalOp, I18nKey> = {
  create: "studio.assistant.change.create",
  update: "studio.assistant.change.update",
  delete: "studio.assistant.change.delete",
};

export function ProposalCard({ item, state, delivered, asSample, onAsSampleChange, onApply, onDiscard, reopened, link, proposalKey, draft }: ProposalCardProps) {
  const st = state?.status ?? "idle";
  const view = proposalView(item, { delivered });
  const cardRef = useRef<HTMLElement>(null);
  const applyRef = useRef<HTMLButtonElement>(null);
  const undoRef = useRef<HTMLButtonElement>(null);
  const acted = useRef(false);
  const backToRoot = useRef(false);
  const [confirming, setConfirming] = useState(false);
  const [typed, setTyped] = useState("");
  const [liveAsk, setLiveAsk] = useState(false);
  // Hoàn tác in place: idle → asking (labels read just now) → back; stale / error keep the card as it is.
  const [undo, setUndo] = useState<{ phase: "asking"; labels: string[] } | { phase: "stale" | "error" | "busy" } | null>(null);

  // Preview (specs/076 FR-070..072): the button exists only when the overlay is mounted AND an editor paints this kind.
  const overlay = useProposalOverlay();
  const change = proposalKey ? proposedFromItem(item, proposalKey) : null;
  const canPreview = !!overlay && !!change && overlay.canPreview(change);
  const preview = overlay?.active && proposalKey && overlay.active.change.proposalKey === proposalKey ? overlay.active : null;
  const previewing = !!preview;
  const clearPreview = overlay?.clear;

  // Applying makes the ghost real: scroll to what is now on the page once, then drop the ghost.
  useEffect(() => {
    if (st === "done" && previewing) {
      document.querySelector<HTMLElement>(`[data-proposal-key="${CSS.escape(proposalKey ?? "")}"]`)?.scrollIntoView({ block: "nearest" });
      clearPreview?.();
    }
  }, [st, previewing, proposalKey, clearPreview]);

  // After the user's own Áp dụng / Bỏ / Thử lại lands, focus goes to the card — never to <body>. A change nobody clicked
  // here (Áp dụng tất cả) must not pull focus around.
  useEffect(() => {
    if (acted.current && (st === "done" || st === "warn" || st === "error" || st === "discarded" || st === "undone")) {
      acted.current = false;
      cardRef.current?.focus();
    }
  }, [st]);

  // Cancelling an inline question (delete, "Áp ngay", Hoàn tác) hands focus back to the button that opened it (it was unmounted meanwhile).
  const questionOpen = confirming || liveAsk || undo?.phase === "asking" || undo?.phase === "busy";
  useEffect(() => {
    if (!questionOpen && backToRoot.current) {
      backToRoot.current = false;
      (applyRef.current ?? undoRef.current)?.focus();
    }
  }, [questionOpen]);

  if (!view) {
    return (
      <article ref={cardRef} tabIndex={-1} className="slds-card" style={{ border: "1px dashed var(--slds-g-color-neutral-base-60)" }} aria-label={t("studio.assistant.card.unknown")}>
        <div className="slds-card__body slds-card__body_inner slds-grid">
          <HelpCircle className="nx-lucide slds-m-right_x-small slds-no-flex" width={16} height={16} aria-hidden />
          <div>
            <p className="slds-text-body_regular">{t("studio.assistant.card.unknown")}</p>
            <p className="slds-text-body_regular slds-text-color_weak">{t("studio.assistant.card.unknownHint")}</p>
            {st === "error" && <p role="alert" className="slds-m-top_x-small"><StatusText tone="error">{state?.message}</StatusText></p>}
          </div>
        </div>
      </article>
    );
  }

  const dd = view.destructive;
  const open = st === "idle" || st === "applying" || st === "error";
  const lead: { tone: StatusTone; text: string; stateKey: I18nKey } =
    st === "done" ? { tone: "success", text: t(dd ? "studio.assistant.card.deletedShort" : "studio.assistant.card.state.done"), stateKey: "studio.assistant.card.state.done" }
    : st === "warn" ? { tone: "warn", text: t("studio.assistant.card.state.warn"), stateKey: "studio.assistant.card.state.warn" }
    : st === "discarded" ? { tone: "weak", text: t("studio.assistant.card.state.discarded"), stateKey: "studio.assistant.card.state.discarded" }
    : st === "undone" ? { tone: "weak", text: t("studio.assistant.card.state.undone"), stateKey: "studio.assistant.card.state.undone" }
    : st === "sealed" ? { tone: "weak", text: t("studio.assistant.card.state.sealed"), stateKey: "studio.assistant.card.state.sealed" }
    : { tone: "proposed", text: view.op ? `${t("studio.assistant.card.lead")} · ${t(OP_WORD[view.op])}` : t("studio.assistant.card.lead"), stateKey: st === "applying" ? "studio.assistant.card.state.applying" : "studio.assistant.card.state.open" };

  const done = proposalDone(item, state?.createdId, link);
  const warn = proposalWarnLink(item, link);
  const de = getDeleteEntityDraft(item);
  const df = getDeleteFieldDraft(item);
  const retypeOk = !de || typed === de.entity;

  const mode = draft?.mode;
  const doApply = () => { acted.current = true; setConfirming(false); setLiveAsk(false); setTyped(""); onApply(); };
  const realRows = item.kind === "records" && needsLiveConfirm(item, !!delivered, asSample); // records written as real data on a delivered project
  const startApply = () => (dd || (!mode && realRows) ? setConfirming(true) : mode?.confirm ? setLiveAsk(true) : doApply());
  const cancelConfirm = () => { backToRoot.current = true; setConfirming(false); setLiveAsk(false); setTyped(""); };

  const askUndo = async () => {
    if (!draft) return;
    backToRoot.current = true;
    setUndo({ phase: "busy" });
    const plan = await draft.onUndoPlan();
    setUndo(typeof plan === "string" ? { phase: plan } : { phase: "asking", labels: plan.labels });
  };
  const doUndo = async () => {
    if (!draft) return;
    acted.current = true;
    setUndo({ phase: "busy" });
    backToRoot.current = true; // a failed undo brings the Hoàn tác button back: focus returns to it
    const r = await draft.onUndo();
    setUndo(r === "undone" ? null : { phase: r });
  };
  const openDrafts = draft && (
    <Button variant="base" onClick={draft.onOpenDrafts}>{t("studio.assistant.draft.openDrafts")}</Button>
  );

  const side = (title: string, lines: string[]) => (
    <div className="slds-col">
      <p className="slds-text-title_caps slds-text-color_weak">{title}</p>
      {lines.length === 0
        ? <p className="slds-text-body_regular slds-text-color_weak">{t("studio.assistant.change.empty")}</p>
        : lines.map((l, i) => <p key={i} className="slds-text-body_regular">{l}</p>)}
    </div>
  );

  return (
    <article
      ref={cardRef}
      tabIndex={-1}
      aria-label={tf("studio.assistant.card.label", { state: t(lead.stateKey), title: view.title })}
      className={previewing ? "slds-card slds-card_boundary nx-proposed" : "slds-card slds-card_boundary"}
    >
      <div className="slds-card__body slds-card__body_inner">
        <p className="slds-grid slds-grid_vertical-align-center slds-text-body_regular">
          <Sparkles className="nx-lucide slds-m-right_xx-small" width={14} height={14} aria-hidden />
          <StatusText tone={lead.tone}>{lead.text}</StatusText>
        </p>
        <h3 className="slds-text-heading_small slds-m-top_xx-small">{view.title}</h3>
        <p className="slds-text-body_regular slds-text-color_weak">{view.where}</p>
        {item.entity?.appName && (
          <p className="slds-text-body_regular slds-text-color_weak">{tf("studio.assistant.preview.intoMenu", { app: item.entity.appName })}</p>
        )}

        {st !== "discarded" && st !== "undone" && (
          <>
            <div className="slds-m-top_small">
              <ChangeList items={view.changes} tone={open ? "proposed" : "neutral"} label={t("studio.assistant.change.list")} />
              {view.moreCount > 0 && (
                <p className="slds-text-body_regular slds-text-color_weak slds-m-top_xx-small">{tf("studio.assistant.change.more", { n: view.moreCount })}</p>
              )}
            </div>

            {view.before && (
              <div className="slds-grid slds-grid_vertical-stretch slds-m-top_small" data-testid="draft-change">
                {side(t("studio.assistant.change.before"), view.before)}
                {view.after && side(t("studio.assistant.change.after"), view.after)}
              </div>
            )}
            {reopened && view.op && view.op !== "create" && (
              <p className="slds-text-body_regular slds-text-color_weak">{t("studio.assistant.change.stale")}</p>
            )}

            {view.table && (
              <div className="slds-m-top_small">
                {open && (
                  <Checkbox
                    label={t("studio.assistant.sample.label")}
                    help={t("studio.assistant.sample.help")}
                    checked={asSample}
                    onChange={(e) => onAsSampleChange(e.target.checked)}
                  />
                )}
                <div className="slds-scrollable_x">
                  <table className="slds-table slds-table_bordered slds-table_cell-buffer slds-no-row-hover">
                    <thead>
                      <tr className="slds-line-height_reset">
                        {view.table.columns.map((c) => <th key={c} scope="col"><div className="slds-truncate">{c}</div></th>)}
                      </tr>
                    </thead>
                    <tbody>
                      {view.table.rows.map((r, i) => (
                        <tr key={i}>{r.map((c, j) => <td key={j}><div className="slds-truncate">{c}</div></td>)}</tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                {view.table.moreCount > 0 && (
                  <p className="slds-text-body_regular slds-text-color_weak">{tf("studio.assistant.table.more", { n: view.table.moreCount, total: view.table.total })}</p>
                )}
              </div>
            )}

            {dd && open && <p className="slds-text-body_regular slds-m-top_small"><StatusText tone="error">{t("studio.assistant.confirm.notInAll")}</StatusText></p>}
          </>
        )}

        {open && mode?.note && (
          <p role="status" className="slds-grid slds-grid_vertical-align-center slds-m-top_small slds-text-body_regular">
            <span className="slds-col"><StatusText tone={mode.note === "unknown" ? "warn" : "weak"}>{t(mode.note === "unknown" ? "studio.assistant.draft.unknown" : "studio.assistant.draft.checking")}</StatusText></span>
            {mode.recheck && draft && <Button variant="base" className="slds-m-left_small" onClick={draft.onRecheck}>{t("studio.assistant.draft.recheck")}</Button>}
          </p>
        )}

        {open && !confirming && !liveAsk && (
          <div className="slds-grid slds-grid_align-end slds-m-top_small">
            <Button ref={applyRef} variant={dd ? "destructive" : "brand"} disabled={st === "applying" || !!mode?.disabled} onClick={startApply}>
              {st === "applying" ? <><Spinner size="xx-small" inline variant="inverse" className="slds-m-right_x-small" label={t("studio.assistant.card.applying")} />{t("studio.assistant.card.applying")}</>
                : st === "error" ? t("studio.assistant.card.retry")
                : dd ? t("studio.assistant.card.deleteAction")
                : t(mode?.label ?? "studio.assistant.card.apply")}
            </Button>
            {canPreview && change && (
              <Button className="slds-m-left_x-small" variant="neutral" aria-pressed={previewing} disabled={st === "applying"} onClick={() => overlay.toggle(change)}>
                {t(previewing ? "studio.assistant.preview.hide" : "studio.assistant.preview.show")}
              </Button>
            )}
            <Button className="slds-m-left_x-small" disabled={st === "applying"} onClick={() => { acted.current = true; clearPreview?.(); onDiscard(); }}>
              {t("studio.assistant.card.discard")}
            </Button>
          </div>
        )}

        {open && preview && (
          <div role="status" className="slds-m-top_x-small slds-text-body_regular slds-text-color_weak">
            <p className="slds-grid slds-grid_vertical-align-center">
              <Eye className="nx-lucide slds-m-right_xx-small slds-no-flex" width={14} height={14} aria-hidden />
              {tf("studio.assistant.preview.highlighting", { screen: preview.screen })}
            </p>
            {preview.opened && <p>{tf("studio.assistant.preview.opened", { where: preview.opened })}</p>}
          </div>
        )}

        {open && liveAsk && !dd && (
          <div className="slds-m-top_small">
            <InlineConfirm
              tone="warning"
              message={realRows ? t("studio.assistant.sample.confirmReal") : tf("studio.assistant.draft.confirmNow", { desc: view.title })}
              confirmLabel={realRows ? t("studio.assistant.sample.confirmRealAction") : t("studio.assistant.draft.confirmNowAction")}
              onConfirm={doApply}
              onCancel={cancelConfirm}
            />
          </div>
        )}

        {open && confirming && (
          <div className="slds-m-top_small">
            <InlineConfirm
              tone="error"
              message={realRows ? t("studio.assistant.sample.confirmReal") : de
                ? tf("studio.assistant.confirm.entity", { label: de.label || de.entity, n: de.fieldCount })
                : tf("studio.assistant.confirm.field", { label: df?.label || df?.field || "", entity: df?.entity ?? "" })}
              confirmLabel={realRows ? t("studio.assistant.sample.confirmRealAction") : t(de ? "studio.assistant.confirm.entityAction" : "studio.assistant.confirm.fieldAction")}
              confirmDisabled={!retypeOk}
              onConfirm={doApply}
              onCancel={cancelConfirm}
            >
              {de && (
                <Input
                  label={tf("studio.assistant.confirm.typeName", { name: de.entity })}
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                  autoComplete="off"
                />
              )}
            </InlineConfirm>
          </div>
        )}

        {st === "error" && (
          <p role="alert" className="slds-m-top_small"><StatusText tone="error">{state?.message}</StatusText></p>
        )}
        {st === "done" && (
          <div role="status" className="slds-grid slds-grid_vertical-align-center slds-m-top_small slds-text-body_regular">
            <Check className="nx-lucide slds-m-right_xx-small slds-no-flex" width={14} height={14} aria-hidden />
            <span className="slds-col">{done.text}{state?.message ? ` ${state.message}` : ""}</span>
            <Link href={done.href} className="slds-m-left_small">{done.label}</Link>
            {draft && state?.changeIds?.length && (!undo || undo.phase === "error" || undo.phase === "stale") ? (
              <Button ref={undoRef} variant="base" className="slds-m-left_small" onClick={askUndo}>{t("studio.assistant.draft.undo")}</Button>
            ) : null}
          </div>
        )}
        {st === "sealed" && state?.sealedVersion != null && (
          <p role="status" className="slds-text-body_regular slds-m-top_small">{tf("studio.assistant.draft.sealed", { n: state.sealedVersion })}</p>
        )}
        {draft && (st === "done" || st === "warn") && undo?.phase === "asking" && (
          <div className="slds-m-top_small">
            <InlineConfirm
              tone="warning"
              message={tf("studio.assistant.draft.undoConfirm", { labels: undo.labels.join(", ") })}
              confirmLabel={t("studio.assistant.draft.undoAction")}
              cancelLabel={t("studio.assistant.draft.undoKeep")}
              onConfirm={doUndo}
              onCancel={() => { backToRoot.current = true; setUndo(null); }}
            />
          </div>
        )}
        {draft && undo?.phase === "busy" && (
          <p role="status" className="slds-text-body_regular slds-m-top_small"><Spinner size="xx-small" inline className="slds-m-right_x-small" label={t("studio.assistant.card.applying")} />{t("studio.assistant.draft.undo")}…</p>
        )}
        {draft && undo?.phase === "stale" && (
          <p role="alert" className="slds-text-body_regular slds-m-top_small">
            <StatusText tone="warn">{t("studio.assistant.draft.undoStale")}</StatusText>{draft.canOpenDrafts && <> {openDrafts}</>}
          </p>
        )}
        {draft && undo?.phase === "error" && (
          <p role="alert" className="slds-text-body_regular slds-m-top_small"><StatusText tone="error">{t("studio.assistant.draft.undoFailed")}</StatusText></p>
        )}
        {draft && (st === "done" || st === "warn") && state?.noUndo === "seen" && (
          <p className="slds-text-body_regular slds-text-color_weak slds-m-top_xx-small">
            {draft.canOpenDrafts ? openDrafts : t("studio.assistant.draft.alreadyDrafted")}
          </p>
        )}
        {st === "warn" && (
          <div role="status" className="slds-grid slds-grid_vertical-align-center slds-m-top_small slds-text-body_regular">
            <span className="slds-col">{state?.message}</span>
            {warn && <Link href={warn.href} className="slds-m-left_small">{warn.label}</Link>}
          </div>
        )}
        {(st === "done" || st === "warn") && state?.foot && (
          <p data-testid="ai-draft-foot" className="slds-text-body_regular slds-text-color_weak slds-m-top_xx-small">{t(state.foot)}</p>
        )}
      </div>
    </article>
  );
}
