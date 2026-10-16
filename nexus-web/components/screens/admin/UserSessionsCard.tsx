"use client";

// "Phiên đăng nhập" card — one row per device a user is logged in on (specs/055). Shared by
// /profile (my own sessions: GET/DELETE /auth/sessions) and the admin's user detail (someone
// else's: GET/DELETE /admin/users/:id/sessions[/:sid], specs/065 lát A, #313). The card owns the
// list, the loading/empty/failed states and the confirm; the caller owns WHICH endpoints:
//   load()      → the rows            end(id) → sign that one device out
//   revokeAll?  → "Đăng xuất mọi thiết bị" (own account only — the button is absent without it)
// Loading, empty and failed loads each say what they are — a blank card would read as "no devices".
// The row of THIS browser (the access token's `sid`, only ever set on /profile) is "Phiên này"
// and has no logout button; every other row can be ended, which the server enforces on that
// device's very next request.
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { LogOut, Monitor, Smartphone } from "lucide-react";
import { ApiError, type LoginSession } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { Card } from "@/components/slds/Card";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { Icon } from "@/components/slds/Icon";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";

export interface UserSessionsCardProps {
  load: () => Promise<LoginSession[]>;
  end: (sessionId: string) => Promise<void>;
  /** Own-account "log out everywhere"; resolves when done (the caller navigates away). */
  revokeAll?: () => Promise<void>;
  /** Card blurb — defaults to the own-account wording. */
  description?: string;
}

export function UserSessionsCard({ load: loadRows, end, revokeAll, description }: UserSessionsCardProps) {
  const [sessions, setSessions] = useState<LoginSession[] | null>(null);
  const [loadFailed, setLoadFailed] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [endTarget, setEndTarget] = useState<LoginSession | null>(null);
  const [ending, setEnding] = useState(false);
  const [endErr, setEndErr] = useState<string | null>(null);
  const listRef = useRef<HTMLUListElement>(null);
  // In the Studio work surface (specs/078 u7-nguoi-dung-chi-tiet) the confirm sits under the row, not in a dialog.
  const embedded = useEmbedded();

  // This browser first, then most recently used (the BE's order).
  const show = useCallback((rows: LoginSession[]) => {
    setSessions([...rows].sort((a, b) => Number(!!b.current) - Number(!!a.current)));
    setLoadFailed(false);
  }, []);
  // Imperative reload (retry button, after ending a row). `loadRows` must be a stable identity.
  const load = useCallback(async () => {
    try {
      show(await loadRows());
    } catch {
      setLoadFailed(true);
    }
  }, [loadRows, show]);
  // First load: setState only inside the promise callbacks (react-hooks/set-state-in-effect).
  useEffect(() => {
    let alive = true;
    loadRows().then((rows) => { if (alive) show(rows); }, () => { if (alive) setLoadFailed(true); });
    return () => { alive = false; };
  }, [loadRows, show]);

  const doRevokeAll = async () => {
    if (!revokeAll) return;
    setBusy(true);
    try {
      await revokeAll();
    } catch {
      // The local session is already cleared by logoutEverywhere's finally block; what failed is
      // the remote revoke, so say THAT rather than a generic error.
      setBusy(false);
      setConfirmOpen(false);
      toast.error(t("page.profile.sessions.errRevoke"));
    }
  };

  const endSession = async () => {
    if (!endTarget) return;
    setEnding(true);
    try {
      await end(endTarget.id);
      toast.success(t("page.profile.sessions.endDone"));
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 404)) {
        setEnding(false);
        if (embedded) {
          setEndErr(t("studio.operate.sessions.errEnd")); // the question stays so the builder can try again
          return;
        }
        setEndTarget(null);
        toast.error(t("page.profile.sessions.errEnd"));
        return;
      }
      // 404: that device is already gone (ended elsewhere, expired) — say so and drop the row.
      toast.info(t("page.profile.sessions.errEndGone"));
    }
    // Reload BEFORE closing: the ended row (and its button, the dialog's opener) must be gone when
    // the dialog closes, or focus returns to that button and drops to <body> once it unmounts.
    await load();
    setEnding(false);
    setEndTarget(null);
    setEndErr(null);
  };

  const endDevice = endTarget ? deviceLabel(endTarget) : "";

  if (embedded) {
    return (
      <section aria-label={t("studio.operate.sessions.title")}>
        <h3 className="slds-text-heading_small">{t("studio.operate.sessions.title")}</h3>
        {description && <p className="slds-text-color_weak">{description}</p>}
        {loadFailed ? (
          <ScopedNotification theme="error" className="slds-m-top_small">
            {t("studio.operate.sessions.errLoad")}{" "}
            <Button variant="base" onClick={() => void load()}>{t("studio.query.retry")}</Button>
          </ScopedNotification>
        ) : sessions === null ? (
          <div className="slds-m-top_small">
            <Spinner size="small" inline delayed label={t("page.profile.sessions.loading")} />
          </div>
        ) : sessions.length === 0 ? (
          <p className="slds-m-top_small slds-text-color_weak">{t("studio.operate.sessions.empty")}</p>
        ) : (
          <ul className="slds-has-dividers_bottom-space slds-m-top_small" aria-label={t("studio.operate.sessions.title")}>
            {sessions.map((s) => (
              <SessionItem
                key={s.id}
                session={s}
                selected={endTarget?.id === s.id}
                onEnd={() => { setEndErr(null); setEndTarget(s); }}
                below={
                  endTarget?.id === s.id && (
                    <div className="slds-m-top_x-small">
                      {endErr && <ScopedNotification theme="error">{endErr}</ScopedNotification>}
                      <InlineConfirm
                        tone="warning"
                        message={tf("studio.operate.sessions.endConfirm", { device: endDevice })}
                        confirmLabel={t("page.profile.sessions.endAction")}
                        busy={ending}
                        onConfirm={() => void endSession()}
                        onCancel={() => { setEndTarget(null); setEndErr(null); }}
                      />
                    </div>
                  )
                }
              />
            ))}
          </ul>
        )}
      </section>
    );
  }

  return (
    <Card
      title={t("page.profile.sessions.title")}
      icon={{ glyph: Monitor, name: "device" }}
      count={sessions?.length}
    >
      <p className="slds-text-color_weak">{description ?? t("page.profile.sessions.desc")}</p>
      {loadFailed ? (
        <ScopedNotification theme="error" className="slds-m-top_small">
          {t("page.profile.sessions.errLoad")}{" "}
          <Button variant="base" onClick={() => void load()}>{t("page.profile.sessions.retry")}</Button>
        </ScopedNotification>
      ) : sessions === null ? (
        <div className="slds-m-top_small">
          <Spinner size="small" inline delayed label={t("page.profile.sessions.loading")} />
        </div>
      ) : sessions.length === 0 ? (
        <p className="slds-m-top_small">{t("page.profile.sessions.empty")}</p>
      ) : (
        <ul
          ref={listRef}
          tabIndex={-1}
          className="slds-has-dividers_bottom-space slds-m-top_small"
          aria-label={t("page.profile.sessions.title")}
        >
          {sessions.map((s) => <SessionItem key={s.id} session={s} onEnd={() => setEndTarget(s)} />)}
        </ul>
      )}
      {/* One confirm for every row; the row's button is not a Radix trigger (it is gone once the
          session ends — endSession reloads before closing), so focus goes back to the list. */}
      <Modal
        open={endTarget !== null}
        onOpenChange={(open) => { if (!open && !ending) setEndTarget(null); }}
        returnFocus={() => listRef.current}
        title={t("page.profile.sessions.endTitle")}
        size="small"
        footer={
          <>
            <Button disabled={ending} onClick={() => setEndTarget(null)}>{t("action.cancel")}</Button>
            <Button variant="destructive" disabled={ending} onClick={endSession}>
              {ending ? t("page.profile.sessions.confirmWorking") : t("page.profile.sessions.endAction")}
            </Button>
          </>
        }
      >
        <p>{tf("page.profile.sessions.endBody", { device: endDevice })}</p>
      </Modal>
      {revokeAll && (
        // The opener is the Modal's trigger, so Radix returns focus to it on close; the modal
        // itself renders in a portal, so this div still holds just the button.
        <div className="slds-m-top_small">
          <Modal
            trigger={<Button iconLeft={LogOut}>{t("page.profile.sessions.logoutAll")}</Button>}
            open={confirmOpen}
            onOpenChange={(open) => { if (!busy) setConfirmOpen(open); }}
            title={t("page.profile.sessions.confirmTitle")}
            size="small"
            footer={
              <>
                <Button disabled={busy} onClick={() => setConfirmOpen(false)}>{t("action.cancel")}</Button>
                <Button variant="destructive" disabled={busy} onClick={doRevokeAll}>
                  {busy ? t("page.profile.sessions.confirmWorking") : t("page.profile.sessions.confirmAction")}
                </Button>
              </>
            }
          >
            <p>{t("page.profile.sessions.confirmBody")}</p>
            <ul className="slds-list_dotted slds-m-top_small">
              <li>{t("page.profile.sessions.impactThis")}</li>
              <li>{t("page.profile.sessions.impactOthers")}</li>
            </ul>
          </Modal>
        </div>
      )}
    </Card>
  );
}

