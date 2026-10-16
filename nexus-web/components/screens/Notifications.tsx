"use client";

// Notifications full-page screen (G — Screen #32-33 from SCREEN-INDEX)
// LIVE endpoints (verified 2026-06-30):
//   GET /notifications?page=&limit= → { data: ApiNotification[], pagination: {...} }
//   POST /notifications/read-all → 200
//   POST /notifications/:id/read → 200
// Data lives in nexus_notifications table (server writes; this screen reads + marks read).
//
// SLDS (specs/038 T032, RESEARCH §5 /notifications): the ObjectHomePage template —
// page-headers/object-home inside a card, "Tất cả / Chưa đọc" as the list-view switcher next to
// the title — and the list as `ul.slds-has-dividers_bottom-space` rows of slds-media + avatar
// (blueprint `avatar`, Lucide glyph on the object colour). Unread rows take the SLDS
// notification-list tint (`slds-global-header__notification_unread`) AND say "Chưa đọc" in text,
// so the state is never colour-only.
import { useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import Link from "next/link";
import clsx from "clsx";
import { AtSign, Bell, BadgeCheck, BellOff, ChevronDown, Clock, RefreshCw, Settings2, UserPlus } from "lucide-react";
import {
  listNotificationsPage, markNotificationRead, markAllNotificationsRead,
  ensureAuth, ApiError, type ApiNotification,
} from "@/lib/admin";
import { relTime } from "@/lib/workflowRuns";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { toast } from "@/components/ui/Toaster";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { Menu, MenuCheckboxItem } from "@/components/slds/Menu";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Avatar } from "@/components/slds/Avatar";
import { Badge } from "@/components/slds/Badge";
import { EmptyState } from "@/components/slds/EmptyState";
import { Spinner } from "@/components/slds/Spinner";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

type Filter = "all" | "unread";

// ponytail: mirrors TYPE_ICON/TYPE_META in components/ui/NotificationBell.tsx (not exported, and
// the bell belongs to the shell migration). Fold both into one shared map once the bell is on SLDS.
const TYPES: Record<string, { glyph: typeof Bell; color: string; label: I18nKey }> = {
  // BE emits "assignment" (SetOwner notice); "assign" kept for any legacy row.
  assignment: { glyph: UserPlus, color: "user", label: "page.notifications.type.assignment" },
  assign: { glyph: UserPlus, color: "user", label: "page.notifications.type.assign" },
  mention: { glyph: AtSign, color: "feed", label: "page.notifications.type.mention" },
  due: { glyph: Clock, color: "task", label: "page.notifications.type.due" },
  approval: { glyph: BadgeCheck, color: "approval", label: "page.notifications.type.approval" },
  system: { glyph: Settings2, color: "custom-notification", label: "page.notifications.type.system" },
};

// Exact VN timestamp for the hover title; the row shows the friendly relative time.
function absTime(iso: string): string {
  const ms = new Date(iso).getTime();
  if (!Number.isFinite(ms)) return "";
  return new Date(ms).toLocaleString("vi-VN", {
    day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit",
  });
}

function NotificationRow({ noti, onRead }: { noti: ApiNotification; onRead: () => void }) {
  const type = TYPES[noti.type ?? "system"] ?? TYPES.system;
  return (
    <li className={clsx("slds-item", !noti.isRead && "slds-global-header__notification_unread")}>
      {/* next/link: the deep link (backend in-app path) is a client-side transition. */}
      <Link href={noti.href ?? "/notifications"} onClick={onRead} className="slds-media slds-text-link_reset">
        <div className="slds-media__figure">
          <Avatar name="" icon={type.glyph} colorName={type.color} shape="circle" />
        </div>
        <div className="slds-media__body">
          {/* title is optional server-side (several producers never set it); message is always
              written — show the headline when there is one, and always the sentence. */}
          {noti.title && <h3 className="slds-text-heading_small">{noti.title}</h3>}
          <p className="slds-text-body_regular">{noti.message}</p>
          <ul className="slds-list_horizontal slds-has-dividers_right slds-text-body_small slds-text-color_weak mt-1">
            <li className="slds-item">
              <Badge variant="lightest">{t(type.label)}</Badge>
            </li>
            <li className="slds-item" title={absTime(noti.createdAt)}>
              {relTime(noti.createdAt)}
            </li>
            {!noti.isRead && (
              <li className="slds-item">
                <strong>{t("page.notifications.unread")}</strong>
              </li>
            )}
          </ul>
        </div>
      </Link>
    </li>
  );
}

