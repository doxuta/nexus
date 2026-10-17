"use client";

// AssistantPanel — the assistant box beside the project page (specs/076 ai3, UI-ARCH panels/docked, demo A).
// Not a chat app: no bubbles. A turn is a speaker label ("Anh" / "Trợ lý") and its words; proposals are ProposalCard
// blocks under the assistant's words; progress, Huỷ, errors and the disabled state use SLDS spinner / scoped notification.
// All behaviour (stream, Huỷ, errors, scope gate, apply) lives in the hook and AssistantProvider: this file is markup.
import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { useQuery } from "@tanstack/react-query";
import { Send } from "lucide-react";
import { Button } from "@/components/slds/Button";
import { Panel } from "@/components/slds/Panel";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { Textarea } from "@/components/slds/Textarea";
import { MarkdownLite } from "@/components/ui/MarkdownLite";
import { hasDrafts, isDestructiveDraft, type AiDraftItem, type ChatTurn } from "@/lib/aiBuilder";
import { cardMode, proposalKey, sealedVersion, type Delivery } from "@/lib/aiApply";
import type { ApplyState } from "@/lib/aiApplyState";
import { hasPublishReviewOpener, openPublishReview, watchPublishReviewOpener } from "@/lib/draftEvents";
import { accessTablesHidden, proposalView } from "@/lib/aiProposalView";
import { listEntities } from "@/lib/api";
import { resolveAssistantLink } from "@/lib/assistantLinks";
import { setAssistantOpen, useAssistantOpen } from "@/lib/assistantOpen";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { studioHref, type RouteId } from "@/lib/studioRoutes";
import { ProposalCard } from "./ProposalCard";
import { stepLabel, type StepSummary } from "./useAssistantChat";
import { useAssistant } from "./AssistantProvider";

type Tone = "default" | "ops" | "access" | "branding";
const TONE: Record<string, Tone> = {
  "van-hanh": "ops", "bao-tri": "ops", "nguoi-dung": "ops", "vai-tro": "access", "quyen-truong": "access", "thuong-hieu": "branding",
};

// A card applied before a later Publish is sealed: its draft row is part of a version now (nothing left to undo).
function sealed(st: ApplyState | undefined, d: Delivery): ApplyState | undefined {
  if (!st || (st.status !== "done" && st.status !== "warn")) return st;
  const v = sealedVersion(st, d);
  return v === null ? st : { ...st, status: "sealed", sealedVersion: v };
}

