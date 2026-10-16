"use client";

// NotificationBell — dropdown chuông + unread count. Bound to real backend:
//   GET /notifications/unread-count (badge count, 30s poll)
//   GET /notifications?limit=20 (dropdown list, on open)
//   POST /notifications/:id/read (mark single read)
//   POST /notifications/read-all (mark all read)
// Fallback to empty state if backend returns 404 (endpoint not deployed yet).

import { useState, useRef, useEffect } from "react";
import Link from "next/link";
import { clsx } from "clsx";
import { Bell, Check, ArrowRight, UserPlus, AtSign, Clock, BadgeCheck, Settings2 } from "lucide-react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import {
  listNotifications,
  getNotificationUnreadCount,
  markNotificationRead,
  markAllNotificationsRead,
  type ApiNotification,
} from "@/lib/api";
import { relTime } from "@/lib/workflowRuns";
import { qk } from "@/lib/query/keys";
import { useAuth } from "@/lib/auth/AuthProvider";

export const TYPE_ICON: Record<string, typeof Bell> = {
  // BE emits "assignment" (SetOwner notice); "assign" kept for any legacy row.
  assignment: UserPlus,
  assign: UserPlus,
  mention: AtSign,
  due: Clock,
  approval: BadgeCheck,
  system: Settings2,
};

export const TYPE_META: Record<string, { label: string; tone: string }> = {
  assignment: { label: "Được giao việc", tone: "ac" },
  assign: { label: "Được giao", tone: "ac" },
  mention: { label: "Nhắc đến", tone: "cy" },
  due: { label: "Sắp đến hạn", tone: "wn" },
  approval: { label: "Cần duyệt", tone: "ok" },
  system: { label: "Hệ thống", tone: "neu" },
};

function toneFillClass(tone: string) {
  if (tone === "ac") return "bg-acd text-ac-hi";
  if (tone === "cy") return "bg-cyd text-cy";
  if (tone === "wn") return "bg-wnd text-wnf";
  if (tone === "ok") return "bg-okd text-ok";
  return "bg-neud text-mut";
}

// Absolute VN-formatted timestamp for the hover title (relTime shows the friendly
// "x phút trước"; this gives the exact moment on hover). Empty on unparseable input.
function absTime(iso: string): string {
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return "";
  return new Date(t).toLocaleString("vi-VN", {
    day: "2-digit", month: "2-digit", year: "numeric",
    hour: "2-digit", minute: "2-digit",
  });
}

/**
 * Unread count (30s poll) + the latest 20 notifications (fetched only while `open`) + the two
 * mark-read mutations. Shared by this bell (Studio Topbar) and the SLDS global header, so both
 * read one cache under the same query keys and never poll twice.
 */
export function useNotificationTray(open: boolean) {
  const { user } = useAuth();
  const qc = useQueryClient();
  const enabled = !!user;

  // Unread count — polls every 30s
  const { data: unreadCount = 0 } = useQuery({
    queryKey: qk.notificationCount(),
    queryFn: getNotificationUnreadCount,
    staleTime: 30 * 1000,
    refetchInterval: 30 * 1000,
    enabled,
  });

  // Full list — fetched only when dropdown opens
  const { data: notiData } = useQuery({
    queryKey: qk.notifications(20),
    queryFn: () => listNotifications(20),
    staleTime: 30 * 1000,
    enabled: enabled && open,
  });

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: qk.notificationCount() });
    qc.invalidateQueries({ queryKey: qk.notifications() });
  };
  const markRead = useMutation({ mutationFn: markNotificationRead, onSuccess: invalidate });
  const markAll = useMutation({ mutationFn: markAllNotificationsRead, onSuccess: invalidate });

  return { unreadCount, notifications: notiData?.notifications ?? [], markRead, markAll };
}

