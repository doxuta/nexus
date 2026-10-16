"use client";

// DelegateButton — hand THIS approval step to a named person. Shared by the record Approvals tab
// (LayoutDetail) and the inbox deep-link (ApprovalDetail) so both speak one delegate UX.
//
// It names an INDIVIDUAL on purpose: delegating a role-targeted step "to the role" would be a
// no-op, since it already belonged to everyone holding it. The list comes from /users/directory,
// reachable by any authenticated user (unlike /admin/users), so a non-admin approver can actually
// use it — the exact failure that made the history tab useless for everyone but an admin.
//
// SLDS (specs/038 T031): a neutral button opening menus/dropdown (components/slds/Menu — Radix owns
// focus, arrows, typeahead, Esc), capped at 7 items tall; the directory loads on first open.
import { useState } from "react";
import { UserPlus } from "lucide-react";
import { listUserDirectory, type DirectoryUser } from "@/lib/api";
import { t } from "@/lib/i18n";
import { Menu, MenuItem } from "@/components/slds/Menu";

export function DelegateButton({ disabled, onPick }: { disabled: boolean; onPick: (userId: string) => void }) {
  const [users, setUsers] = useState<DirectoryUser[] | null>(null);
  const load = (open: boolean) => {
    if (!open || users?.length) return;
    listUserDirectory().then(setUsers).catch(() => setUsers([])); // refused → the honest empty note below
  };

  return (
    <Menu
      length={7}
      onOpenChange={load}
      trigger={
        <button type="button" className="slds-button slds-button_neutral" disabled={disabled} title={t("page.approvals.delegateTitle")}>
          <UserPlus aria-hidden="true" className="slds-button__icon slds-button__icon_left nx-lucide" />
          {t("page.approvals.delegate")}
        </button>
      }
    >
      {users === null ? (
        <MenuItem label={t("page.approvals.delegateLoading")} disabled />
      ) : users.length === 0 ? (
        <MenuItem label={t("page.approvals.delegateEmpty")} disabled />
      ) : (
        users.map((u) => <MenuItem key={u.id} label={u.fullName?.trim() || u.email} onSelect={() => onPick(u.id)} />)
      )}
    </Menu>
  );
}
