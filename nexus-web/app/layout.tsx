import type { Metadata } from "next";
import { Inter } from "next/font/google";
import "./globals.css";
import { NavigationBlockerProvider } from "@/lib/navigation-blocker";
import { NexusQueryProvider } from "@/lib/query/client";
import { AuthProvider } from "@/lib/auth/AuthProvider";
import { ActiveAppProvider } from "@/lib/apps";
import { Toaster } from "@/components/slds/Toast";
import { SessionExpiredGuard } from "@/components/ui/SessionExpiredGuard";
import { VersionWarningToast } from "@/components/shell/VersionWarningToast";
import { BrandingBoot } from "@/components/brand/BrandingBoot";
import { HtmlLang } from "@/lib/i18n";

// Inter (OFL-1.1): full Vietnamese subset AND a `tnum` feature, so numeric grid cells align
// (RESEARCH-2026-09 §6.2 — Be Vietnam Pro has no tnum, which made every tabular-nums a no-op).
// SLDS reads the face through --slds-g-font-family-base (app/globals.css). No serif and no
// separate mono face: SLDS has no serif role and ships its own monospace stack.
const sans = Inter({
  subsets: ["latin", "vietnamese"],
  // Optical size axis (specs/073 FR-003): Inter's display cut for large headings. Same file count, one family.
  axes: ["opsz"],
  variable: "--font-sans",
  display: "swap",
});

// Tiêu đề tab của MỌI trang, kể cả mặt khách. "Nexus — Nền tảng metadata-driven" là tên và
// nghề của cái XƯỞNG; nhân viên khách hàng thấy nó trên mọi tab và mọi bookmark của họ
// (MỘT XƯỞNG TRỤ 3). Tên thương hiệu thật của từng tenant do BrandingBoot đặt lúc chạy —
// đây chỉ là nhãn trung tính trước khi biết mình đang phục vụ ai.
export const metadata: Metadata = {
  title: "Nexus",
  description: "Phần mềm quản lý công việc và dữ liệu của doanh nghiệp.",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html
      lang="vi"
      className={`${sans.variable} h-full antialiased`}
    >
      <body className="min-h-full">
        {/* AO-THUONG-HIEU: cached branding applied on mount (accent vars/favicon/title), then
            refreshed from the public endpoint — root-mounted so /login is branded too. */}
        <BrandingBoot />
        <HtmlLang />
        <NexusQueryProvider>
          <AuthProvider>
            {/* Active-app lens (Phase 2) — ROOT-mounted so the app Sidebar switcher AND the
                Studio create-app dialog share ONE state; queries are gated on !!user. */}
            <ActiveAppProvider>
              <NavigationBlockerProvider>
                {children}
              </NavigationBlockerProvider>
            </ActiveAppProvider>
            <Toaster />
            <VersionWarningToast />
            <SessionExpiredGuard />
          </AuthProvider>
        </NexusQueryProvider>
      </body>
    </html>
  );
}