export function NotificationBell() {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const { unreadCount, notifications, markRead, markAll } = useNotificationTray(open);

  // Close on Escape
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open]);

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen((o) => !o)}
        aria-label="Thông báo"
        aria-haspopup="true"
        aria-expanded={open}
        className={clsx(
          "relative grid size-9 place-items-center rounded-input transition-colors",
          open ? "bg-panel2 text-fg" : "text-mut hover:bg-panel2",
        )}
      >
        <Bell className="size-[18px]" strokeWidth={1.8} />
        {unreadCount > 0 && (
          <span
            aria-label={`${unreadCount} thông báo chưa đọc`}
            className="absolute -right-0.5 -top-0.5 grid h-4 min-w-4 place-items-center rounded-full bg-dg px-1 font-mono text-9-5px font-semibold text-white ring-2 ring-panel"
          >
            {unreadCount > 9 ? "9+" : unreadCount}
          </span>
        )}
      </button>

      {open && (
        <>
          <div className="fixed inset-0 z-30" onClick={() => setOpen(false)} />
          <div className="absolute right-0 top-11 z-40 w-[392px] overflow-hidden rounded-card border border-bd bg-panel shadow-pop">
            {/* Header */}
            <div className="flex items-center gap-2 border-b border-bd px-4 py-3">
              <h3 className="font-display text-14px font-semibold text-fg">Thông báo</h3>
              {unreadCount > 0 && (
                <span className="rounded-pill bg-dgd px-2 py-0.5 font-mono text-10-5px font-semibold text-dgf">
                  {unreadCount} mới
                </span>
              )}
              <button
                onClick={() => markAll.mutate()}
                disabled={unreadCount === 0 || markAll.isPending}
                className={clsx(
                  "ml-auto flex items-center gap-1 text-sub font-medium transition-colors",
                  unreadCount > 0 ? "text-ac-hi hover:text-ac2" : "cursor-not-allowed text-fnt",
                )}
              >
                <Check className="size-3.5" strokeWidth={2} /> Đánh dấu đã đọc
              </button>
            </div>

            {/* List */}
            <div className="max-h-[420px] overflow-y-auto">
              {notifications.length === 0 ? (
                <div className="flex flex-col items-center gap-2 py-10 text-fnt">
                  <Bell className="size-8 opacity-30" strokeWidth={1.5} />
                  <p className="text-12-5px">Không có thông báo mới</p>
                </div>
              ) : (
                notifications.slice(0, 8).map((n) => (
                  <NotiItem
                    key={n.id}
                    noti={n}
                    compact
                    onRead={() => {
                      if (!n.isRead) markRead.mutate(n.id);
                      // SPA nav keeps the Topbar mounted — close the dropdown ourselves
                      // (a full reload used to do this implicitly).
                      setOpen(false);
                    }}
                  />
                ))
              )}
            </div>

            {/* Footer — SPA link (F7): no full reload on the way to the notification center */}
            <Link
              href="/notifications"
              onClick={() => setOpen(false)}
              className="flex items-center justify-center gap-1.5 border-t border-bd bg-panel2/50 py-2.5 text-12-5px font-medium text-ac-hi hover:bg-acd/40"
            >
              Xem tất cả thông báo <ArrowRight className="size-3.5" strokeWidth={2} />
            </Link>
          </div>
        </>
      )}
    </div>
  );
}

export function NotiItem({
  noti,
  compact,
  onRead,
}: {
  noti: ApiNotification;
  compact?: boolean;
  onRead?: () => void;
}) {
  const type = noti.type ?? "system";
  const meta = TYPE_META[type] ?? TYPE_META.system;
  const Icon = TYPE_ICON[type] ?? Settings2;
  const toneFill = toneFillClass(meta.tone);

  return (
    // next/link (F7 SPA-ify): the deep link into the record/approval is a client-side transition,
    // not a full reload. hrefs come from the backend as in-app paths ("/tickets/<id>", …).
    <Link
      href={noti.href ?? "/notifications"}
      onClick={onRead}
      className={clsx(
        "group relative flex items-start gap-3 border-b border-bd2 px-4 transition-colors last:border-0 hover:bg-panel2",
        compact ? "py-3" : "py-3.5",
        !noti.isRead && "bg-acd/30",
      )}
    >
      {!noti.isRead && (
        <span className="absolute left-1.5 top-1/2 size-1.5 -translate-y-1/2 rounded-full bg-ac" />
      )}
      <span className={clsx(
        "grid shrink-0 place-items-center rounded-[9px]",
        compact ? "size-9" : "size-10",
        toneFill,
      )}>
        {/* Was a two-branch avatar keyed on `actorInitials`, a field no producer has ever written —
            so the initials branch was unreachable and the type icon was the only outcome. Kept the
            outcome, dropped the pretence. */}
        <Icon className={compact ? "size-4" : "size-[18px]"} strokeWidth={1.9} />
      </span>
      <div className="min-w-0 flex-1">
        {/* title is OPTIONAL server-side and several producers never set it, so it cannot be the
            only thing rendered — that is what showed an empty line. message is always written.
            Show the headline when there is one, and always show the sentence. */}
        {noti.title && (
          <p className={clsx("font-medium text-fg", compact ? "text-12-5px leading-snug" : "text-13-5px leading-snug")}>
            {noti.title}
          </p>
        )}
        <p className={clsx("text-fg", compact ? "text-12-5px leading-snug" : "text-13-5px leading-snug", noti.title && "text-mut")}>
          {noti.message}
        </p>
        <div className="mt-1 flex items-center gap-2">
          <span className={clsx("inline-flex items-center gap-1 rounded-badge px-1.5 py-px text-10px font-medium", toneFill)}>
            <Icon className="size-3" strokeWidth={2} /> {meta.label}
          </span>
          <span className="text-caption text-fnt" title={absTime(noti.createdAt)}>{relTime(noti.createdAt)}</span>
        </div>
      </div>
    </Link>
  );
}
