"use client";

// "Chia sẻ" (specs/051-record-manual-share, DAILY-QUEUE #35) — SLDS modal (modals/base) holding a
// people lookup (lookups blueprint = combobox autocomplete, the same kit ComboboxField as OwnerDialog)
// with its "Chia sẻ" button, and under it the people this record is shared with: tiles with an avatar
// (tiles/with-avatar + with-action) in a divided list (slds-has-dividers_bottom-space), each with a
// bordered x-small icon button that revokes the share.
// Shares are READ-ONLY (Q4 2026-09-05) and authorization is server-side (edit grant + record in the
// caller's edit scope): a 403 RECORD_SHARED_READ_ONLY / 404 / 422 shows the server's own sentence here.
import { useEffect, useId, useState } from "react";
import { UserRound, Users, X } from "lucide-react";
import {
  ensureAuth, listRecordShares, listTeamDirectory, shareRecord, shareRecordWithTeam, unshareRecord, ApiError,
  type DirectoryUser, type RecordShare, type TeamDirectoryEntry,
} from "@/lib/api";
import { formatDate } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Avatar } from "@/components/slds/Avatar";
import { Icon } from "@/components/slds/Icon";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { ComboboxField, type ComboboxOption } from "@/components/slds/Combobox";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { useUserPicker } from "./useUserPicker";

function toOption(u: DirectoryUser): ComboboxOption {
  return {
    value: u.id,
    label: u.fullName || u.email,
    meta: u.email,
    icon: { glyph: UserRound, name: "user", label: t("page.record.owner.userKind") },
  };
}

function toTeamOption(team: TeamDirectoryEntry): ComboboxOption {
  return {
    value: team.id,
    label: team.name,
    icon: { glyph: Users, name: "groups", label: t("page.record.share.teamKind") },
  };
}

// A share row's grantee: the team's id for a team share, else the person's.
const granteeOf = (s: RecordShare) => s.teamId || s.userId || "";

const errText = (e: unknown, fallback: string) => (e instanceof ApiError ? e.message : fallback);

