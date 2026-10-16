"use client";

// One customer-owner row of "Giao cho khách" (specs/077 f5-handoff-owner): the sentence, one "Đổi vai" button, and an
// in-place form strip (no dialog). The ONLY write is PUT /admin/users/:id with a body of exactly {roleId}: that route
// also accepts isAdmin/password/status, so this form must never send them. The server decides which roles are valid.
import { CircleCheck } from "lucide-react";
import { useRef, useState } from "react";
import { Button } from "@/components/slds/Button";
import { Icon } from "@/components/slds/Icon";
import { Select } from "@/components/slds/Select";
import { updateUser } from "@/lib/admin";
import type { AdminUser, RoleSummary } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";

export function OwnerAccountRow({ owner, roles, onChanged }: { owner: AdminUser; roles: RoleSummary[]; onChanged: () => void }) {
  const [open, setOpen] = useState(false);
  // Current role missing/unknown: start empty WITH a placeholder option so the form never shows (or submits) a first choice silently.
  const known = roles.some((r) => r.id === owner.roleId);
  const initial = known ? (owner.roleId as string) : "";
  const [roleId, setRoleId] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inFlight = useRef(false); // state is stale between two fast clicks
  const opener = useRef<HTMLButtonElement>(null);
  const role = roles.find((r) => r.id === owner.roleId);

  const close = () => {
    setOpen(false);
    setError(null);
    opener.current?.focus();
  };
  const save = async () => {
    if (inFlight.current || !roleId) return; // empty roleId would CLEAR the role server-side
    inFlight.current = true;
    setBusy(true);
    setError(null);
    try {
      await updateUser(owner.id, { roleId });
      setOpen(false);
      opener.current?.focus();
      onChanged();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  };

  return (
    <>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <Icon icon={CircleCheck} size="x-small" tone="success" label={t("page.appOverview.readiness.pass")} />
        <span className="flex-1" style={{ minWidth: "12rem" }}>
          {role
            ? tf("studio.build.handover.row.owner.ok", { email: owner.email, role: role.label || role.name })
            : tf("studio.build.handover.row.owner.noRole", { email: owner.email })}
        </span>
        <Button ref={opener} variant="neutral" aria-expanded={open} onClick={() => { setRoleId(initial); setError(null); setOpen(true); }}>
          {t("studio.build.handover.action.changeRole")}
        </Button>
      </div>
      {open && (
        <form
          className="slds-theme_shade slds-p-around_small slds-m-top_x-small"
          aria-label={t("studio.build.handover.action.changeRole")}
          onKeyDown={(e) => { if (e.key === "Escape") { e.stopPropagation(); close(); } }}
          onSubmit={(e) => { e.preventDefault(); void save(); }}
        >
          <Select
            label={t("studio.build.handover.row.owner.roleLabel")}
            layout="stacked"
            autoFocus
            placeholder={known ? false : undefined}
            value={roleId}
            error={error ?? undefined}
            help={t("studio.build.handover.row.owner.roleHint")}
            onChange={(e) => setRoleId(e.target.value)}
            options={roles.map((r) => ({ value: r.id, label: r.label || r.name }))}
          />
          <div className="flex gap-2 slds-m-top_x-small">
            <Button type="submit" variant="brand" aria-disabled={busy || !roleId || undefined}>
              {t("studio.build.handover.action.save")}
            </Button>
            <Button type="button" variant="neutral" onClick={close}>
              {t("studio.build.handover.action.cancel")}
            </Button>
          </div>
        </form>
      )}
    </>
  );
}
