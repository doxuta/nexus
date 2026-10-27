"use client";

// AssistantProvider — the assistant of one project frame (specs/076 ai3, 067:T093). Mounted ONCE by ProjectFrame inside
// ContextGate and keyed by the frame's `key={slug}`: the conversation lives here, so a step change never loses it and a
// project change drops it. It owns: the chat flow (useAssistantChat), what was applied, the open state, the "needs
// attention" dot on the header button, and the SCOPE GATE (FR-108, FR-128): `assertScope` is asked right before every
// send and every write, and answers false while a context switch is in flight, when the token in the tab stopped being
// this project's, or in a view-as-role tab. A late result after such a change is dropped by the hook (turnGen).
// The record button ("Hỏi trợ lý", EntityPage) reaches `ask(entity, recordId)` through `useAskAssistant()`; it sends an
// ID only — the server reads the row under the maker's own ACL.
import { useSearchParams } from "next/navigation";
import { useQueryClient } from "@tanstack/react-query";
import {
  createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode,
} from "react";
import { Sparkles } from "lucide-react";
import { adminApp, adminApps, aiHealth, getAiSession, listEntities, settleAiProposal } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { hasDrafts, newTurnId, draftApplyRank, isDestructiveDraft, serverMessagesToTurns, type AiDraftItem, type ChatTurn } from "@/lib/aiBuilder";
import { applyTracked, cardMode, deliveryOf, planUndo, proposalKey, proposalPlace, undoProposal, type Delivery } from "@/lib/aiApply";
import { isTerminal, loadApplyState, saveApplyState, type ApplyState } from "@/lib/aiApplyState";
import { invalidateAfterApply } from "@/lib/aiInvalidate";
import { ASSISTANT_OPEN_EVENT, setAssistantOpen, toggleAssistant, useAssistantOpen } from "@/lib/assistantOpen";
import { t } from "@/lib/i18n";
import { getPreviewToken } from "@/lib/previewSession";
import { homeSlug, sessionReadable, transitionPhase, useStudioIdentity, workspaceOf } from "@/lib/studioContext";
import { assistantEligible, resolveAppForEntity, studioAiContext } from "@/lib/studioAiContext";
import type { StudioRoute } from "@/lib/studioRoutes";
import { usePublishState } from "../usePublishState";
import { useAssistantChat } from "./useAssistantChat";

// How often an open MCP thread re-reads itself, and after how long without a new card it stops (issue #22).
const FOLLOW_POLL_MS = 5_000;
const FOLLOW_IDLE_MS = 10 * 60_000;
/** A turn this tab sent (a read-back turn is m_<seq>, serverMessagesToTurns). */
const ownTurn = (x: ChatTurn) => !x.id.startsWith("m_");

type Ask = (entity: string, recordId: string) => void;
const AskCtx = createContext<Ask | null>(null);
/** null = no assistant on this page (outside the project frame, or the assistant is off) ⇒ callers must not draw the button. */
export const useAskAssistant = (): Ask | null => useContext(AskCtx);

type OpenThread = (sessionId: string, liveUntil?: string) => Promise<void>;
const OpenCtx = createContext<OpenThread | null>(null);
/** Opens a saved conversation in the panel (an MCP agent's thread: issue #3); null where no panel mounts. Rejects when it cannot be read.
 *  liveUntil = the expiry of the agent key bound to the thread: until then the open thread follows what the agent adds (issue #22). */
export const useOpenThread = (): OpenThread | null => useContext(OpenCtx);

export type Attention = "working" | "proposal" | null;

export interface AssistantValue {
  slug: string;
  route: StudioRoute | null;
  projectName: string;
  eligible: boolean;
  /** The server reports the assistant off (aiHealth) or a turn came back AI_DISABLED. */
  off: boolean;
  chat: ReturnType<typeof useAssistantChat>;
  applyMap: Record<string, ApplyState>;
  setApply: (key: string, s: ApplyState) => void;
  asSample: boolean;
  setAsSample: (v: boolean) => void;
  /** Fail-closed tri-state (FR-130): a card never assumes "not delivered" from an error. */
  delivery: Delivery;
  recheck: () => void;
  undoPlan: (turnId: string, index: number) => Promise<{ labels: string[] } | "stale" | "error">;
  undoCard: (turnId: string, index: number) => Promise<"undone" | "stale" | "error">;
  applyDraft: (turnId: string, index: number, item: AiDraftItem) => Promise<void>;
  applyAll: (turnId: string, drafts: AiDraftItem[]) => Promise<void>;
  send: (text: string) => void;
  retry: () => void;
  newChat: () => void;
  assertScope: () => boolean;
  attention: Attention;
}

