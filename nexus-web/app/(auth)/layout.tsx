// (auth) route group layout — public auth pages (login, forgot-password, reset-password,
// SSO complete) share ONE brand frame (AuthShell: SLDS brand band + one centred card).
// No app shell, no sidebar. Authentication state is handled inside each page component.
import { AuthShell } from "@/components/screens/AuthShell";

export default function AuthLayout({ children }: { children: React.ReactNode }) {
  return <AuthShell>{children}</AuthShell>;
}
