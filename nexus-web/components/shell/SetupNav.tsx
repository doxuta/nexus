"use client";

// SetupNav — the Setup column of /admin/* (specs/038 T047; RESEARCH §5 "Admin (Setup)":
// blueprint vertical-navigation/list, the Setup tree on the left). Same pages, order and operator
// tier as the header's Setup gear: both read ADMIN_LINKS. The page under a nested route
// (/admin/users/…) stays marked.
import { usePathname } from "next/navigation";
import { VerticalNavigation } from "@/components/slds/VerticalNavigation";
import { ADMIN_LINKS } from "@/components/slds/shell/adminLinks";
import { useAuth } from "@/lib/auth/AuthProvider";
import { t } from "@/lib/i18n";

export function SetupNav({ className }: { className?: string }) {
  const pathname = usePathname();
  const { user } = useAuth();
  const isOperator = !!user?.isPlatformOperator;
  const items = ADMIN_LINKS.filter((l) => !l.operatorOnly || isOperator).map((l) => ({
    key: l.href,
    label: t(l.label),
    href: l.href,
    active: pathname === l.href || pathname.startsWith(`${l.href}/`),
  }));
  return (
    <VerticalNavigation
      label={t("shell.setupNav.label")}
      sections={[{ id: "admin", title: t("slds.globalHeader.admin"), items }]}
      compact
      shade
      className={className}
    />
  );
}
