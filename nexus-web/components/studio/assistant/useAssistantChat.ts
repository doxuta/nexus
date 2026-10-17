"use client";

// useAssistantChat — the assistant's ask/answer flow (specs/076 lát ai3a): stream, Huỷ, session id,
// error mapping, apply-failure report, usage/outcome per turn. Pure behaviour, no markup, so the
// the docked panel and ⌘K share ONE copy.
import { useState, useRef, useEffect, useCallback } from "react";
import { ApiError } from "@/lib/api";
import { aiBuilderChatStream, AiStreamCancelledError, type AiStepEvent } from "@/lib/aiBuilderStream";
import { errorMessage } from "@/lib/errorMessages";
import { t as tr } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import {
  newTurnId, aiLimitMessage,
  type ChatTurn, type AiDraftItem, type AiTurnContext, type AiTurnContextInput,
} from "@/lib/aiBuilder";

/** A failed/refused turn's inline notice: "limit" = transient 429 throttle, "error" = real failure. */
export interface TurnNotice { text: string; kind: "error" | "limit" }
/** The collapsed one-liner an assistant turn keeps after its stream finished. */
export interface StepSummary { count: number; tools: string[] }
/** What the "Đang dựng" chip shows. */
export interface ContextView {
  appName: string | null; appLabel: string | null; panel: string | null;
  entity: string | null; recordId: string | null;
  /** The server answered without `data.context` (backend older than #415): the context was NOT used. */
  unsent?: boolean;
}

export function viewFromInput(c?: AiTurnContextInput): ContextView | null {
  return c && (c.app || c.panel || c.entity)
    ? { appName: c.app ?? null, appLabel: null, panel: c.panel ?? null, entity: c.entity ?? null, recordId: c.recordId ?? null }
    : null;
}
export function viewFromServer(c?: AiTurnContext | null): ContextView | null {
  return c
    ? {
        appName: c.app?.name ?? null, appLabel: c.app?.label ?? null, panel: c.panel,
        entity: c.entity ? c.entity.label || c.entity.name : null, recordId: c.recordId ?? null,
      }
    : null;
}

const STEP_TOOLS = new Set([
  "list_entities", "list_fields", "list_relationships", "list_layouts", "list_roles", "list_packs",
  "snapshot_model", "explain_access", "get_layout", "list_workflows", "list_reports", "get_app",
]);
const STEP_KINDS = new Set([
  "entity", "field", "relationship", "layout", "workflow", "report", "app", "edit_field", "delete_field",
  "delete_entity", "records", "approval", "dashboard", "workqueue", "access",
]);

/** Business-language label for a tool the stream names (FR-116). Unknown tools keep their raw name. */
export function stepLabel(tool?: string): string {
  if (!tool) return tr("studio.assistant.step.tool");
  if (STEP_TOOLS.has(tool)) return tr(`studio.assistant.step.${tool}` as I18nKey);
  if (tool.startsWith("propose_")) {
    const kind = tool.slice(8);
    const k = STEP_KINDS.has(kind) ? tr(`studio.assistant.stepKind.${kind}` as I18nKey) : kind.replace(/_/g, " ");
    return tr("studio.assistant.step.propose").replace("{kind}", k);
  }
  return tool;
}

export interface UseAssistantChatOpts {
  /** The screen the maker opened the assistant from; a CHANGED value replaces the context. */
  context?: AiTurnContextInput;
  onContextDropped?: () => void;
  /** Called right before EVERY send; false ⇒ nothing leaves the page (FR-128). Default: always true. */
  assertScope?: () => boolean;
  /** A turn finished or the session vanished — the host refreshes its session list. */
  onSessionsChanged?: () => void;
}

