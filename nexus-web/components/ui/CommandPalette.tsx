"use client";

// CommandPalette — ⌘K v2 (W3, plan §2.1) — migrated onto cmdk (list/keyboard/aria), keeping
// every W0 win: stripVN diacritic-insensitive match, guarded router.push SPA nav, /settings
// entry, App palette carries ZERO /studio/* links EXCEPT the single sanctioned bridge
// command "Chuyển sang Studio" (admin-only).
//
// W3 additions:
//   • Recents-first — last 5 visited routes (per-user localStorage, lib/recents.ts).
//   • Action commands — đổi giao diện sáng/tối, đăng xuất, tạo bản ghi (pinned/current first).
//   • Cross-entity record search — query ≥2 chars fans out GET /:entity?q=&limit=3 to the
//     top-5 accessible entities (current + pinned first; lib/paletteSearch.ts), debounce
//     250ms, stale responses dropped by sequence guard. Honest loading + empty states.
//   • "/" opens the palette in search mode when not typing in an input (useCommandPalette).
//
// ⌘K assistant (ask-data): any non-empty query also offers "Hỏi AI: …" — selecting it swaps the
// list for an inline answer panel (POST /assistant/ask, markdown reply via the shared XSS-safe
// MarkdownLite, session kept for follow-ups). EVERY user gets this (not admin-gated): the
// assistant's read tools run under the caller's own RecordScope/ACL server-side.
//
// ⌘K-can-act (EASE Nước 7): an ask turn may return drafts[] (record_create/record_update) —
// rendered as HITL confirm cards under the reply. NOTHING auto-applies: "Xác nhận & lưu" runs the
// NORMAL createRecord/updateRecord under the user's own token (422/403/409 surface as the same
// honest ApiErrors the manual form gets). "Phiên trước" resumes a persisted thread (user-scoped
// /assistant/sessions rail) — its drafts re-render still actionable.
//
// shouldFilter={false}: cmdk's built-in scorer can't do VN-diacritic folding and would
// wrongly filter the async record group — we filter ourselves with stripVN (same as W0).

import { useEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { Command } from "cmdk";
import { clsx } from "clsx";
import {
  Search, Boxes, LayoutGrid, Plus, CornerDownLeft, Loader2,
  Home, FileBarChart2, CheckSquare, ShieldCheck, ScrollText, ArrowRight, Settings, Settings2,
  CircleHelp, Bell, History, LogOut, Wrench, FileText,
  Sparkles, ArrowUp, AlertCircle, Clock, Check, Trash2,
} from "lucide-react";
import { usePathname } from "next/navigation";
import { useQueryClient } from "@tanstack/react-query";
import { useGuardedRouter } from "@/lib/navigation-blocker";
import { useEntities } from "@/lib/metadata/hooks";
import { usePermissions } from "@/lib/metadata/permissions";
import { useAuth } from "@/lib/auth/AuthProvider";
import {
  searchAll, assistantAsk, aiHealth, createRecord, updateRecord,
  listAssistantSessions, getAssistantSession, deleteAssistantSession, type EntityMeta,
} from "@/lib/api";
import {
  assistantNotice, assistantMessagesToTurns, askReplyText, getRecordCreateDraft, getRecordUpdateDraft,
  assistantDraftValueText,
  type AssistantNotice, type AssistantDraft, type AssistantTurn, type AssistantSessionSummary,
} from "@/lib/assistant";
import { newTurnId } from "@/lib/aiBuilder";
import { errorMessage } from "@/lib/errorMessages";
import { formatSaveError } from "@/lib/saveErrors";
import { relTime } from "@/lib/workflowRuns";
import { qk } from "@/lib/query/keys";
import { MarkdownLite } from "@/components/ui/MarkdownLite";
import { usePins } from "@/lib/pins";
import { ASSISTANT_OPEN_EVENT } from "@/lib/assistantOpen";
import { t } from "@/lib/i18n";
import { ROUTES, studioHref } from "@/lib/studioRoutes";
import { projectSlugAt } from "@/lib/studioContext";
import { loadRecentRoutes, type RecentRoute } from "@/lib/recents";
import {
  shapeServerSearch, type RecordHitGroup,
  SEARCH_MIN_CHARS, SEARCH_DEBOUNCE_MS,
} from "@/lib/paletteSearch";

// Bỏ dấu tiếng Việt + lowercase để match diacritic-insensitive.
// NFD tách dấu tổ hợp (á → a + ́) rồi strip; đ/Đ không phân rã qua NFD nên thay riêng.
function stripVN(s: string): string {
  return s
    .normalize("NFD")
    .replace(/[\u0300-\u036f]/g, "")
    .replace(/đ/g, "d")
    .replace(/Đ/g, "D")
    .toLowerCase();
}

type NavCmd = {
  id: string;
  label: string;
  group: string;
  href: string;
  icon: typeof Boxes;
  hint?: string;
  /** Fires this window event instead of navigating (the docked assistant listens to nexus:assistant-open). */
  event?: string;
};

// Per-draft-card apply state, keyed `${turnId}:${index}`. "done" keeps the saved record id for the
// "Mở bản ghi" link; "dismissed" keeps the card visible but muted (honest — nothing vanished).
type AskDraftState = { status: "saving" | "done" | "dismissed" | "error"; message?: string; savedId?: string };

function buildStaticCmds(isAdmin: boolean, inStudio: boolean, slug: string): NavCmd[] {
  const nav: NavCmd[] = [
    { id: "nav-home", label: "Trang chủ", group: "Điều hướng", href: "/home", icon: Home },
    { id: "nav-dashboards", label: "Bảng điều khiển", group: "Điều hướng", href: "/dashboards", icon: LayoutGrid },
    { id: "nav-reports", label: "Báo cáo", group: "Điều hướng", href: "/reports", icon: FileBarChart2 },
    { id: "nav-approvals", label: "Phê duyệt", group: "Điều hướng", href: "/approvals", icon: CheckSquare },
    { id: "nav-notifications", label: "Thông báo", group: "Điều hướng", href: "/notifications", icon: Bell },
    { id: "nav-profile", label: "Hồ sơ & Mật khẩu", group: "Cài đặt", href: "/profile", icon: Settings },
    // W0-QW6: /settings hết mồ côi — vào được từ ⌘K (+ user-menu Topbar)
    { id: "nav-preferences", label: "Tuỳ chọn", group: "Cài đặt", href: "/settings", icon: Settings2 },
    { id: "nav-about", label: "Giới thiệu Nexus", group: "Cài đặt", href: "/about", icon: CircleHelp },
  ];
  if (isAdmin) {
    nav.push(
      { id: "nav-settings", label: "Phân quyền", group: "Cài đặt", href: "/admin/roles", icon: ShieldCheck },
      // Customer-governance screens were unreachable from ⌘K (typing "người dùng" found nothing).
      { id: "nav-admin-users", label: "Người dùng", group: "Cài đặt", href: "/admin/users", icon: Settings },
      { id: "nav-admin-teams", label: "Nhóm", group: "Cài đặt", href: "/admin/teams", icon: Settings },
      { id: "nav-admin-fieldacl", label: "Phân quyền Field", group: "Cài đặt", href: "/admin/field-acl", icon: ShieldCheck },
      { id: "nav-audit", label: "Nhật ký hệ thống", group: "Cài đặt", href: "/admin/audit", icon: ScrollText },
    );
  }
  // Inside Studio, ⌘K used to offer ONLY app-face destinations — the product's own search
  // teleported builders out of the builder. Add the studio tools when standing in /studio.
  if (isAdmin && inStudio) {
    // Every page of the project that has a real screen (ROUTES.mounted), named by the same i18n label the
    // page's own header uses: ⌘K cannot drift from the route table (specs/073 kh6).
    const studio: [string, string][] = [
      ["/studio", t("studio.projects.title")], [studioHref(slug, "dung/du-lieu"), t("studio.project.home")],
      ...ROUTES.filter((x) => x.mounted && x.ctx === "phien" && x.params.length === 0 && x.id !== "dung/du-lieu")
        .map((x): [string, string] => [studioHref(slug, x.id), t(x.labelKey)]),
      ["/studio/verticals", "Cài mẫu & Bàn giao"],
    ];
    nav.push(...studio.map(([href, label]) => ({
      id: `nav-studio-${href.slice(1).replace(/\//g, "-")}`, label, group: "Studio", href, icon: Wrench,
    })));
    // One assistant: the docked panel. Offered inside a project only (the panel mounts nowhere else).
    if (slug) nav.push({ id: "nav-studio-assistant", label: "Mở trợ lý", group: "Studio", href: "", icon: Sparkles, event: ASSISTANT_OPEN_EVENT });
  }
  return nav;
}

export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [q, setQ] = useState("");
  const router = useGuardedRouter();
  const pathname = usePathname();
  const { user, logout } = useAuth();
  const isAdmin = user?.isAdmin ?? false;
  // EASE Nước 6: the Builder face needs the platform-operator flag, not mere tenant-admin.
  const isOperator = !!user?.isPlatformOperator;
  const { data: entities = [] } = useEntities();
  const { can } = usePermissions();
  const { pins } = usePins();
  const [recents, setRecents] = useState<RecentRoute[]>([]);

  // Cross-entity record search state
  const [recordGroups, setRecordGroups] = useState<RecordHitGroup[] | null>(null);
  const [searching, setSearching] = useState(false);
  // "no results" and "the request failed" must not render the same sentence — see the catch below.
  const [searchFailed, setSearchFailed] = useState(false);
  const seqRef = useRef(0);

  // "Hỏi AI" ask-data panel state (⌘K assistant — every user, ACL enforced server-side).
  // sessionId lives for the lifetime of the open panel so follow-ups stay in one thread.
  const [askOpen, setAskOpen] = useState(false);
  const [askTurns, setAskTurns] = useState<AssistantTurn[]>([]);
  const [askLoading, setAskLoading] = useState(false);
  const [askNotice, setAskNotice] = useState<AssistantNotice | null>(null);
  const [askInput, setAskInput] = useState("");
  const askSession = useRef<string | null>(null);
  // RỔ-4 P2: probe AI_ENABLED khi mở palette, TRƯỚC khi mời người dùng gõ. Endpoint
  // GET /ai/health có từ chiến dịch EASE nhưng KHÔNG AI GỌI — nên ⌘K vẫn mời gõ, người
  // dùng soạn xong cả câu rồi mới ăn 503. null = chưa biết (render như đang bật).
  const [aiOn, setAiOn] = useState<boolean | null>(null);
  // Bumped on every send AND on every reset; a stale in-flight ask (palette closed+reopened while
  // its request was pending) sees a changed id after await and drops its result — no orphan bubble
  // grafted onto a fresh thread (reviewer LOW-1). Session resume bumps it too (thread swap).
  const askReqId = useRef(0);
  const askEndRef = useRef<HTMLDivElement | null>(null);
  // ⌘K-can-act: per-card apply state + the "Phiên trước" resume rail.
  const qc = useQueryClient(); // record-list cache invalidation after a confirmed save
  const [draftStates, setDraftStates] = useState<Record<string, AskDraftState>>({});
  const [sessionsOpen, setSessionsOpen] = useState(false);
  const [sessions, setSessions] = useState<AssistantSessionSummary[] | null>(null);
  const [sessionsLoading, setSessionsLoading] = useState(false);
  const [sessionsErr, setSessionsErr] = useState<string | null>(null);
  const [sessionLoadingId, setSessionLoadingId] = useState<string | null>(null);

  const accessible = useMemo(
    () => entities.filter((e) => can("view", e.name)),
    [entities, can],
  );
  // Entity the user is standing on (first path segment when it names an accessible entity).
  const currentEntity = useMemo(() => {
    const seg = pathname.split("/").filter(Boolean)[0];
    return accessible.some((e) => e.name === seg) ? seg : undefined;
  }, [pathname, accessible]);

  // W3: pinned/current entities first for both "Đối tượng" and "Tạo … mới" commands.
  const orderedEntities = useMemo(() => {
    const seen = new Set<string>();
    const out: EntityMeta[] = [];
    const push = (name?: string) => {
      if (!name || seen.has(name)) return;
      const e = accessible.find((x) => x.name === name);
      if (e) { seen.add(name); out.push(e); }
    };
    push(currentEntity);
    for (const p of pins) push(p);
    for (const e of accessible) push(e.name);
    return out;
  }, [accessible, pins, currentEntity]);

  const entityCmds: NavCmd[] = useMemo(
    () =>
      orderedEntities.map((e) => ({
        id: `ent-${e.name}`,
        label: e.label,
        group: "Đối tượng",
        href: `/${e.name}`,
        icon: Boxes,
        hint: `Danh sách ${e.label}`,
      })),
    [orderedEntities],
  );
  const createCmds: NavCmd[] = useMemo(
    () =>
      orderedEntities.map((e) => ({
        id: `ent-new-${e.name}`,
        label: `Tạo ${e.label} mới`,
        group: "Hành động",
        href: `/${e.name}/new`,
        icon: Plus,
      })),
    [orderedEntities],
  );

  const staticCmds = useMemo(
    () => buildStaticCmds(isAdmin, pathname.startsWith("/studio"), projectSlugAt(pathname)),
    [isAdmin, pathname],
  );

  const needle = stripVN(q.trim());
  const matches = (c: NavCmd) =>
    !needle ||
    stripVN(c.label).includes(needle) ||
    stripVN(c.group).includes(needle) ||
    (c.hint ? stripVN(c.hint).includes(needle) : false);

  const navFiltered = staticCmds.filter(matches);
  // Khi chưa gõ gì: cap danh sách đối tượng/tạo-mới cho gọn (pinned/current đã lên đầu).
  const entFiltered = (needle ? entityCmds : entityCmds.slice(0, 6)).filter(matches);
  const createFiltered = (needle ? createCmds : createCmds.slice(0, 3)).filter(matches);
  const recentsShown = needle ? [] : recents; // recents chỉ hiện ở trạng thái chưa gõ

  // Action commands (theme / studio bridge / logout) — matched bỏ-dấu như mọi lệnh khác.
  const showBridge = isOperator && (!needle || stripVN("Chuyển sang Studio builder").includes(needle));
  const showLogout = !needle || stripVN("Đăng xuất logout thoát").includes(needle);

  const showRecordSearch = needle.length >= SEARCH_MIN_CHARS;
  const nothingMatched =
    navFiltered.length === 0 && entFiltered.length === 0 && createFiltered.length === 0 &&
    !showBridge && !showLogout &&
    (!showRecordSearch || (!searching && (recordGroups?.length ?? 0) === 0));

  // Reset + snapshot theme/recents khi mở
  useEffect(() => {
    if (!open) return;
    setQ("");
    setRecordGroups(null);
    setSearching(false);
    setRecents(loadRecentRoutes());
    // Fresh ask-panel per palette open — a new panel starts a new assistant thread.
    setAskOpen(false);
    setAskTurns([]);
    setAskLoading(false);
    setAskNotice(null);
    setAskInput("");
    setDraftStates({});
    setSessionsOpen(false);
    setSessions(null);
    setSessionsLoading(false);
    setSessionsErr(null);
    setSessionLoadingId(null);
    askSession.current = null;
    askReqId.current++; // invalidate any in-flight ask so its late resolve is dropped
  }, [open]);

  // Keep the newest ask turn / spinner in view.
  useEffect(() => {
    if (askOpen) askEndRef.current?.scrollIntoView({ block: "end" });
  }, [askOpen, askTurns, askLoading, askNotice]);

  // Escape đóng (cmdk không tự đóng vì ta không dùng Command.Dialog)
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") onClose(); };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  // Probe AI on/off ONCE per palette open, before the user is invited to type. Fire-and-forget:
  // aiHealth() never throws (unknown → treated as on), so a blip cannot hide a working assistant.
  useEffect(() => {
    if (!open) return;
    let alive = true;
    void aiHealth().then((on) => { if (alive) setAiOn(on); });
    return () => { alive = false; };
  }, [open]);

  // SEARCH-UNIFIED (Wave 3.5): one server call GET /search?q= fans out over EVERY viewable
  // entity (ACL + row scope enforced server-side) — no more client-side top-5 cap that hid
  // records in the 6th+ entity, and no churny accessible/pins deps (the server owns the entity
  // set now). Debounce + a seq guard still drop stale in-flight responses.
  useEffect(() => {
    if (!open) return;
    const trimmed = q.trim();
    if (trimmed.length < SEARCH_MIN_CHARS) {
      seqRef.current++;
      setRecordGroups(null);
      setSearching(false);
      return;
    }
    const seq = ++seqRef.current;
    setSearching(true);
    setSearchFailed(false);
    const t = setTimeout(async () => {
      try {
        const groups = await searchAll(trimmed);
        if (seqRef.current !== seq) return; // stale — user typed on
        setRecordGroups(shapeServerSearch(groups));
      } catch {
        // A FAILED search is not an empty search. Flattening it to [] rendered the confident
        // sentence "không tìm thấy bản ghi khớp X" for a request that never completed — the user
        // concludes the record does not exist and stops looking.
        if (seqRef.current === seq) { setRecordGroups([]); setSearchFailed(true); }
      } finally {
        if (seqRef.current === seq) setSearching(false);
      }
    }, SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [q, open]);

  if (!open) return null;

  const go = (href: string) => {
    router.push(href);
    onClose();
  };

  const fire = (name: string) => {
    window.dispatchEvent(new Event(name));
    onClose();
  };

  async function handleLogout() {
    onClose();
    try {
      await logout();
    } finally {
      window.location.href = "/login";
    }
  }

  // One ask-turn against POST /assistant/ask. NEVER console.log the question/reply/error —
  // this is the user's business data. A failed turn maps to an honest notice (lib/assistant);
  // resending after a 429 is always safe (the refused turn is not persisted server-side).
  async function sendAsk(text: string) {
    const question = text.trim();
    if (!question || askLoading) return;
    const reqId = ++askReqId.current;
    setSessionsOpen(false); // a send always lands in the thread view
    setAskTurns((ts) => [...ts, { id: newTurnId(), role: "user", content: question }]);
    setAskLoading(true);
    setAskNotice(null);
    try {
      const res = await assistantAsk(question, askSession.current ?? undefined);
      if (reqId !== askReqId.current) return; // stale (palette reset/reopened) — drop
      askSession.current = res.sessionId; // keep the thread for follow-ups in this panel
      // A max_steps turn (or an empty reply) shows an honest "narrow it down" bubble instead of a blank one
      // (reviewer MED-1); a refused turn says the model declined. The same text a reopened thread shows.
      const content = askReplyText(res.reply, res.outcome);
      // drafts ride along even on a max_steps turn — they were validated before the loop stopped.
      setAskTurns((ts) => [...ts, { id: newTurnId(), role: "assistant", content, drafts: res.drafts ?? null }]);
    } catch (err) {
      if (reqId !== askReqId.current) return; // stale — don't surface on a fresh panel
      setAskNotice(assistantNotice(err));
    } finally {
      if (reqId === askReqId.current) setAskLoading(false);
    }
  }

  // Confirm one draft card: the NORMAL record mutation under the user's own token (no AI apply
  // endpoint by design) — validation/ACL failures are the same honest ApiErrors the manual form
  // gets, surfaced on the card with a retry.
  async function applyDraft(key: string, d: AssistantDraft) {
    const create = getRecordCreateDraft(d);
    const update = create ? null : getRecordUpdateDraft(d);
    if (!create && !update) return; // malformed card renders without buttons anyway
    const reqId = askReqId.current; // thread swap/reset while saving → drop the state write
    setDraftStates((s) => ({ ...s, [key]: { status: "saving" } }));
    try {
      let entity: string;
      let savedId: string;
      if (create) {
        entity = create.entity;
        savedId = (await createRecord(create.entity, create.data)).id;
      } else {
        entity = update!.entity;
        savedId = update!.recordId;
        await updateRecord(update!.entity, update!.recordId, update!.data);
        qc.invalidateQueries({ queryKey: qk.record(entity, savedId) });
      }
      // Prefix ["records", entity] catches every query-string variant (qk.records pins a 3rd element).
      qc.invalidateQueries({ queryKey: ["records", entity] });
      if (reqId !== askReqId.current) return;
      setDraftStates((s) => ({ ...s, [key]: { status: "done", savedId } }));
    } catch (err) {
      if (reqId !== askReqId.current) return;
      // One sentence (a veto gives the rule's own line); never a field code.
      setDraftStates((s) => ({ ...s, [key]: { status: "error", message: formatSaveError(err) } }));
    }
  }

  // "Phiên trước": load the user-scoped rail (max 10 shown — it's a palette, not an archive).
  async function openSessions() {
    setSessionsOpen(true);
    setSessionsLoading(true);
    setSessionsErr(null);
    const reqId = askReqId.current;
    try {
      const rows = await listAssistantSessions();
      if (reqId !== askReqId.current) return;
      setSessions(rows.slice(0, 10));
    } catch (err) {
      if (reqId !== askReqId.current) return;
      setSessionsErr(errorMessage(err));
    } finally {
      if (reqId === askReqId.current) setSessionsLoading(false);
    }
  }

  // Resume a thread: swap the panel onto its persisted turns (drafts re-render actionable) and
  // keep its sessionId for follow-ups. Bumps askReqId so an in-flight ask/apply of the old thread
  // can't graft onto the resumed one.
  async function resumeSession(id: string) {
    if (sessionLoadingId) return;
    const reqId = ++askReqId.current;
    setSessionLoadingId(id);
    setSessionsErr(null);
    try {
      const detail = await getAssistantSession(id);
      if (reqId !== askReqId.current) return;
      askSession.current = detail.id;
      setAskTurns(assistantMessagesToTurns(detail.messages));
      setDraftStates({}); // resumed drafts start un-applied (the record store is the truth)
      setAskNotice(null);
      setAskLoading(false);
      setSessionsOpen(false);
    } catch (err) {
      if (reqId !== askReqId.current) return;
      setSessionsErr(errorMessage(err));
    } finally {
      if (reqId === askReqId.current) setSessionLoadingId(null);
    }
  }

  // Delete a thread — optimistic row removal, rollback on failure. Deleting the ACTIVE thread
  // resets the panel to a fresh one only after the server confirmed (no phantom reset).
  async function removeSession(id: string) {
    const snapshot = sessions;
    setSessions((rows) => (rows ?? []).filter((r) => r.id !== id));
    try {
      await deleteAssistantSession(id);
      if (askSession.current === id) {
        askSession.current = null;
        askReqId.current++;
        setAskTurns([]);
        setDraftStates({});
        setAskNotice(null);
        setAskLoading(false);
      }
    } catch (err) {
      setSessions(snapshot); // the row is still alive server-side — put it back
      setSessionsErr(errorMessage(err));
    }
  }

  // Selected the "Hỏi AI" command → switch the palette into the answer panel with the typed query.
  function openAsk() {
    const question = q.trim();
    if (!question) return;
    setAskOpen(true);
    void sendAsk(question);
  }

  function submitFollowUp(e: FormEvent) {
    e.preventDefault();
    const question = askInput.trim();
    if (!question || askLoading) return;
    setAskInput("");
    void sendAsk(question);
  }

  const itemClass =
    "group flex cursor-default select-none items-center gap-2.5 rounded-input px-3 py-2 text-body text-fg transition-colors " +
    // W0-QW11: text-ac-hi trên nền acd (contrast dark mode)
    "data-[selected=true]:bg-acd data-[selected=true]:text-ac-hi";
  const groupHeadingClass = "px-3 pb-1 pt-2 text-10-5px font-semibold tracking-wider text-fnt";

  const renderNavItem = (c: NavCmd) => (
    <Command.Item key={c.id} value={c.id} onSelect={() => (c.event ? fire(c.event) : go(c.href))} className={itemClass}>
      <c.icon className="size-4 shrink-0 text-mut" strokeWidth={1.8} />
      <span className="flex-1 truncate">{c.label}</span>
      {c.hint && <span className="shrink-0 text-caption text-fnt">{c.hint}</span>}
      <ArrowRight className="hidden size-3.5 shrink-0 text-mut group-data-[selected=true]:block" strokeWidth={2} />
    </Command.Item>
  );

  return (
    <div className="fixed inset-0 z-[60] flex items-start justify-center pt-[10vh]" onClick={onClose}>
      {/* Backdrop */}
      <div className="absolute inset-0 bg-fg/10" style={{ backdropFilter: "blur(3px)" }} />

      <div
        onClick={(e) => e.stopPropagation()}
        className="relative w-full max-w-[580px] overflow-hidden rounded-card border border-bd shadow-dialog"
        style={{ background: "oklch(var(--panel) / 0.95)", backdropFilter: "blur(20px)" }}
      >
        {askOpen ? (
          /* ── "Hỏi AI" answer panel — replaces the command list while open. esc still closes. ── */
          <div className="flex max-h-[60vh] min-h-[260px] flex-col">
            <div className="flex items-center gap-2.5 border-b border-bd px-4 py-3">
              <Sparkles className="size-4 shrink-0 text-ac" strokeWidth={1.8} />
              <span className="flex-1 truncate text-13-5px font-medium text-fg">Hỏi AI về dữ liệu của bạn</span>
              <button
                type="button"
                onClick={() => (sessionsOpen ? setSessionsOpen(false) : void openSessions())}
                className={clsx(
                  "flex items-center gap-1 rounded-input border border-bd bg-panel px-2 py-1 text-caption transition-colors hover:text-fg",
                  sessionsOpen ? "text-fg" : "text-fnt",
                )}
              >
                <History className="size-3" strokeWidth={1.8} /> Phiên trước
              </button>
              <button
                type="button"
                onClick={() => setAskOpen(false)}
                className="rounded-input border border-bd bg-panel px-2 py-1 text-caption text-fnt transition-colors hover:text-fg"
              >
                ← Tìm kiếm
              </button>
            </div>

            {sessionsOpen ? (
              /* "Phiên trước" — resume rail. Click a row → thread swaps in (drafts actionable). */
              <div className="flex-1 overflow-y-auto p-2">
                {sessionsErr && (
                  <p className="px-2 py-1.5 text-sub text-dgf">{sessionsErr}</p>
                )}
                {sessionsLoading ? (
                  <div className="flex items-center gap-2.5 px-2 py-2 text-12-5px text-fnt">
                    <Loader2 className="size-4 animate-spin" strokeWidth={2} /> Đang tải phiên…
                  </div>
                ) : (sessions ?? []).length === 0 ? (
                  !sessionsErr && <p className="px-2 py-2 text-12-5px text-fnt">Chưa có phiên hỏi-đáp nào.</p>
                ) : (
                  (sessions ?? []).map((s) => (
                    <div key={s.id} className="group flex items-center gap-1.5 rounded-input px-2 py-1 hover:bg-panel2">
                      <button
                        type="button"
                        onClick={() => void resumeSession(s.id)}
                        disabled={!!sessionLoadingId}
                        className="flex min-w-0 flex-1 items-center gap-2 py-1 text-left disabled:opacity-60"
                      >
                        {sessionLoadingId === s.id ? (
                          <Loader2 className="size-3.5 shrink-0 animate-spin text-fnt" strokeWidth={2} />
                        ) : (
                          <History className="size-3.5 shrink-0 text-mut" strokeWidth={1.8} />
                        )}
                        <span className="min-w-0 flex-1 truncate text-body text-fg">{s.title || "Phiên không tên"}</span>
                        <span className="shrink-0 text-caption text-fnt">{relTime(s.updatedAt)}</span>
                      </button>
                      <button
                        type="button"
                        aria-label={`Xoá phiên ${s.title || s.id}`}
                        onClick={() => void removeSession(s.id)}
                        className="shrink-0 rounded-input p-1 text-fnt opacity-0 transition-opacity hover:text-dgf focus-visible:opacity-100 group-hover:opacity-100"
                      >
                        <Trash2 className="size-3.5" strokeWidth={1.8} />
                      </button>
                    </div>
                  ))
                )}
              </div>
            ) : (
            <div className="flex-1 space-y-3 overflow-y-auto px-4 py-3">
              {askTurns.map((t) =>
                t.role === "user" ? (
                  <div key={t.id} className="flex justify-end">
                    <div className="max-w-[86%] whitespace-pre-wrap rounded-card rounded-tr-[4px] bg-acd px-3 py-2 text-body leading-relaxed text-fg">
                      {t.content}
                    </div>
                  </div>
                ) : (
                  <div key={t.id} className="flex w-full gap-2.5">
                    <span className="mt-0.5 grid size-6 shrink-0 place-items-center rounded-full bg-acd text-ac">
                      <Sparkles className="size-3" strokeWidth={1.8} />
                    </span>
                    <div className="min-w-0 flex-1 space-y-2">
                      {/* Never empty: askReplyText says why a turn has no answer, live or reopened. */}
                      <div className="rounded-card rounded-tl-[4px] border border-bd bg-panel2 px-3 py-2">
                        <MarkdownLite text={t.content} />
                      </div>
                      {(t.drafts ?? []).map((d, i) => (
                        <AskDraftCard
                          key={`${t.id}:${i}`}
                          draft={d}
                          state={draftStates[`${t.id}:${i}`]}
                          onConfirm={() => void applyDraft(`${t.id}:${i}`, d)}
                          onDismiss={() =>
                            setDraftStates((s) => ({ ...s, [`${t.id}:${i}`]: { status: "dismissed" } }))
                          }
                          onOpen={go}
                        />
                      ))}
                    </div>
                  </div>
                ),
              )}
              {askLoading && (
                <div className="flex items-center gap-2.5 text-12-5px text-fnt">
                  <Loader2 className="size-4 animate-spin" strokeWidth={2} /> Đang hỏi AI…
                </div>
              )}
              {askNotice && (
                // Tone mirrors the assistant panel (M4): "error" = danger red; "limit"/"disabled" = warning —
                // transient throttle or feature-off, never the user's fault.
                <div
                  role="status"
                  className={clsx(
                    "flex items-start gap-2.5 rounded-card border px-3 py-2.5",
                    askNotice.kind === "error" ? "border-dg/30 bg-dgd" : "border-wn/30 bg-wnd",
                  )}
                >
                  {askNotice.kind === "error" ? (
                    <AlertCircle className="mt-0.5 size-4 shrink-0 text-dgf" strokeWidth={2} />
                  ) : (
                    <Clock className="mt-0.5 size-4 shrink-0 text-wnf" strokeWidth={2} />
                  )}
                  <p className={clsx("min-w-0 flex-1 text-12-5px", askNotice.kind === "error" ? "text-dgf" : "text-wnf")}>
                    {askNotice.text}
                  </p>
                </div>
              )}
              <div ref={askEndRef} />
            </div>
            )}

            {/* Follow-up composer — same assistant thread (sessionId kept while the panel lives).
                Disabled only for AI_DISABLED; a 429'd or failed turn stays resendable. */}
            <form onSubmit={submitFollowUp} className="flex items-center gap-2 border-t border-bd px-4 py-3">
              <input
                autoFocus
                value={askInput}
                onChange={(e) => setAskInput(e.target.value)}
                placeholder={askNotice?.kind === "disabled" ? "Trợ lý AI đang tắt" : "Hỏi tiếp về dữ liệu…"}
                disabled={askNotice?.kind === "disabled"}
                className="flex-1 bg-transparent text-13-5px text-fg placeholder:text-fnt focus:outline-none disabled:opacity-60"
              />
              <button
                type="submit"
                aria-label="Gửi câu hỏi"
                disabled={askLoading || !askInput.trim() || askNotice?.kind === "disabled"}
                className="grid size-7 shrink-0 place-items-center rounded-input bg-ac text-acf transition-colors hover:bg-ac2 disabled:opacity-40"
              >
                <ArrowUp className="size-3.5" strokeWidth={2.2} />
              </button>
            </form>
          </div>
        ) : (
        <Command shouldFilter={false} label="Lệnh nhanh">
          {/* Search input */}
          <div className="flex items-center gap-2.5 border-b border-bd px-4 py-3">
            <Search className="size-4 shrink-0 text-fnt" strokeWidth={1.8} />
            <Command.Input
              autoFocus
              value={q}
              onValueChange={setQ}
              placeholder="Tìm bản ghi, đối tượng, lệnh…"
              className="flex-1 bg-transparent text-14px text-fg placeholder:text-fnt focus:outline-none"
            />
            <kbd className="rounded-[5px] border border-bd bg-panel px-1.5 py-0.5 font-mono text-10px text-fnt">
              esc
            </kbd>
          </div>

          <Command.List className="max-h-[52vh] overflow-y-auto p-2">
            {/* W3: recents-first — 5 route ghé gần nhất (chỉ khi chưa gõ) */}
            {recentsShown.length > 0 && (
              <div className="mb-1">
                <p className={groupHeadingClass}>Gần đây</p>
                {recentsShown.map((r) => (
                  <Command.Item key={`recent-${r.href}`} value={`recent-${r.href}`} onSelect={() => go(r.href)} className={itemClass}>
                    <History className="size-4 shrink-0 text-mut" strokeWidth={1.8} />
                    <span className="flex-1 truncate">{r.label}</span>
                    <span className="shrink-0 font-mono text-10-5px text-fnt">{r.href}</span>
                  </Command.Item>
                ))}
              </div>
            )}

            {/* W3: cross-entity record search — nhóm nổi lên NGAY khi có query ≥2 ký tự */}
            {showRecordSearch && (
              <div className="mb-1">
                <p className={groupHeadingClass}>Bản ghi</p>
                {searching && (
                  <div className="flex items-center gap-2.5 px-3 py-2 text-12-5px text-fnt">
                    <Loader2 className="size-4 animate-spin" strokeWidth={2} />
                    Đang tìm trong dữ liệu…
                  </div>
                )}
                {!searching && searchFailed && (
                  <p className="px-3 py-2 text-12-5px text-wnf">
                    Không tìm được lúc này — thử lại.
                  </p>
                )}
                {!searching && !searchFailed && recordGroups && recordGroups.length === 0 && (
                  <p className="px-3 py-2 text-12-5px text-fnt">
                    Không tìm thấy bản ghi khớp &ldquo;{q.trim()}&rdquo;.
                  </p>
                )}
                {!searching &&
                  recordGroups?.map((g) => (
                    <div key={g.entity}>
                      {g.hits.map((h) => (
                        <Command.Item key={`rec-${g.entity}-${h.id}`} value={`rec-${g.entity}-${h.id}`} onSelect={() => go(h.href)} className={itemClass}>
                          <FileText className="size-4 shrink-0 text-mut" strokeWidth={1.8} />
                          <span className="flex-1 truncate">{h.label}</span>
                          <span className="shrink-0 text-caption text-fnt">{g.entityLabel}</span>
                        </Command.Item>
                      ))}
                    </div>
                  ))}
              </div>
            )}

            {navFiltered.filter((c) => c.group === "Điều hướng").length > 0 && (
              <div className="mb-1">
                <p className={groupHeadingClass}>Điều hướng</p>
                {navFiltered.filter((c) => c.group === "Điều hướng").map(renderNavItem)}
              </div>
            )}

            {/* Studio tools — only offered while standing in /studio (admin); previously ⌘K
                inside Studio knew ONLY app-face routes and teleported builders out. */}
            {navFiltered.filter((c) => c.group === "Studio").length > 0 && (
              <div className="mb-1">
                <p className={groupHeadingClass}>Studio</p>
                {navFiltered.filter((c) => c.group === "Studio").map(renderNavItem)}
              </div>
            )}

            {entFiltered.length > 0 && (
              <div className="mb-1">
                <p className={groupHeadingClass}>Đối tượng</p>
                {entFiltered.map(renderNavItem)}
              </div>
            )}

            {(createFiltered.length > 0 || showBridge || showLogout) && (
              <div className="mb-1">
                <p className={groupHeadingClass}>Hành động</p>
                {createFiltered.map(renderNavItem)}
                {/* Lệnh cầu nối DUY NHẤT sang Studio (plan §2.1) — admin only */}
                {showBridge && (
                  <Command.Item value="action-studio" onSelect={() => go("/studio")} className={itemClass}>
                    <Wrench className="size-4 shrink-0 text-mut" strokeWidth={1.8} />
                    <span className="flex-1 truncate">Chuyển sang Studio</span>
                    <span className="shrink-0 text-caption text-fnt">Builder · Nexus</span>
                  </Command.Item>
                )}
                {showLogout && (
                  <Command.Item value="action-logout" onSelect={handleLogout} className={clsx(itemClass, "text-dgf data-[selected=true]:bg-dgd data-[selected=true]:text-dgf")}>
                    <LogOut className="size-4 shrink-0" strokeWidth={1.8} />
                    <span className="flex-1 truncate">Đăng xuất</span>
                  </Command.Item>
                )}
              </div>
            )}

            {navFiltered.filter((c) => c.group === "Cài đặt").length > 0 && (
              <div className="mb-1">
                <p className={groupHeadingClass}>Cài đặt</p>
                {navFiltered.filter((c) => c.group === "Cài đặt").map(renderNavItem)}
              </div>
            )}

            {nothingMatched && q && (
              <p className="px-3 py-6 text-center text-12-5px text-fnt">
                Không có kết quả cho <span className="font-medium text-fg">&ldquo;{q}&rdquo;</span>
              </p>
            )}

            {/* ⌘K assistant — hỏi AI về CHÍNH dữ liệu user được xem (mọi user; ACL server-side).
                Nhóm cuối có chủ đích: câu-hỏi-dạng-câu thường không khớp lệnh/bản ghi nào, khi đó
                đây là Command.Item duy nhất → cmdk tự chọn → Enter gửi thẳng. */}
            {needle.length > 0 && (
              <div className="mb-1">
                <p className={groupHeadingClass}>Trợ lý AI</p>
                {aiOn === false ? (
                  /* Honest OFF state, known BEFORE the user commits to a question (RỔ-4 P2). */
                  <div className={clsx(itemClass, "cursor-not-allowed opacity-60")}>
                    <Sparkles className="size-4 shrink-0 text-fnt" strokeWidth={1.8} />
                    <span className="flex-1 truncate">Trợ lý AI đang tắt</span>
                    <span className="shrink-0 text-caption text-fnt">bật ở cấu hình máy chủ</span>
                  </div>
                ) : (
                  <Command.Item value="ask-ai" onSelect={openAsk} className={itemClass}>
                    <Sparkles className="size-4 shrink-0 text-ac" strokeWidth={1.8} />
                    <span className="flex-1 truncate">Hỏi AI: &ldquo;{q.trim()}&rdquo;</span>
                    <span className="shrink-0 text-caption text-fnt">dữ liệu bạn được xem</span>
                  </Command.Item>
                )}
              </div>
            )}
          </Command.List>
        </Command>
        )}

        {/* Footer */}
        <div className="flex items-center gap-3 border-t border-bd px-4 py-2 text-10-5px text-fnt">
          {askOpen ? (
            <span className="flex items-center gap-1">
              <CornerDownLeft className="size-3" strokeWidth={2} /> gửi
            </span>
          ) : (
            <>
              <span className="flex items-center gap-1">
                <kbd className="rounded border border-bd bg-panel px-1 font-mono">↑↓</kbd> chọn
              </span>
              <span className="flex items-center gap-1">
                <CornerDownLeft className="size-3" strokeWidth={2} /> mở
              </span>
            </>
          )}
          <span className="flex items-center gap-1">
            <kbd className="rounded border border-bd bg-panel px-1 font-mono">esc</kbd> đóng
          </span>
          <span className="ml-auto font-mono">Nexus</span>
        </div>
      </div>
    </div>
  );
}

// One HITL confirm card for a record draft the assistant proposed. NOTHING auto-applies — the
// buttons run the normal record mutations under the user's own token (see applyDraft). An update
// card diffs ONLY the proposed fields (before → new); a malformed/unknown draft renders an honest
// dashed fallback with no apply button (mirror of the assistant's unknown-kind card, palette-sized).
function AskDraftCard({ draft, state, onConfirm, onDismiss, onOpen }: {
  draft: AssistantDraft;
  state?: AskDraftState;
  onConfirm: () => void;
  onDismiss: () => void;
  onOpen: (href: string) => void;
}) {
  const create = getRecordCreateDraft(draft);
  const update = create ? null : getRecordUpdateDraft(draft);
  if (!create && !update) {
    return (
      <div className="rounded-card border border-dashed border-bd bg-panel px-3 py-2 text-sub text-fnt">
        Bản nháp không đọc được (thiếu dữ liệu hoặc loại chưa hỗ trợ) — hỏi lại AI.
      </div>
    );
  }
  const entity = (create ?? update!).entity;
  const entries = Object.entries((create ?? update!).data);
  const st = state?.status;
  const dismissed = st === "dismissed";

  return (
    <div className={clsx("overflow-hidden rounded-card border border-bd bg-panel", dismissed && "opacity-60")}>
      <div className="flex items-center gap-2 border-b border-bd px-3 py-2">
        <FileText className="size-3.5 shrink-0 text-mut" strokeWidth={1.8} />
        <span className="min-w-0 flex-1 truncate text-12-5px font-medium text-fg">
          {create ? `Bản nháp: tạo ${entity}` : `Bản nháp: sửa ${entity}`}
        </span>
        {update && <span className="shrink-0 font-mono text-10-5px text-fnt">#{update.recordId.slice(0, 8)}</span>}
      </div>

      <div className="max-h-44 overflow-y-auto px-3 py-2">
        {update && (
          <p className="pb-1 text-caption text-fnt">Chỉ ghi đè {entries.length} trường bên dưới:</p>
        )}
        {entries.length === 0 ? (
          <p className="text-sub text-fnt">Không có trường nào trong bản nháp.</p>
        ) : (
          <table className="w-full border-collapse text-left text-sub">
            <tbody>
              {entries.map(([k, v]) => (
                <tr key={k} className="align-top">
                  <td className="whitespace-nowrap py-0.5 pr-3 font-mono text-caption text-fnt">{k}</td>
                  <td className="break-words py-0.5 text-fg">
                    {update && (
                      <>
                        <span className="text-fnt line-through">{assistantDraftValueText(update.before?.[k])}</span>
                        <span className="px-1.5 text-fnt">→</span>
                      </>
                    )}
                    {assistantDraftValueText(v)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {st === "done" ? (
        <div className="flex items-center gap-2 border-t border-bd bg-okd px-3 py-2">
          <Check className="size-3.5 shrink-0 text-okf" strokeWidth={2.4} />
          <span className="flex-1 text-sub font-medium text-okf">Đã lưu</span>
          <button
            type="button"
            onClick={() => onOpen(`/${entity}/${state?.savedId}`)}
            className="flex items-center gap-1 rounded-input border border-bd bg-panel px-2 py-1 text-11-5px text-fg transition-colors hover:bg-panel2"
          >
            Mở bản ghi <ArrowRight className="size-3" strokeWidth={2} />
          </button>
        </div>
      ) : dismissed ? (
        <div className="border-t border-bd px-3 py-2 text-sub text-fnt">Đã bỏ qua.</div>
      ) : (
        <>
          {st === "error" && state?.message && (
            <div className="flex items-start gap-2 border-t border-dg/25 bg-dgd px-3 py-2">
              <AlertCircle className="mt-0.5 size-3.5 shrink-0 text-dgf" strokeWidth={2} />
              <span className="min-w-0 flex-1 text-sub text-dgf">{state.message}</span>
            </div>
          )}
          <div className="flex items-center justify-end gap-2 border-t border-bd px-3 py-2">
            <button
              type="button"
              onClick={onDismiss}
              disabled={st === "saving"}
              className="rounded-input border border-bd bg-panel px-2.5 py-1 text-sub text-mut transition-colors hover:text-fg disabled:opacity-60"
            >
              Bỏ qua
            </button>
            <button
              type="button"
              onClick={onConfirm}
              disabled={st === "saving"}
              className="flex items-center gap-1.5 rounded-input bg-ac px-3 py-1 text-sub font-medium text-acf transition-colors hover:bg-ac2 disabled:opacity-60"
            >
              {st === "saving"
                ? <Loader2 className="size-3.5 animate-spin" strokeWidth={2} />
                : <Check className="size-3.5" strokeWidth={2.2} />}
              {st === "error" ? "Thử lại" : "Xác nhận & lưu"}
            </button>
          </div>
        </>
      )}
    </div>
  );
}

// Hook: open/close ⌘K / Ctrl+K globally. W3: phím "/" cũng mở palette (search-mode) khi
// KHÔNG đứng trong input/textarea/contenteditable — không cướp phím khi user đang gõ form.
export function useCommandPalette() {
  const [open, setOpen] = useState(false);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((o) => !o);
        return;
      }
      if (e.key === "/" && !e.metaKey && !e.ctrlKey && !e.altKey) {
        const t = e.target as HTMLElement | null;
        if (t && t.closest("input, textarea, select, [contenteditable='true'], [contenteditable='']")) return;
        e.preventDefault();
        setOpen(true);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);
  return { open, setOpen };
}
