// /settings/users — "Cài đặt › Người dùng" of the customer owner (specs/065 lát B): see every account of the
// tenant, give an existing role, lock/reopen staff. The gate is client-side UX only (fail-closed); the
// server re-checks users.can_manage_users on every /org-admin/* call.
import { UserList } from "@/components/screens/admin/UserList";
import { RequireUserManager } from "@/lib/auth/guards";

export default function SettingsUsersPage() {
  return (
    <RequireUserManager>
      <UserList mode="owner" />
    </RequireUserManager>
  );
}
