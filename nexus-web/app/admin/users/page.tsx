// C1 — User List · C2 — Create User · C3 — User Detail/Edit
// (state machine inside UserList component handles all three sub-views)
import { UserList } from "@/components/screens/admin/UserList";

export default function AdminUsersPage() {
  return <UserList />;
}