export function AssistantPanel() {
  const a = useAssistant();
  const { chat } = a;
  const open = useAssistantOpen();
  const [input, setInput] = useState("");
  const scroller = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  const tone: Tone = TONE[a.route?.aiPanel ?? ""] ?? "default";
  const label = a.route ? t(a.route.labelKey) : "";

  // Follow the newest words only while the maker is already at the bottom (reading above is never yanked away).
  useEffect(() => {
    const el = scroller.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [chat.turns, chat.sending, chat.steps, chat.cancelled, chat.turnError]);

  // Overlay mode (narrow): opening puts focus in the composer, the one thing the maker came to use (Esc returns it to the button).
  const composer = useRef<HTMLDivElement>(null);
  const wasOpen = useRef(open);
  useEffect(() => {
    if (open && !wasOpen.current && !window.matchMedia("(min-width: 1280px)").matches) composer.current?.querySelector("textarea")?.focus();
    wasOpen.current = open;
  }, [open]);

  const locked = a.off;
  const canSend = input.trim().length > 0 && !chat.sending && !locked;
  const submit = () => {
    if (!canSend) return;
    a.send(input.trim());
    setInput("");
  };

  // sticky only when docked (>=1280px): a sticky stacking context would trap the overlay panel under the page.
  return (
    <aside aria-label={t("studio.assistant.region")} data-testid="assistant-dock" className="flex xl:sticky xl:top-3 xl:self-start xl:max-h-[calc(100vh-1.5rem)]">
      <Panel
        id="assistant-panel"
        open={open}
        title={t("studio.assistant.title")}
        position="right"
        size="medium"
        responsive
        className="max-xl:w-[25rem]! max-sm:w-full!"
        onClose={() => setAssistantOpen(false)}
        actions={chat.turns.length > 0 ? (
          <Button variant="base" onClick={a.newChat} disabled={chat.sending}>{t("studio.assistant.newChat")}</Button>
        ) : undefined}
      >
        <div className="flex flex-col gap-3">
          <div className="slds-grid slds-grid_vertical-align-center slds-text-body_small">
            <p className="slds-col slds-truncate" title={tf("studio.assistant.context.viewing", { screen: label, project: a.projectName })}>
              {tf("studio.assistant.context.viewing", { screen: label, project: a.projectName })}
            </p>
            {chat.ctxView && (
              <Button variant="base" className="slds-col_bump-left slds-shrink-none" onClick={chat.dropContext}>
                {t("studio.assistant.context.clear")}
              </Button>
            )}
          </div>

          {locked && <ScopedNotification theme="info">{t("studio.assistant.disabledNotice")}</ScopedNotification>}

          <div
            ref={scroller}
            role="log"
            aria-live="polite"
            className="max-h-[55vh] overflow-y-auto"
            onScroll={(e) => {
              const el = e.currentTarget;
              pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
            }}
          >
            {chat.turns.length === 0 && !chat.sending && (
              <p className="slds-text-body_regular slds-text-color_weak">{t(`studio.assistant.intro.${tone}` as I18nKey)}</p>
            )}
            {chat.turns.map((turn) => (
              <Turn key={turn.id} turn={turn} summary={chat.stepSummaries[turn.id]} />
            ))}
            {chat.sending && <Progress />}
            {!chat.sending && chat.cancelled && <p className="slds-text-body_small slds-text-color_weak slds-m-top_small">{t("studio.assistant.cancelled")}</p>}
            {chat.turnError && (
              <div className="slds-m-top_small">
                <ScopedNotification theme={chat.turnError.kind === "limit" ? "warning" : "error"}>{chat.turnError.text}</ScopedNotification>
                <Button variant="neutral" className="slds-m-top_x-small" onClick={a.retry} disabled={chat.sending}>{t("studio.assistant.card.retry")}</Button>
              </div>
            )}
          </div>

          <div ref={composer}>
            <Textarea
              label={t("studio.assistant.title")}
              labelHidden
              name="assistantMessage"
              rows={3}
              value={input}
              disabled={locked}
              placeholder={t(`studio.assistant.placeholder.${tone === "ops" ? "ops" : "default"}` as I18nKey)}
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !e.shiftKey) {
                  e.preventDefault();
                  submit();
                }
              }}
              help={t("studio.assistant.composerHint")}
            />
            <div className="slds-m-top_x-small slds-text-align_right">
              <Button variant="brand" iconLeft={Send} aria-disabled={!canSend} onClick={submit}>{t("studio.assistant.send")}</Button>
            </div>
          </div>
        </div>
      </Panel>
    </aside>
  );
}

