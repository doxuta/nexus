"use client";

// Studio navigation family (specs/073 u0-dieu-huong): Path mode="steps", Tabs mode="links", Button
// stateful, GlobalHeader with the Studio slots. Static sample data; links go to anchors on this page.
import { useState } from "react";
import { Button } from "@/components/slds/Button";
import { Path, type PathStep } from "@/components/slds/Path";
import { Tabs, type TabLink } from "@/components/slds/Tabs";
import { GlobalHeader, type GlobalHeaderProps } from "@/components/slds/shell/GlobalHeader";

const noop = () => {};

function Demo({ id, title, children }: { id: string; title: string; children: React.ReactNode }) {
  return (
    <div id={id} className="slds-m-bottom_large">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

const STEPS: PathStep[] = [
  { key: "data", label: "Dữ liệu", sublabel: "3 bảng", complete: true, href: "#kit-studio-path" },
  { key: "links", label: "Quan hệ", complete: true, href: "#kit-studio-path" },
  { key: "screens", label: "Màn hình", complete: false, href: "#kit-studio-path" },
  { key: "rules", label: "Quy tắc", complete: false, href: "#kit-studio-path" },
  { key: "approvals", label: "Phê duyệt", complete: null, href: "#kit-studio-path" },
  { key: "reports", label: "Báo cáo", complete: null, href: "#kit-studio-path" },
  { key: "access", label: "Quyền", complete: false, href: "#kit-studio-path" },
  { key: "publish", label: "Xuất bản", complete: null, href: "#kit-studio-path" },
];

const ZONES: TabLink[] = [
  { value: "build", label: "Dựng", href: "#kit-studio-tabs" },
  { value: "run", label: "Chạy thử", href: "#kit-studio-tabs", dot: true },
  { value: "deliver", label: "Giao khách", href: "#kit-studio-tabs" },
];

const SUB: TabLink[] = [
  { value: "tables", label: "Bảng", href: "#kit-studio-tabs" },
  { value: "fields", label: "Trường", href: "#kit-studio-tabs", dot: true },
  { value: "layouts", label: "Bố cục", href: "#kit-studio-tabs" },
  { value: "picklists", label: "Danh sách chọn", href: "#kit-studio-tabs" },
  { value: "formulas", label: "Công thức", href: "#kit-studio-tabs" },
  { value: "validation", label: "Kiểm tra dữ liệu", href: "#kit-studio-tabs" },
  { value: "indexes", label: "Chỉ mục", href: "#kit-studio-tabs" },
];

const HEADER: GlobalHeaderProps = {
  brandName: "Nexus",
  brandSuffix: "Studio",
  searchInMenu: true,
  hideSetup: true,
  onSearch: noop,
  favorites: [],
  onHelp: noop,
  isAdmin: true,
  setupLinks: [],
  notifications: { unread: 0, items: [], onRead: noop, onMarkAll: noop },
  user: { name: "Xuân Tài", subtitle: "Người dựng · northwind" },
  onNavigate: noop,
  onLogout: noop,
};

export function StudioNavSection() {
  const [assistant, setAssistant] = useState(false);
  const [pressed, setPressed] = useState(false);
  return (
    <>
      <Demo id="kit-studio-path" title="Path mode=steps — path/base · 8 bước: đủ / chưa đủ / chưa biết (null) · bước đang mở là đoạn đặc, không nút hành động">
        <Path mode="steps" label="Các bước dựng" steps={STEPS} current="screens" />
      </Demo>
      <Demo id="kit-studio-tabs" title="Tabs mode=links — tabs/base · khu (medium, chấm có việc chờ) và mục con (cuộn ngang trong chính nó ở 375px)">
        <Tabs mode="links" label="Khu của dự án" items={ZONES} current="build" size="medium" />
        <div className="slds-m-top_medium" style={{ maxWidth: 375 }}>
          <Tabs mode="links" label="Mục trong khu" items={SUB} current="validation" size="medium" />
        </div>
      </Demo>
      <Demo id="kit-studio-stateful" title="Button stateful — buttons/stateful · aria-pressed">
        <Button stateful selected={pressed} onClick={() => setPressed((p) => !p)}>
          {pressed ? "Đang bật" : "Đang tắt"}
        </Button>
      </Demo>
      <Demo id="kit-studio-header" title="GlobalHeader — chỗ gắn bộ chuyển dự án + nút Trợ lý (stateful), không bánh răng Thiết lập; dưới 768px: ⌘K vào menu tài khoản">
        <GlobalHeader
          {...HEADER}
          switcher={<Button className="whitespace-nowrap">Minh Phát CRM</Button>}
          assistantToggle={{ pressed: assistant, onToggle: () => setAssistant((a) => !a) }}
        />
      </Demo>
    </>
  );
}
