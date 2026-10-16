// Kho khoá kết nối — quản trị khách cất khoá đối tác (specs/050 S8a).
// Gate RequireAuth → RequireAdmin nằm ở app/admin/layout.tsx.
import { NamedCredentials } from "@/components/screens/admin/NamedCredentials";

export default function AdminCredentialsPage() {
  return <NamedCredentials />;
}