export function Notifications() {
  const [filter, setFilter] = useState<Filter>("all");
  const [markingAll, setMarkingAll] = useState(false);
  const qc = useQueryClient();

  // The list is a react-query read (was useEffect + setState, which fetched the same way but
  // tripped react-hooks/set-state-in-effect). Same request: page 1 × 50, unread-only per view.
  const queryKey = ["notifications-page", filter] as const;
  const { data: items = [], isFetching: loading, error, refetch } = useQuery({
    queryKey,
    queryFn: async () => {
      await ensureAuth();
      return (await listNotificationsPage(1, 50, filter === "unread")).items;
    },
    retry: false,
    // Old screen fetched on every mount / view switch; keep that (app default staleTime is 2 min,
    // which would show a cached view without the marks made in the other one).
    staleTime: 0,
    refetchOnReconnect: false,
  });
  const err = error ? (error instanceof ApiError ? error.message : t("page.notifications.loadError")) : null;
  const patchItems = (fn: (n: ApiNotification) => ApiNotification) =>
    qc.setQueryData<ApiNotification[]>(queryKey, (xs) => xs?.map(fn));

  async function handleMarkOne(id: string) {
    try {
      await markNotificationRead(id);
      patchItems((x) => x.id === id ? { ...x, isRead: true } : x);
    } catch {
      // Silently fail — not critical
    }
  }

  async function handleMarkAll() {
    setMarkingAll(true);
    try {
      await markAllNotificationsRead();
      patchItems((x) => ({ ...x, isRead: true }));
      toast.success(t("page.notifications.markedAll"));
    } catch (ex) {
      toast.error(ex instanceof ApiError ? ex.message : t("page.notifications.markAllError"));
    } finally {
      setMarkingAll(false);
    }
  }

  const unreadCount = items.filter((n) => !n.isRead).length;
  const unreadView = t("page.notifications.view.unread");
  const switchLabel = t("page.notifications.switchView");

  let empty: ReactNode = null;
  if (loading) {
    empty = (
      <div className="slds-is-relative h-32">
        <Spinner label={t("page.notifications.loadingList")} />
      </div>
    );
  } else if (err) {
    empty = (
      <div className="p-4">
        <ScopedNotification theme="error">{err}</ScopedNotification>
      </div>
    );
  } else if (items.length === 0) {
    empty = (
      <EmptyState
        className="p-6"
        icon={BellOff}
        title={t("page.notifications.empty")}
        description={t(filter === "unread" ? "page.notifications.emptyUnread" : "page.notifications.emptyAll")}
      />
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col p-4">
      <ObjectHomePage
        icon={{ glyph: Bell, name: "custom-notification" }}
        label={t("page.notifications.label")}
        title={filter === "unread" ? unreadView : t("page.notifications.view.all")}
        titleMenu={
          <Menu
            trigger={
              <button type="button" className="slds-button slds-button_icon slds-button_icon-small" title={switchLabel}>
                <ChevronDown aria-hidden="true" className="slds-button__icon slds-icon_x-small nx-lucide" />
                <span className="slds-assistive-text">{switchLabel}</span>
              </button>
            }
          >
            <MenuCheckboxItem
              label={t("page.notifications.view.all")}
              checked={filter === "all"}
              onCheckedChange={() => setFilter("all")}
            />
            <MenuCheckboxItem
              label={unreadCount > 0 ? `${unreadView} (${unreadCount})` : unreadView}
              checked={filter === "unread"}
              onCheckedChange={() => setFilter("unread")}
            />
          </Menu>
        }
        actions={[
          {
            // Constant label: ActionGroup keys buttons by label, so swapping it mid-action would remount
            // the button and drop keyboard focus. `disabled` + the toast carry the feedback.
            label: t("page.notifications.markAll"),
            onSelect: handleMarkAll,
            disabled: unreadCount === 0 || markingAll,
          },
        ]}
        meta={
          loading
            ? t("page.notifications.loading")
            : unreadCount > 0
              ? t("page.notifications.unreadCount").replace("{n}", String(unreadCount))
              : t("page.notifications.allRead")
        }
        controls={[
          <ButtonIcon
            key="refresh"
            icon={RefreshCw}
            label={t("page.notifications.refresh")}
            variant="border-filled"
            onClick={() => refetch()}
          />,
        ]}
        empty={empty}
      >
        <ul className="slds-has-dividers_bottom-space slds-border_top">
          {items.map((n) => (
            <NotificationRow key={n.id} noti={n} onRead={() => handleMarkOne(n.id)} />
          ))}
        </ul>
      </ObjectHomePage>
    </div>
  );
}