// "Chrome · macOS", or "Thiết bị không xác định" when the BE could not name it.
function deviceLabel(s: LoginSession): string {
  return [s.browser, s.os].filter(Boolean).join(" · ") || t("page.profile.sessions.unknownDevice");
}

// One session: blueprint `lists` (slds-has-dividers_bottom-space) item with a media object —
// device icon, "Chrome · macOS" (+ "Phiên này" success badge), "IP 113.161.42.x · Hoạt động lần cuối
// 08:15 30/09/2026", and on every OTHER row a reverse-figure "Đăng xuất" button.
function SessionItem({ session: s, onEnd, below, selected }: { session: LoginSession; onEnd: () => void; below?: ReactNode; selected?: boolean }) {
  const mobile = s.os === "iOS" || s.os === "Android";
  const device = deviceLabel(s);
  const meta = [
    s.ip ? tf("page.profile.sessions.ip", { ip: s.ip }) : null,
    tf("page.profile.sessions.lastUsed", { when: formatDateTime(s.lastUsedAt) }),
  ].filter(Boolean).join(" · ");
  return (
    <li className={selected ? "slds-item slds-is-selected" : "slds-item"}>
      <div className="slds-media slds-media_center">
        <div className="slds-media__figure">
          <Icon icon={mobile ? Smartphone : Monitor} size="small" />
        </div>
        <div className="slds-media__body">
          <p className="slds-text-heading_small slds-truncate">
            <span title={s.userAgent || device}>{device}</span>
            {s.current && (
              <>
                {" "}
                <Badge theme="success">{t("page.profile.sessions.current")}</Badge>
              </>
            )}
          </p>
          <p className="slds-text-body_small slds-text-color_weak">{meta}</p>
        </div>
        {!s.current && (
          <div className="slds-media__figure slds-media__figure_reverse">
            <Button onClick={onEnd} aria-label={tf("page.profile.sessions.endAria", { device })}>
              {t("page.profile.sessions.end")}
            </Button>
          </div>
        )}
      </div>
      {below}
    </li>
  );
}
