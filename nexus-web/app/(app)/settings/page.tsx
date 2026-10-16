import { PreferencesScreen } from "@/components/screens/PreferencesScreen";

// /settings — User preferences (client-only, localStorage nexus.prefs.v1).
// Previous: redirected to /admin/roles (Slice 5); now: proper preferences page.
// ACL / Role management is at /admin/roles.
export default function SettingsPage() {
  return <PreferencesScreen />;
}