export function useAssistantChat({ context, onContextDropped, assertScope, onSessionsChanged }: UseAssistantChatOpts = {}) {
  const contextRef = useRef<AiTurnContextInput | undefined>(context);
  const [ctxView, setCtxView] = useState<ContextView | null>(() => viewFromInput(context));
  const [turns, setTurns] = useState<ChatTurn[]>([]);
  const [sending, setSending] = useState(false);
  const [turnError, setTurnError] = useState<TurnNotice | null>(null);
  const [aiDisabled, setAiDisabled] = useState(false);
  const [steps, setSteps] = useState<AiStepEvent[]>([]);
  const [cancelled, setCancelled] = useState(false);
  const [stepSummaries, setStepSummaries] = useState<Record<string, StepSummary>>({});
  const [sessionId, setSessionId] = useState<string | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  // Mirrors sessionId so runTurn (a stable callback) reads the CURRENT id; setActiveSession is the only writer.
  const sessionIdRef = useRef<string | null>(null);
  // Bumped whenever a late result must be ignored (unmount, workspace ended, new context, new conversation).
  const turnGen = useRef(0);
  // Latest host callbacks, read at call time (kept out of render: refs are written in an effect).
  const cbs = useRef({ assertScope, onSessionsChanged, onContextDropped });
  useEffect(() => { cbs.current = { assertScope, onSessionsChanged, onContextDropped }; });

  const setActiveSession = useCallback((id: string | null) => {
    sessionIdRef.current = id;
    setSessionId(id);
  }, []);

  /** Abort the in-flight turn and orphan its result. */
  const invalidateTurn = useCallback(() => {
    turnGen.current += 1;
    abortRef.current?.abort();
    abortRef.current = null;
    setSending(false);
    setSteps([]);
  }, []);

  const ctxKey = JSON.stringify(context ?? null);
  const appliedKey = useRef(ctxKey);
  useEffect(() => {
    if (appliedKey.current === ctxKey) return;
    appliedKey.current = ctxKey;
    contextRef.current = context;
    setCtxView(viewFromInput(context));
    invalidateTurn();
  }, [ctxKey, context, invalidateTurn]);

  useEffect(() => {
    const onEnded = () => invalidateTurn();
    window.addEventListener("nexus:workspace-ended", onEnded);
    return () => {
      window.removeEventListener("nexus:workspace-ended", onEnded);
      turnGen.current += 1;
      abortRef.current?.abort();
    };
  }, [invalidateTurn]);

  const dropContext = useCallback(() => {
    contextRef.current = undefined;
    setCtxView(null);
    cbs.current.onContextDropped?.();
  }, []);

  // Send ONE new message with the current sessionId (server owns history). Returns false when it did not go out.
  const runTurn = useCallback(async (message: string): Promise<boolean> => {
    // abortRef is the in-flight lock (claimed synchronously): a second caller (e.g. two failing Apply
    // reports) must not start a concurrent turn and orphan the first one's Huỷ.
    if (abortRef.current) return false;
    if (cbs.current.assertScope && !cbs.current.assertScope()) return false;
    const gen = turnGen.current;
    const ctrl = new AbortController();
    abortRef.current = ctrl;
    setSending(true);
    setTurnError(null);
    setCancelled(false);
    setSteps([]);
    const stale = () => gen !== turnGen.current;
    const seen: AiStepEvent[] = [];
    try {
      const res = await aiBuilderChatStream(message, sessionIdRef.current ?? undefined, {
        signal: ctrl.signal,
        context: contextRef.current,
        onStep: (e) => {
          if (stale()) return;
          seen.push(e);
          setSteps((prev) => [...prev, e]);
        },
      });
      if (stale()) return true;
      if (ctrl.signal.aborted) {
        // The non-streaming fallback can't abort its fetch — a turn resolving AFTER Huỷ is discarded.
        setCancelled(true);
        return true;
      }
      setActiveSession(res.sessionId);
      if (contextRef.current) {
        const v = viewFromServer(res.context);
        setCtxView(v ?? (() => { const f = viewFromInput(contextRef.current); return f && { ...f, unsent: true }; })());
      }
      const assistant: ChatTurn = {
        id: newTurnId(), role: "assistant", content: res.reply, draft: res.draft, outcome: res.outcome, usage: res.usage,
      };
      setTurns((prev) => [...prev, assistant]);
      if (seen.length > 0) {
        const tools: string[] = [];
        for (const s of seen) {
          if (s.phase === "tool_done" && s.tool) {
            const label = stepLabel(s.tool);
            if (!tools.includes(label)) tools.push(label);
          }
        }
        setStepSummaries((m) => ({ ...m, [assistant.id]: { count: seen[seen.length - 1].step, tools } }));
      }
      cbs.current.onSessionsChanged?.();
    } catch (err) {
      if (stale()) return true;
      if (err instanceof AiStreamCancelledError || ctrl.signal.aborted) {
        setCancelled(true); // user Huỷ — neutral note, never the red notice
      } else if (err instanceof ApiError && err.code === "AI_DISABLED") {
        setAiDisabled(true);
      } else if (err instanceof ApiError && (err.code === "AI_CONTEXT_NOT_FOUND" || err.code === "AI_CONTEXT_INVALID")) {
        // Refused BEFORE any AI turn: drop the context so "Thử lại" never loops on it.
        dropContext();
        setTurnError({
          kind: "error",
          text: tr(err.code === "AI_CONTEXT_NOT_FOUND" ? "page.studio.ai.context.notFound" : "page.studio.ai.context.invalid"),
        });
      } else if (err instanceof ApiError && err.code === "SESSION_NOT_FOUND") {
        setActiveSession(null);
        setTurnError({ kind: "error", text: tr("studio.assistant.sessionGone") });
        cbs.current.onSessionsChanged?.();
      } else if (err instanceof ApiError && err.status === 429) {
        setTurnError({ kind: "limit", text: aiLimitMessage(err.code, err.details.retryAfter) ?? errorMessage(err) });
      } else {
        setTurnError({ kind: "error", text: errorMessage(err) });
      }
    } finally {
      if (!stale()) {
        abortRef.current = null;
        setSteps([]);
        setSending(false);
      }
    }
    return true;
  }, [setActiveSession, dropContext]);

  const cancel = useCallback(() => { abortRef.current?.abort(); }, []);

  /** Hand the real server error of a failed Apply back to the assistant (best-effort). */
  const reportApplyFailure = useCallback(async (item: AiDraftItem, err: unknown) => {
    const code = err instanceof ApiError ? err.code : "";
    const detail = err instanceof ApiError ? err.message : String(err);
    const name = item.entity?.name ?? item.field?.name ?? item.workflow?.name ?? item.report?.name ?? item.app?.name ?? "";
    try {
      await runTurn(
        `[hệ thống] Áp dụng đề xuất "${item.kind}${name ? " " + name : ""}" THẤT BẠI.` +
          (code ? ` Mã lỗi: ${code}.` : "") +
          ` Máy chủ trả: ${detail}. Hãy sửa đề xuất cho đúng rồi đề xuất lại.`,
      );
    } catch {
      /* reporting is best-effort — the red chip already told the human */
    }
  }, [runTurn]);

  /** "Cuộc trò chuyện mới": orphan any late result and clear the conversation. */
  const reset = useCallback(() => {
    invalidateTurn();
    setActiveSession(null);
    setTurns([]);
    setStepSummaries({});
    setCancelled(false);
    setTurnError(null);
  }, [invalidateTurn, setActiveSession]);

  return {
    turns, setTurns, sending, turnError, setTurnError, aiDisabled, steps, cancelled, setCancelled,
    stepSummaries, setStepSummaries, sessionId, sessionIdRef, setActiveSession,
    ctxView, setCtxView, dropContext, runTurn, cancel, reportApplyFailure, reset,
  };
}