const AssistantCtx = createContext<AssistantValue | null>(null);
export function useAssistant(): AssistantValue {
  const v = useContext(AssistantCtx);
  if (!v) throw new Error("useAssistant outside AssistantProvider");
  return v;
}

interface Props {
  slug: string;
  projectName: string;
  route: StudioRoute | null;
  params: { bang?: string; id?: string };
  /** The panel itself (AssistantMount) — drawn beside the page. */
  panel: ReactNode;
  children: ReactNode;
}

export function AssistantProvider({ slug, projectName, route, params, panel, children }: Props) {
  const qc = useQueryClient();
  const identity = useStudioIdentity();
  const matches = identity.matches(slug);
  const eligible = assistantEligible(route, matches);
  const matchesRef = useRef(matches);
  useEffect(() => { matchesRef.current = matches; });

  // aiHealth never throws (unknown ⇒ on), so a blip cannot hide a working assistant. Not asked where the panel never mounts.
  const [aiOn, setAiOn] = useState(true);
  useEffect(() => {
    if (!eligible) return;
    let alive = true;
    void aiHealth().then((on) => { if (alive) setAiOn(on); });
    return () => { alive = false; };
  }, [eligible]);

  // The scope gate. Live reads (not the render's values): it is asked inside async callbacks.
  const assertScope = useCallback(() => {
    if (transitionPhase() !== "idle") return false;
    if (getPreviewToken()) return false; // view-as-role tab (FR-086): never writes
    if (!sessionReadable() || !matchesRef.current) return false;
    const w = workspaceOf();
    return w ? w.tenantSlug === slug : slug === homeSlug();
  }, [slug]);

  // The record belongs to the screen it was picked on: a different table drops it by construction.
  const [rec, setRec] = useState<{ entity: string; recordId: string } | null>(null);
  const sp = useSearchParams(); // the path parser never sees the query string
  const spKey = sp.toString();
  const query = useMemo(() => Object.fromEntries(new URLSearchParams(spKey)), [spKey]);
  const screenEntity = params.bang ?? query.bang;
  // No ?app on a table page => find the owning app (read only while the panel is open).
  const panelOpen = useAssistantOpen();
  const [found, setFound] = useState<{ entity: string; app?: string } | null>(null);
  const needApp = panelOpen && !!screenEntity && !query.app;
  useEffect(() => {
    if (!needApp || !screenEntity) return;
    let alive = true;
    void resolveAppForEntity(screenEntity, {
      apps: async () => (await adminApps()).map((a) => ({ id: a.id, name: a.name })),
      entityId: async (n) => (await listEntities()).find((e) => e.name === n)?.id,
      menu: async (id) => (await adminApp(id)).items,
    }).then((app) => { if (alive) setFound({ entity: screenEntity, app }); });
    return () => { alive = false; };
  }, [needApp, screenEntity]);
  const resolvedApp = found && found.entity === screenEntity ? found.app : undefined;
  const context = useMemo(
    () => studioAiContext(route, params, query, rec && rec.entity === screenEntity ? rec.recordId : undefined, resolvedApp),
    [route, params, query, rec, screenEntity, resolvedApp],
  );
  const onDropped = useCallback(() => setRec(null), []);
  const chat = useAssistantChat({ context, onContextDropped: onDropped, assertScope });
  const { turns, setTurns, sending, runTurn, reportApplyFailure, sessionId, sessionIdRef } = chat;

  const ask = useCallback<Ask>((entity, recordId) => {
    setRec({ entity, recordId });
    window.dispatchEvent(new Event(ASSISTANT_OPEN_EVENT));
  }, []);

  // ── what was applied (terminal outcomes survive a reload, per project + conversation) ──
  const [applyMap, setApplyMap] = useState<Record<string, ApplyState>>({});
  const setApply = useCallback((key: string, s: ApplyState) => {
    setApplyMap((m) => ({ ...m, [key]: s }));
    // Issue #42: a card of a saved thread that is applied, discarded or undone is settled on the server, so the thread's MCP
    // agent stops replaying it. Best effort: a miss leaves the agent judging against a stale card, and Apply re-validates.
    const at = proposalPlace(key);
    if (at && sessionId && isTerminal(s.status) && assertScope()) void settleAiProposal(sessionId, at.seq, at.index).catch(() => {});
  }, [sessionId, assertScope]);
  const [asSample, setAsSample] = useState(true);
  const publishQ = usePublishState(slug);
  const delivery = deliveryOf(publishQ);
  const recheck = useCallback(() => { void publishQ.refetch(); }, [publishQ]);
  const appliedCount = Object.values(applyMap).filter((a) => a.status === "done" || a.status === "warn").length;
  useEffect(() => { if (appliedCount > 0) emitDraftsChanged("aiBuilder"); }, [appliedCount]);
  useEffect(() => { if (sessionId && assertScope()) saveApplyState(slug, sessionId, applyMap); }, [slug, sessionId, applyMap, assertScope]);
  // The mode is read at the click: foot, draft rows and head version all describe that instant (lib/aiApply cardMode).
  const applyDraft = useCallback(async (turnId: string, index: number, item: AiDraftItem) => {
    const key = proposalKey(turnId, index);
    setApply(key, { status: "applying" });
    const { error, reportable, ...state } = await applyTracked(item, { asSample, assertScope }, cardMode(item, delivery, asSample));
    const landed = state.status === "done" || state.status === "warn";
    setApply(key, state);
    if (landed) invalidateAfterApply(qc);
    // The model can read and correct a 400/409/422 (FR-112); the hook sends it only when the scope still holds.
    if (error !== undefined && reportable) void reportApplyFailure(item, error);
  }, [delivery, asSample, assertScope, qc, reportApplyFailure, setApply]);

  // Hoàn tác (FR-082): names the rows read just now, deletes only the card's own changeIds.
  const undoPlan = useCallback(async (turnId: string, index: number) => {
    const st = applyMap[proposalKey(turnId, index)];
    if (!st) return "error" as const;
    const plan = await planUndo(st, { assertScope });
    return plan.ok ? { labels: plan.labels } : plan.reason === "stale" ? ("stale" as const) : ("error" as const);
  }, [applyMap, assertScope]);
  const undoCard = useCallback(async (turnId: string, index: number) => {
    const key = proposalKey(turnId, index);
    const st = applyMap[key];
    if (!st) return "error" as const;
    const r = await undoProposal(st, { assertScope });
    if (r.status === "undone") {
      setApply(key, { status: "undone" });
      invalidateAfterApply(qc);
      emitDraftsChanged("aiBuilder");
      return "undone" as const;
    }
    return r.status === "stale" ? ("stale" as const) : ("error" as const);
  }, [applyMap, assertScope, qc, setApply]);

  // Apply-all: dependency order, keep going after a failure; destructive, asked-in-place and unknown-delivery cards are never bulk-fired.
  // The gate is asked before EVERY write inside applyProposal, so a context switch between cards 1 and 2 stops the rest.
  const applyAll = useCallback(async (turnId: string, drafts: AiDraftItem[]) => {
    const order = drafts.map((_, i) => i).sort((a, b) => draftApplyRank(drafts[a].kind) - draftApplyRank(drafts[b].kind) || a - b);
    for (const i of order) {
      if (isDestructiveDraft(drafts[i]) || !cardMode(drafts[i], delivery, asSample).inApplyAll) continue;
      const st = applyMap[proposalKey(turnId, i)]?.status;
      if (st === "done" || st === "warn") continue;
      await applyDraft(turnId, i, drafts[i]);
    }
  }, [applyMap, delivery, asSample, applyDraft]);

  const send = useCallback((text: string) => {
    const userTurn: ChatTurn = { id: newTurnId(), role: "user", content: text };
    setTurns((prev) => [...prev, userTurn]);
    void runTurn(text);
  }, [setTurns, runTurn]);

  const retry = useCallback(() => {
    if (sending) return;
    const last = [...turns].reverse().find((x) => x.role === "user");
    if (last) void runTurn(last.content);
  }, [sending, turns, runTurn]);

  const newChat = useCallback(() => { chat.reset(); setApplyMap({}); }, [chat]);

  // A saved conversation as a resumed session: its turns and cards, and what this browser already applied of it.
  const [follow, setFollow] = useState<{ id: string; until: number } | null>(null);
  const openThread = useCallback<OpenThread>(async (id, liveUntil) => {
    const s = await getAiSession(id);
    chat.reset();
    chat.setTurns(serverMessagesToTurns(s.messages));
    chat.setActiveSession(s.id);
    setApplyMap(loadApplyState(slug, s.id));
    setFollow(liveUntil ? { id: s.id, until: Date.parse(liveUntil) } : null);
    setAssistantOpen(true);
  }, [chat, slug]);

  // Issue #22: an MCP agent adds cards to the thread its key is bound to; the open thread re-reads itself while that
  // key is live: on focus, and every FOLLOW_POLL_MS while the tab is visible until FOLLOW_IDLE_MS pass with nothing new
  // (focus wakes it). Read-back turns keep their ids (m_<seq>), so a card's apply state survives the re-read. A turn of
  // this tab's own stops it: its cards carry ids of this tab, which the server's copy would replace. That is judged
  // again when the read lands, on the turns as they are then: the builder may have sent one while it was out.
  const turnsRef = useRef(turns);
  useEffect(() => { turnsRef.current = turns; });
  const followId = follow && follow.id === sessionId ? follow.id : null;
  const followUntil = follow?.until ?? 0;
  useEffect(() => {
    if (!followId) return;
    let alive = true;
    let grew = Date.now();
    const read = async (woken: boolean) => {
      const now = Date.now();
      if (now >= followUntil) { setFollow(null); return; } // the key expired: nothing more will land here
      if (woken) grew = now;
      if (document.hidden || now - grew > FOLLOW_IDLE_MS) return;
      if (turnsRef.current.some(ownTurn)) return;
      const s = await getAiSession(followId).catch(() => null);
      if (!alive || !s || sessionIdRef.current !== followId || s.messages.length <= turnsRef.current.length) return;
      const next = serverMessagesToTurns(s.messages);
      setTurns((cur) => (cur.some(ownTurn) || next.length <= cur.length ? cur : next));
      grew = Date.now();
    };
    const poll = setInterval(() => void read(false), FOLLOW_POLL_MS);
    const wake = () => void read(true);
    window.addEventListener("focus", wake);
    document.addEventListener("visibilitychange", wake);
    return () => {
      alive = false;
      clearInterval(poll);
      window.removeEventListener("focus", wake);
      document.removeEventListener("visibilitychange", wake);
    };
  }, [followId, followUntil, sessionIdRef, setTurns]);

  // ── the dot on the header button while the panel is closed ──
  const open = useAssistantOpen();
  const proposalTurns = turns.filter((x) => x.role === "assistant" && hasDrafts(x.draft)).length;
  const [acked, setAcked] = useState(0);
  if (open && acked !== proposalTurns) setAcked(proposalTurns); // adjusting state while rendering, the documented pattern
  const attention: Attention = open ? null : sending ? "working" : proposalTurns > acked ? "proposal" : null;

  const off = !aiOn || chat.aiDisabled;
  const value: AssistantValue = {
    slug, route, projectName, eligible, off, chat, applyMap, setApply, asSample, setAsSample, delivery, recheck,
    undoPlan, undoCard, applyDraft, applyAll, send, retry, newChat, assertScope, attention,
  };

  return (
    <AssistantCtx.Provider value={value}>
      <AskCtx.Provider value={eligible && !off && route?.aiPanel ? ask : null}>
        <OpenCtx.Provider value={eligible ? openThread : null}>
          {eligible && (
            <div className="slds-grid slds-grid_align-end slds-m-bottom_x-small">
              <AssistantToggle attention={attention} open={open} />
            </div>
          )}
          <div className="flex flex-col lg:flex-row">
            <div className="min-w-0 flex-1">{children}</div>
            {eligible && panel}
          </div>
        </OpenCtx.Provider>
      </AskCtx.Provider>
    </AssistantCtx.Provider>
  );
}

function AssistantToggle({ attention, open }: { attention: Attention; open: boolean }) {
  const name = attention ? t(`studio.assistant.attention.${attention}`) : t("studio.assistant.toggle");
  return (
    <button
      type="button"
      className={`slds-button slds-button_${open ? "brand" : "neutral"}`}
      aria-pressed={open}
      aria-controls="assistant-panel"
      aria-label={name}
      onClick={toggleAssistant}
      data-testid="assistant-toggle"
    >
      <Sparkles className="slds-button__icon slds-button__icon_left nx-lucide" aria-hidden="true" />
      <span className="hidden sm:inline">{t("studio.assistant.toggle")}</span>
      {attention && <span className="slds-indicator_unread slds-m-left_x-small" aria-hidden="true" data-testid="assistant-dot" />}
    </button>
  );
}