export function ShareDialog({ entity, recordId, currentUserId, open, onOpenChange }: {
  entity: string;
  recordId: string;
  // The signed-in user — never offered as a grantee (the server answers 422 RECORD_SHARE_SELF).
  currentUserId?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const [shares, setShares] = useState<RecordShare[] | null>(null);
  const [shareErr, setShareErr] = useState("");
  const [filter, setFilter] = useState("");
  const [selectedId, setSelectedId] = useState("");
  // The team row: a second picker over GET /teams/directory (a failed read only disables this row).
  const [teams, setTeams] = useState<TeamDirectoryEntry[] | null>(null);
  const [teamErr, setTeamErr] = useState("");
  const [teamFilter, setTeamFilter] = useState("");
  const [selectedTeamId, setSelectedTeamId] = useState("");
  const [busy, setBusy] = useState(false);
  const [errMsg, setErrMsg] = useState("");
  const listHeadingId = useId();
  // First directory page loads on open; typing searches the server (?q=) — see useUserPicker.
  const picker = useUserPicker(open, filter, t("page.record.share.loadError"));
  const loadErr = shareErr || picker.loadErr;

  // Every opening starts clean (adjusting state during render, not in an effect).
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    if (open) {
      setShares(null);
      setShareErr("");
      setErrMsg("");
      setSelectedId("");
      setFilter("");
      setTeams(null);
      setTeamErr("");
      setTeamFilter("");
      setSelectedTeamId("");
    }
  }

  useEffect(() => {
    if (!open) return;
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const sh = await listRecordShares(entity, recordId);
        if (alive) setShares(sh);
      } catch (e) {
        // A refused or failed read shows its real error — never an empty list that reads as "nobody".
        if (alive) setShareErr(errText(e, t("page.record.share.loadError")));
      }
    })();
    (async () => {
      try {
        await ensureAuth();
        const ts = await listTeamDirectory();
        if (alive) setTeams(ts);
      } catch (e) {
        if (alive) setTeamErr(errText(e, t("page.record.share.teamLoadError")));
      }
    })();
    return () => { alive = false; };
  }, [open, entity, recordId]);

  const nameOf = (id: string) => {
    const u = picker.find(id);
    const s = shares?.find((x) => x.userId === id);
    return u?.fullName?.trim() || u?.email || s?.userName || s?.userEmail || id;
  };
  const teamNameOf = (id: string) =>
    teams?.find((x) => x.id === id)?.name || shares?.find((x) => x.teamId === id)?.teamName ||
    // Directory loaded but the team is not in it: the team was deleted, the share row is an orphan.
    (teams ? t("page.record.share.teamGone") : id);

  // A failed re-read after a successful change is its own error: the change itself went through.
  const reload = async () => {
    try {
      setShares(await listRecordShares(entity, recordId));
    } catch (e) {
      setErrMsg(errText(e, t("page.record.share.loadError")));
    }
  };

  const mutate = async (action: () => Promise<unknown>, done: string, fallback: string) => {
    setBusy(true);
    setErrMsg("");
    try {
      await ensureAuth();
      await action();
    } catch (e) {
      setErrMsg(errText(e, fallback));
      setBusy(false);
      return false;
    }
    toast.success(done);
    await reload();
    setBusy(false);
    return true;
  };

  const submit = async () => {
    if (!selectedId || busy) return;
    const id = selectedId;
    const ok = await mutate(() => shareRecord(entity, recordId, id), tf("page.record.share.doneNamed", { name: nameOf(id) }), t("page.record.share.error"));
    if (ok) {
      setSelectedId("");
      setFilter("");
    }
  };

  const submitTeam = async () => {
    if (!selectedTeamId || busy) return;
    const id = selectedTeamId;
    const ok = await mutate(() => shareRecordWithTeam(entity, recordId, id), tf("page.record.share.doneNamed", { name: teamNameOf(id) }), t("page.record.share.error"));
    if (ok) {
      setSelectedTeamId("");
      setTeamFilter("");
    }
  };

  const revoke = (s: RecordShare) => {
    const name = s.teamId ? teamNameOf(s.teamId) : nameOf(s.userId ?? "");
    void mutate(() => unshareRecord(entity, recordId, granteeOf(s)), tf("page.record.share.removedNamed", { name }), t("page.record.share.removeError"));
  };

  const sharedIds = new Set((shares ?? []).map(granteeOf));
  const candidates = picker.visible.filter((u) => u.id !== currentUserId && !sharedIds.has(u.id));
  const chosen = picker.find(selectedId);
  const teamCandidates = (teams ?? []).filter((x) => !sharedIds.has(x.id));
  const chosenTeam = teams?.find((x) => x.id === selectedTeamId);

  return (
    <Modal
      open={open}
      onOpenChange={busy ? () => {} : onOpenChange}
      title={t("page.record.share.title")}
      tagline={t("page.record.share.tagline")}
      footer={
        <Button onClick={() => onOpenChange(false)} disabled={busy}>
          {t("action.close")}
        </Button>
      }
    >
      {loadErr ? (
        <ScopedNotification theme="warning">{loadErr}</ScopedNotification>
      ) : !picker.ready || shares === null ? (
        <div className="relative h-16">
          <Spinner size="small" label={t("action.loading")} />
        </div>
      ) : (
        <>
          <div className="flex items-end gap-2">
            <div className="flex-1 min-w-0">
              <ComboboxField
                mode="autocomplete"
                label={t("page.record.share.search")}
                placeholder={t("page.record.share.searchPlaceholder")}
                options={candidates.map(toOption)}
                selected={chosen ? [toOption(chosen)] : []}
                onSelect={(o) => setSelectedId(o.value)}
                onDeselect={() => setSelectedId("")}
                inputValue={filter}
                onInputChange={setFilter}
                noResultsText={t("page.record.share.noMatch")}
              />
            </div>
            <Button variant="brand" onClick={() => void submit()} disabled={busy || !selectedId}>
              {t("page.record.share.submit")}
            </Button>
          </div>

          {teamErr ? (
            <ScopedNotification theme="warning" className="slds-m-top_small">{teamErr}</ScopedNotification>
          ) : teams && teams.length > 0 ? (
            <div className="flex items-end gap-2 slds-m-top_small">
              <div className="flex-1 min-w-0">
                <ComboboxField
                  mode="autocomplete"
                  label={t("page.record.share.teamSearch")}
                  placeholder={t("page.record.share.teamSearchPlaceholder")}
                  options={teamCandidates.map(toTeamOption)}
                  selected={chosenTeam ? [toTeamOption(chosenTeam)] : []}
                  onSelect={(o) => setSelectedTeamId(o.value)}
                  onDeselect={() => setSelectedTeamId("")}
                  inputValue={teamFilter}
                  onInputChange={setTeamFilter}
                  noResultsText={t("page.record.share.teamNoMatch")}
                />
              </div>
              <Button variant="brand" onClick={() => void submitTeam()} disabled={busy || !selectedTeamId}>
                {t("page.record.share.teamSubmit")}
              </Button>
            </div>
          ) : null}

          <h2 id={listHeadingId} className="slds-text-title_caps slds-m-top_medium slds-m-bottom_x-small">
            {t("page.record.share.listTitle")}
          </h2>
          {shares.length === 0 ? (
            <p className="slds-text-color_weak">{t("page.record.share.empty")}</p>
          ) : (
            <ul className="slds-has-dividers_bottom-space" aria-labelledby={listHeadingId}>
              {shares.map((s) => {
                const isTeam = !!s.teamId;
                const name = isTeam
                  ? s.teamName?.trim() || teamNameOf(s.teamId ?? "")
                  : s.userName?.trim() || s.userEmail || nameOf(s.userId ?? "");
                return (
                  <li key={granteeOf(s)} className="slds-item">
                    <article className="slds-tile slds-media slds-hint-parent">
                      <div className="slds-media__figure">
                        {isTeam ? (
                          <Icon icon={Users} category="standard" name="groups" label={t("page.record.share.teamKind")} />
                        ) : (
                          <Avatar name={name} shape="circle" size="small" />
                        )}
                      </div>
                      <div className="slds-media__body">
                        <div className="slds-grid slds-grid_align-spread slds-has-flexi-truncate">
                          <h3 className="slds-tile__title slds-truncate" title={name}>{name}</h3>
                          <div className="slds-shrink-none">
                            <ButtonIcon
                              icon={X}
                              label={tf("page.record.share.remove", { name })}
                              variant="border-filled"
                              size="x-small"
                              disabled={busy}
                              onClick={() => revoke(s)}
                            />
                          </div>
                        </div>
                        <div className="slds-tile__detail">
                          {isTeam && <p className="slds-truncate">{t("page.record.share.teamMembers")}</p>}
                          {s.userEmail && s.userEmail !== name && (
                            <p className="slds-truncate" title={s.userEmail}>{s.userEmail}</p>
                          )}
                          <p className="slds-text-color_weak slds-truncate">
                            {tf("page.record.share.sharedBy", { name: nameOf(s.sharedBy), date: formatDate(s.createdAt) })}
                          </p>
                        </div>
                      </div>
                    </article>
                  </li>
                );
              })}
            </ul>
          )}
        </>
      )}
      {errMsg && <ScopedNotification theme="error" className="slds-m-top_small">{errMsg}</ScopedNotification>}
    </Modal>
  );
}