function Turn({ turn, summary }: { turn: ChatTurn; summary?: StepSummary }) {
  const a = useAssistant();
  const user = turn.role === "user";
  const reviewCanOpen = useSyncExternalStore(watchPublishReviewOpener, hasPublishReviewOpener, () => false);
  const drafts: AiDraftItem[] = !user && hasDrafts(turn.draft) ? turn.draft : [];
  // The Studio's own table list (the builder's draft-overlay read: draftState "new" = still hidden from the customer),
  // so an access card's question says "the table stays hidden" only while it is. The same cache as the project frame.
  const { data: entities } = useQuery({ queryKey: ["studio", "count", "entities"], queryFn: listEntities, staleTime: 60_000,
    enabled: drafts.some((d) => d.kind === "access") });
  const bulk = drafts.filter((d, i) => {
    const st = a.applyMap[proposalKey(turn.id, i)]?.status;
    return !isDestructiveDraft(d) && cardMode(d, a.delivery, a.asSample).inApplyAll && proposalView(d) !== null && st !== "done" && st !== "warn" && st !== "discarded";
  }).length;
  return (
    <div className="slds-m-top_small">
      <p className="slds-text-title_caps">{t(user ? "studio.assistant.speaker.user" : "studio.assistant.speaker.assistant")}</p>
      {user ? (
        <p className="slds-text-body_regular whitespace-pre-wrap">{turn.content}</p>
      ) : (
        <MarkdownLite variant="slds" text={turn.content} resolveHref={(h) => resolveAssistantLink(h, a.slug)} />
      )}
      {drafts.length > 0 && (
        <div className="slds-m-top_x-small flex flex-col gap-2">
          {drafts.length > 1 && (
            <div className="slds-grid slds-grid_vertical-align-center">
              <span className="slds-col slds-text-body_small slds-text-color_weak">{tf("studio.assistant.card.count", { n: drafts.length })}</span>
              {bulk > 0 && (
                <Button variant="outline-brand" onClick={() => void a.applyAll(turn.id, drafts)}>
                  {tf("studio.assistant.card.applyAll", { n: bulk })}
                </Button>
              )}
            </div>
          )}
          {drafts.map((item, i) => (
            <ProposalCard
              key={`${turn.id}:${i}`}
              item={item}
              proposalKey={proposalKey(turn.id, i)}
              state={sealed(a.applyMap[proposalKey(turn.id, i)], a.delivery)}
              delivered={a.delivery.kind === "delivered" ? true : a.delivery.kind === "undelivered" ? false : undefined}
              tableHidden={accessTablesHidden(item, entities)}
              draft={{
                mode: cardMode(item, a.delivery, a.asSample),
                onRecheck: a.recheck,
                onUndoPlan: () => a.undoPlan(turn.id, i),
                onUndo: () => a.undoCard(turn.id, i),
                canOpenDrafts: reviewCanOpen,
                onOpenDrafts: () => { openPublishReview(); },
              }}
              asSample={a.asSample}
              onAsSampleChange={a.setAsSample}
              onApply={() => void a.applyDraft(turn.id, i, item)}
              onDiscard={() => a.setApply(proposalKey(turn.id, i), { status: "discarded" })}
              reopened={turn.reopened}
              link={(id: RouteId) => studioHref(a.slug, id)}
            />
          ))}
        </div>
      )}
      {!user && (summary || turn.outcome === "max_steps") && (
        <p className="slds-text-body_small slds-text-color_weak slds-m-top_xx-small">
          {summary && summary.count > 0 ? `${summary.tools.join(" · ")}` : ""}
          {turn.outcome === "max_steps" ? ` ${t("studio.assistant.maxSteps")}` : ""}
        </p>
      )}
    </div>
  );
}

function Progress() {
  const { chat } = useAssistant();
  const last = chat.steps.length > 0 ? chat.steps[chat.steps.length - 1] : null;
  const line = !last || last.phase === "thinking" || last.phase === "tool_done"
    ? t("studio.assistant.thinking")
    : last.phase === "calling_tool" ? `${stepLabel(last.tool)}…` : t("studio.assistant.composing");
  return (
    <div className="slds-m-top_small slds-grid slds-grid_vertical-align-center">
      <div className="slds-is-relative slds-m-right_small" style={{ width: "1.5rem", height: "1.5rem" }}>
        <Spinner size="x-small" label={line} />
      </div>
      <p className="slds-col slds-truncate slds-text-body_small" title={line}>
        {line}{last ? ` · ${tf("studio.assistant.stepOf", { i: last.step, n: last.maxSteps })}` : ""}
      </p>
      <Button variant="base" className="slds-shrink-none" onClick={chat.cancel}>{t("studio.assistant.cancel")}</Button>
    </div>
  );
}
