"use client";

// Shell family (specs/038 T016, contracts/shell.md): GlobalHeader (global-header/base),
// GlobalNavigation (global-navigation/navigation-bar), AppLauncher (app-launcher/base). The
// presentational components on static sample data; AppShell mounts the session-wired
// ShellGlobalHeader / ShellGlobalNavigation. Each block carries an id for screenshots.
import { useState } from "react";
import type { ApiNotification } from "@/lib/api";
import { AppLauncher } from "@/components/slds/shell/AppLauncher";
import { GlobalHeader, type GlobalHeaderProps } from "@/components/slds/shell/GlobalHeader";
import { GlobalNavigation, type NavTab } from "@/components/slds/shell/GlobalNavigation";

const noop = () => {};

function Demo({ id, title, children }: { id: string; title: string; children: React.ReactNode }) {
  return (
    <div id={id} className="slds-m-bottom_large">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

const NOTIS = [
  { id: "n1", type: "assignment", title: "Bạn được giao khách hàng mới", message: "Công ty TNHH Minh Phát — phụ trách từ hôm nay", href: "#", isRead: false, createdAt: "2026-09-29T02:00:00Z" },
  { id: "n2", type: "approval", title: "Cần duyệt báo giá", message: "BG-2026-0142 · 184.500.000 ₫", href: "#", isRead: false, createdAt: "2026-09-28T09:30:00Z" },
  { id: "n3", type: "system", message: "Đã nhập xong 120 dòng liên hệ từ tệp CSV.", href: "#", isRead: true, createdAt: "2026-09-27T08:00:00Z" },
] as ApiNotification[];

const HEADER_BASE: GlobalHeaderProps = {
  brandName: "Minh Phát CRM",
  onSearch: noop,
  favorites: [],
  onHelp: noop,
  isAdmin: false,
  setupLinks: [],
  notifications: { unread: 0, items: [], onRead: noop, onMarkAll: noop },
  user: { name: "Trần Văn Bình", subtitle: "Nhân viên bán hàng · northwind" },
  onNavigate: noop,
  onLogout: noop,
};

const APPS = [
  { name: "crm", label: "Quản lý Bán hàng", icon: "briefcase", color: "ac" },
  { name: "helpdesk", label: "Chăm sóc khách hàng", icon: "ticket", color: "wn" },
  { name: "lms", label: "Đào tạo nội bộ", icon: "graduation-cap", color: "ok" },
];
const ITEMS = [
  { href: "/dashboards", label: "Bảng điều khiển" },
  { href: "/reports", label: "Báo cáo" },
  { href: "/crm_accounts", label: "Khách hàng" },
  { href: "/crm_contacts", label: "Liên hệ" },
  { href: "/crm_deals", label: "Cơ hội" },
  { href: "/crm_quotes", label: "Báo giá" },
  { href: "/tickets", label: "Phiếu hỗ trợ" },
];

const TABS: NavTab[] = [
  { href: "/home", label: "Trang chủ" },
  { href: "/approvals", label: "Phê duyệt", count: 3, countLabel: "3 yêu cầu đang chờ duyệt" },
  { href: "/notifications", label: "Thông báo", count: 2, countLabel: "2 thông báo chưa đọc" },
  { href: "/crm_accounts", label: "Khách hàng" },
  { href: "/crm_contacts", label: "Liên hệ" },
  { href: "/crm_deals", label: "Cơ hội" },
  { href: "/crm_quotes", label: "Báo giá" },
  { href: "/crm_contracts", label: "Hợp đồng dịch vụ" },
];

function Launcher() {
  const [active, setActive] = useState("crm");
  return <AppLauncher apps={APPS} activeApp={active} items={ITEMS} onSelectApp={setActive} />;
}

export function ShellSection() {
  const [pinned, setPinned] = useState(true);
  return (
    <>
      <Demo id="kit-global-header-rep" title="GlobalHeader — global-header/base · nhân viên (không Thiết lập, trang không ghim được, 0 thông báo)">
        <GlobalHeader {...HEADER_BASE} />
      </Demo>
      <Demo id="kit-global-header-admin" title="GlobalHeader — quản trị viên · đang ở trang Khách hàng (ghim) · 3 thông báo chưa đọc">
        <GlobalHeader
          {...HEADER_BASE}
          isAdmin
          searchPlaceholder="Tìm trong khách hàng…"
          pinTarget={{ label: "Khách hàng", pinned, onToggle: () => setPinned((p) => !p) }}
          favorites={pinned ? [{ href: "/crm_accounts", label: "Khách hàng" }] : []}
          setupLinks={[
            { href: "/admin/users", label: "Người dùng" },
            { href: "/admin/roles", label: "Vai trò & Phân quyền" },
            { href: "/admin/teams", label: "Nhóm" },
            { href: "/admin/audit", label: "Nhật ký" },
          ]}
          studioHref="/studio"
          notifications={{ unread: 3, items: NOTIS, onRead: noop, onMarkAll: noop }}
          user={{ name: "Nguyễn Thị Lan", subtitle: "Quản trị viên · northwind" }}
        />
      </Demo>
      <Demo id="kit-global-nav" title="GlobalNavigation — global-navigation/navigation-bar · tab đang mở (Khách hàng) · badge · waffle mở AppLauncher (app-launcher/base)">
        <GlobalNavigation appName="Quản lý Bán hàng" items={TABS} pathname="/crm_accounts" launcher={<Launcher />} onNavigate={noop} />
      </Demo>
      <Demo id="kit-global-nav-overflow" title="GlobalNavigation — tràn vào “Thêm” (tối đa 4 tab, tab đang mở giữ chỗ cuối)">
        <GlobalNavigation appName="Tất cả dữ liệu" items={TABS} pathname="/crm_quotes" launcher={<Launcher />} maxVisible={4} onNavigate={noop} />
      </Demo>
    </>
  );
}
