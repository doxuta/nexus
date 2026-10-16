"use client";

// ConversationMenu — "Cuộc trò chuyện" menu at the head of the assistant panel (specs/076 ai4, T084).
// Presentational: the panel owns the list/open/delete calls and passes state down. Each row is plain text
// (title + relative time), never a chip or avatar. Delete asks IN the page with InlineConfirm, never a modal.
import { MessagesSquare, SquarePen, Trash2 } from "lucide-react";
import { useState } from "react";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Menu, MenuDivider, MenuItem } from "@/components/slds/Menu";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { t, tf } from "@/lib/i18n";
import { relTime } from "@/lib/workflowRuns";
import { InlineConfirm } from "../InlineConfirm";

export interface ConversationItem {
  id: string;
  title: string;
  updatedAt: string;
}

interface Props {
  sessions: ConversationItem[];
  loading: boolean;
  /** The list failed to load: shows the message and "Thử lại". */
  error?: boolean;
  activeId: string | null;
  deleting?: boolean;
  onRetry: () => void;
  onNew: () => void;
  onOpen: (id: string) => void;
  onDelete: (id: string) => void;
}

export function ConversationMenu({ sessions, loading, error, activeId, deleting, onRetry, onNew, onOpen, onDelete }: Props) {
  const [asking, setAsking] = useState(false);
  const active = sessions.find((s) => s.id === activeId);
  const nameOf = (s: ConversationItem) => s.title || t("studio.assistant.sessions.untitled");
  const showList = !loading && !error;
  return (
    <div>
      <Menu
        size="small"
        length={7}
        trigger={<ButtonIcon icon={MessagesSquare} label={t("studio.assistant.sessions.title")} variant="border" more />}
      >
        <MenuItem label={t("studio.assistant.sessions.new")} icon={SquarePen} onSelect={onNew} />
        <MenuDivider />
        {loading && (
          <li role="presentation" className="slds-p-around_small">
            <Spinner size="x-small" label={t("studio.assistant.sessions.loading")} />
          </li>
        )}
        {showList && sessions.length === 0 && (
          <li role="presentation" className="slds-p-around_small slds-text-color_weak">{t("studio.assistant.sessions.empty")}</li>
        )}
        {showList && sessions.map((s) => (
          <MenuItem key={s.id} label={`${nameOf(s)} · ${relTime(s.updatedAt)}`} current={s.id === activeId} onSelect={() => onOpen(s.id)} />
        ))}
        {active && <MenuDivider />}
        {active && <MenuItem label={t("studio.assistant.sessions.delete")} icon={Trash2}onSelect={() => setAsking(true)} />}
      </Menu>
      {error && (
        <ScopedNotification theme="error" role="alert">
          {t("studio.assistant.sessions.loadError")}{" "}
          <Button variant="base" onClick={onRetry}>{t("studio.assistant.sessions.retry")}</Button>
        </ScopedNotification>
      )}
      {asking && active && (
        <InlineConfirm
          tone="error"
          message={tf("studio.assistant.sessions.deleteConfirm", { title: nameOf(active) })}
          confirmLabel={t("studio.assistant.sessions.delete")}
          busy={deleting}
          onConfirm={() => { onDelete(active.id); setAsking(false); }}
          onCancel={() => setAsking(false)}
        />
      )}
    </div>
  );
}
