// SSO / Đăng nhập một lần — admin CRUD cho sso_providers (ENG-AUTH2 #8, api-conventions §3c.4).
// Gate RequireAuth → RequireAdmin nằm ở app/admin/layout.tsx (dùng chung cho mọi /admin/*).
import { SsoProviders } from "@/components/screens/admin/SsoProviders";

export default function AdminSsoPage() {
  return <SsoProviders />;
}
