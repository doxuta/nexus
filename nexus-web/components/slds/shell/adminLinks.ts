// /admin/* pages — ONE list for both Setup surfaces: the header's Setup gear (GlobalHeader) and the
// Setup column of the admin area (components/shell/SetupNav). Same order and operator tier as the
// old sidebar QUẢN TRỊ. Doctor and API keys moved into the project frame (bao-tri/kham, van-hanh/tu-dong/khoa-truy-cap): kh7.
import type { I18nKey } from "@/lib/i18n/vi";

/** The ONLY Setup entry of a customer owner (specs/065): not an admin, so none of ADMIN_LINKS applies. */
export const OWNER_USERS_LINK: { href: string; label: I18nKey } = { href: "/settings/users", label: "settings.users.menu" };

export const ADMIN_LINKS: ReadonlyArray<{ href: string; label: I18nKey; operatorOnly?: boolean }> = [
  { href: "/admin/users", label: "shell.nav.users" },
  { href: "/admin/roles", label: "shell.nav.roles" },
  { href: "/admin/teams", label: "shell.nav.teams" },
  { href: "/admin/field-acl", label: "shell.nav.fieldAcl" },
  { href: "/admin/sso", label: "slds.globalHeader.sso" },
  // The customer's own admin keeps partner keys here (adminGated, not operator): specs/050 S8a.
  { href: "/admin/credentials", label: "slds.globalHeader.credentials" },
  { href: "/admin/business-calendars", label: "slds.globalHeader.calendars" },
  { href: "/admin/audit", label: "shell.nav.audit" },
  // The customer's own admin reads their delivery log here (adminGated, not operator): "why didn't
  // the partner get the order?" (DAILY-QUEUE #33).
  { href: "/admin/jobs", label: "shell.nav.jobs" },
  // Same persona, same gate: which emails did the rules send, which failed (specs/066 L1b).
  { href: "/admin/email-log", label: "shell.nav.emailLog" },
  // How much the automation did, per day: rule runs, mail, webhooks out and in (specs/066 L4).
  { href: "/admin/usage", label: "shell.nav.usage" },
  // Is the customer's app running well: failed jobs, rules, mail, inbound webhooks (specs/066 L6).
  { href: "/admin/health", label: "shell.nav.health" },
  { href: "/admin/sharing", label: "shell.nav.sharing" },
];
