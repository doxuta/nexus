// Page-structure family (specs/038 §5.4): Breadcrumbs, Card, RelatedListCard, PageHeader, Tabs,
// VerticalTabs, Path. Static sample data only (the first Path keeps its stage in local state so
// the main button can be tried); each block names the blueprint/variant it copies and carries an id so it can be
// screenshotted on its own.
// Cards here sit inside the kit's own section card, and SLDS strips a nested card's border and
// shadow, so the samples pass `boundary` (blueprint `nested-with-boundary`) to stay visible.
// Buttons are raw blueprint markup (buttons / button-icons / button-groups); menus are left out
// until the Menu wrapper is merged, so triggers carry no aria-haspopup here.
import { useState, type ReactNode } from "react";
import {
  Briefcase,
  Building2,
  ChartColumn,
  CircleCheck,
  ChevronDown,
  Contact,
  Filter,
  ListTodo,
  Pencil,
  Plus,
  RefreshCw,
  Settings,
  Table,
  Ticket,
  type LucideIcon,
} from "lucide-react";
import { Breadcrumbs } from "@/components/slds/Breadcrumbs";
import { Card, RelatedListCard } from "@/components/slds/Card";
import { PageHeader } from "@/components/slds/PageHeader";
import { Path, type PathStage } from "@/components/slds/Path";
import { Tabs, type TabItem } from "@/components/slds/Tabs";
import { VerticalTabs } from "@/components/slds/VerticalTabs";

const CONTACTS = [
  { id: "c1", name: "Nguyễn Văn An", title: "Giám đốc điều hành", email: "an.nguyen@minhphat.vn", phone: "0903 112 233" },
  { id: "c2", name: "Trần Thị Bình", title: "Kế toán trưởng", email: "binh.tran@minhphat.vn", phone: "0912 445 566" },
  { id: "c3", name: "Lê Hoàng Cường", title: "Trưởng phòng mua hàng", email: "cuong.le@minhphat.vn", phone: "0987 778 899" },
];
const CONTACT_COLUMNS = [
  { key: "name", label: "Họ tên" },
  { key: "title", label: "Chức danh" },
  { key: "email", label: "Email" },
  { key: "phone", label: "Điện thoại" },
];
const NEW_BUTTON = <button className="slds-button slds-button_neutral">Mới</button>;

/** Blueprint button-icons: border-filled by default; `more` adds the small down arrow. */
function IconButton({ glyph: Glyph, label, more, border = "filled" }: { glyph: LucideIcon; label: string; more?: boolean; border?: "filled" | "plain" }) {
  return (
    <button
      className={`slds-button slds-button_icon ${more ? "slds-button_icon-more" : border === "filled" ? "slds-button_icon-border-filled" : "slds-button_icon-border"}`}
      title={label}
    >
      <Glyph aria-hidden="true" className="slds-button__icon nx-lucide" />
      {more && <ChevronDown aria-hidden="true" className="slds-button__icon slds-button__icon_x-small nx-lucide" />}
      <span className="slds-assistive-text">{label}</span>
    </button>
  );
}

function ButtonGroup({ children }: { children: ReactNode[] }) {
  return (
    <ul className="slds-button-group-list">
      {children.map((c, i) => (
        <li key={i}>{c}</li>
      ))}
    </ul>
  );
}

const RECORD_TABS: TabItem[] = [
  { value: "details", label: "Chi tiết", content: "Thông tin chi tiết của cơ hội." },
  { value: "related", label: "Liên quan", content: "Liên hệ, phiếu hỗ trợ và tệp đính kèm." },
  { value: "history", label: "Lịch sử", content: "Dòng thời gian hoạt động." },
];
const OBJECT_TABS: TabItem[] = [
  { value: "opp", label: "Cơ hội", icon: { glyph: Briefcase, name: "opportunity" }, content: "Danh sách cơ hội." },
  { value: "case", label: "Phiếu hỗ trợ", icon: { glyph: Ticket, name: "case" }, content: "Danh sách phiếu hỗ trợ." },
  { value: "contact", label: "Liên hệ", icon: { glyph: Contact, name: "contact" }, content: "Danh sách liên hệ." },
  { value: "account", label: "Khách hàng", icon: { glyph: Building2, name: "account" }, content: "Danh sách khách hàng." },
  { value: "approval", label: "Phê duyệt", icon: { glyph: CircleCheck, name: "approval" }, content: "Yêu cầu chờ duyệt." },
  { value: "task", label: "Công việc", icon: { glyph: ListTodo, name: "task" }, content: "Việc cần làm hôm nay." },
];

const STAGES: PathStage[] = [
  { value: "new", label: "Mới" },
  { value: "qualify", label: "Đánh giá nhu cầu" },
  { value: "proposal", label: "Đề xuất giải pháp" },
  { value: "negotiate", label: "Đàm phán" },
  { value: "closed", label: "Đã đóng" },
];
const COACHING: Record<string, { keyFields: { label: string; value: string }[]; guidance: ReactNode }> = {
  qualify: {
    keyFields: [
      { label: "Ngân sách dự kiến", value: "250.000.000 ₫" },
      { label: "Nguồn khách hàng", value: "Hội thảo và giới thiệu" },
      { label: "Kỹ sư hỗ trợ", value: "Lê Hoàng Cường" },
    ],
    guidance: (
      <>
        <p>Xác định người ra quyết định và quy trình mua hàng của khách.</p>
        <p>Ghi lại ngân sách và thời điểm triển khai mong muốn trước khi sang bước đề xuất.</p>
      </>
    ),
  },
};

function PathDemo() {
  const [current, setCurrent] = useState("proposal");
  return (
    <Path
      stages={STAGES}
      current={current}
      onAction={(action, stage) => {
        const i = STAGES.findIndex((s) => s.value === stage.value);
        setCurrent(action === "complete" ? STAGES[Math.min(i + 1, STAGES.length - 1)].value : stage.value);
      }}
    />
  );
}

function Longform({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="slds-text-longform">
      <h3 className="slds-text-heading_medium">{title}</h3>
      <p>{children}</p>
    </div>
  );
}

const MORE_ACTIONS = <IconButton glyph={ChevronDown} label="Thêm thao tác" />;

function Block({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <div id={id} className="slds-m-bottom_large">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

export function PageStructureSection() {
  return (
    <div>
      <Block id="ps-breadcrumbs" title="Breadcrumbs — breadcrumbs/base">
        <Breadcrumbs
          items={[
            { label: "Khách hàng", href: "#khach-hang" },
            { label: "Công ty TNHH Minh Phát", href: "#minh-phat" },
          ]}
        />
      </Block>

      <Block id="ps-header-base" title="PageHeader — page-headers/base">
        <PageHeader
          icon={{ glyph: Briefcase, name: "opportunity", label: "Cơ hội" }}
          title="Minh Phát - 80.000 thiết bị đầu cuối"
          meta="Nguyễn Văn An • Khách hàng VIP • 13/11/2025"
        />
      </Block>

      <Block id="ps-header-object-home" title="PageHeader — page-headers/object-home">
        <PageHeader
          variant="object-home"
          icon={{ glyph: Briefcase, name: "opportunity" }}
          label="Cơ hội"
          title="Xem gần đây"
          titleMenu={
            <div className="slds-dropdown-trigger slds-dropdown-trigger_click">
              <button className="slds-button slds-button_icon slds-button_icon-small" title="Đổi danh sách">
                <ChevronDown aria-hidden="true" className="slds-button__icon slds-icon_x-small nx-lucide" />
                <span className="slds-assistive-text">Đổi danh sách</span>
              </button>
            </div>
          }
          actions={[<ButtonGroup key="g">{[NEW_BUTTON, MORE_ACTIONS]}</ButtonGroup>]}
          meta="10 mục • Cập nhật 13 phút trước"
          controls={[
            <IconButton key="settings" glyph={Settings} label="Điều khiển danh sách" more />,
            <IconButton key="view" glyph={Table} label="Đổi kiểu xem" more />,
            <IconButton key="edit" glyph={Pencil} label="Sửa danh sách" />,
            <IconButton key="refresh" glyph={RefreshCw} label="Làm mới danh sách" />,
            <ButtonGroup key="g">{[<IconButton key="c" glyph={ChartColumn} label="Biểu đồ" />, <IconButton key="f" glyph={Filter} label="Bộ lọc" />]}</ButtonGroup>,
          ]}
        />
      </Block>

      <Block id="ps-header-record-home" title="PageHeader — page-headers/record-home">
        <PageHeader
          variant="record-home"
          icon={{ glyph: Briefcase, name: "opportunity" }}
          label="Cơ hội"
          title="Minh Phát - 1.200 thiết bị"
          actions={[
            <button key="follow" className="slds-button slds-button_neutral" aria-pressed="false">
              <Plus aria-hidden="true" className="slds-button__icon slds-button__icon_small slds-button__icon_left nx-lucide" />
              Theo dõi
            </button>,
            <ButtonGroup key="g">
              {[
                <button key="e" className="slds-button slds-button_neutral">Sửa</button>,
                <button key="d" className="slds-button slds-button_neutral">Xoá</button>,
                <button key="c" className="slds-button slds-button_neutral">Nhân bản</button>,
                MORE_ACTIONS,
              ]}
            </ButtonGroup>,
          ]}
          details={[
            { label: "Giai đoạn", value: "Đàm phán / Xem xét lại" },
            { label: "Giá trị", value: "1.250.000.000 ₫" },
            { label: "Website", value: <a href="#minhphat">minhphat.vn</a> },
            { label: "Người phụ trách", value: "Trần Thị Bình" },
          ]}
        />
      </Block>

      <Block id="ps-header-related-list" title="PageHeader — page-headers/related-list">
        <PageHeader
          variant="related-list"
          title="Liên hệ"
          breadcrumbs={[
            { label: "Khách hàng", href: "#khach-hang" },
            { label: "Công ty TNHH Minh Phát", href: "#minh-phat" },
          ]}
          actions={[<ButtonGroup key="g">{[<button key="a" className="slds-button slds-button_neutral">Thêm liên hệ</button>, MORE_ACTIONS]}</ButtonGroup>]}
          meta="3 mục • sắp theo họ tên"
          controls={[
            <IconButton key="view" glyph={Table} label="Đổi kiểu xem" more />,
            <ButtonGroup key="g">
              {[
                <IconButton key="c" glyph={ChartColumn} label="Biểu đồ" border="plain" />,
                <IconButton key="f" glyph={Filter} label="Lọc danh sách" border="plain" />,
              ]}
            </ButtonGroup>,
          ]}
        />
      </Block>

      <Block id="ps-tabs" title="Tabs — tabs/base (default, error)">
        <Tabs
          label="Hồ sơ cơ hội"
          items={[RECORD_TABS[0], { ...RECORD_TABS[1], error: "Tab này có lỗi cần xử lý" }, RECORD_TABS[2]]}
        />
      </Block>

      <Block id="ps-tabs-overflow" title="Tabs — tabs/base (overflow-icons, maxVisible 4)">
        <Tabs label="Đối tượng" items={OBJECT_TABS} maxVisible={4} />
      </Block>

      <Block id="ps-tabs-scoped" title="Tabs — scoped-tabs/base">
        <Tabs variant="scoped" label="Hồ sơ cơ hội" items={RECORD_TABS} />
      </Block>

      <Block id="ps-vertical-tabs" title="VerticalTabs — vertical-tabs/base (with-icons, with-badges)">
        <VerticalTabs
          label="Thiết lập khách hàng"
          items={[
            {
              value: "general",
              label: "Thông tin chung",
              icon: { glyph: Building2, name: "account" },
              content: <Longform title="Thông tin chung">Tên, mã số thuế, địa chỉ trụ sở và ngành nghề của khách hàng.</Longform>,
            },
            {
              value: "contacts",
              label: "Liên hệ",
              icon: { glyph: Contact, name: "contact" },
              badge: "Mới",
              content: <Longform title="Liên hệ">Những người làm việc với Nexus tại khách hàng này.</Longform>,
            },
            {
              value: "billing",
              label: "Thanh toán và hoá đơn điện tử theo từng chi nhánh",
              icon: { glyph: ListTodo, name: "task" },
              content: <Longform title="Thanh toán">Phương thức thanh toán, kỳ hạn và thông tin xuất hoá đơn điện tử.</Longform>,
            },
          ]}
        />
      </Block>

      <Block id="ps-path" title="Path — path/base (default; bấm giai đoạn khác để thấy 'Đánh dấu là giai đoạn hiện tại')">
        <PathDemo />
      </Block>

      <Block id="ps-path-coaching" title="Path — path/base (with-coaching, mở sẵn)">
        <Path
          stages={STAGES}
          current="qualify"
          coaching={(s) => COACHING[s.value] ?? { keyFields: [], guidance: <p>Chưa có hướng dẫn cho giai đoạn này.</p> }}
          onEditKeyFields={() => {}}
          onAction={() => {}}
          defaultCoachingOpen
        />
      </Block>

      <Block id="ps-path-closed" title="Path — path/base (lost, won)">
        <div className="slds-m-bottom_small">
          <Path stages={[...STAGES.slice(0, 4), { value: "lost", label: "Đã đóng - Thất bại" }]} current="lost" closed="lost" onAction={() => {}} />
        </div>
        <Path stages={[...STAGES.slice(0, 4), { value: "won", label: "Đã đóng - Thành công" }]} current="won" closed="won" onAction={() => {}} />
      </Block>

      <Block id="ps-card" title="Card — cards/default">
        <Card title="Khách hàng" href="#khach-hang" icon={{ glyph: Building2, name: "account" }} actions={NEW_BUTTON} viewAllHref="#khach-hang" boundary>
          Công ty TNHH Minh Phát · Hà Nội · khách hàng từ 2021
        </Card>
      </Block>

      <Block id="ps-related-list" title="RelatedListCard — cards/related-list-table">
        <RelatedListCard
          title="Liên hệ"
          href="#lien-he"
          icon={{ glyph: Contact, name: "contact" }}
          actions={NEW_BUTTON}
          columns={CONTACT_COLUMNS}
          rows={CONTACTS}
          rowKey={(r) => r.id}
          rowHref={(r) => `#${r.id}`}
          viewAllHref="#lien-he"
          boundary
        />
      </Block>

      <Block id="ps-card-states" title="Card — cards/empty, cards/loading">
        <RelatedListCard
          title="Liên hệ"
          icon={{ glyph: Contact, name: "contact" }}
          actions={NEW_BUTTON}
          columns={CONTACT_COLUMNS}
          rows={[] as typeof CONTACTS}
          rowKey={(r) => r.id}
          boundary
          className="slds-m-bottom_small"
        />
        <Card title="Công việc" icon={{ glyph: ListTodo, name: "task" }} loading boundary />
      </Block>
    </div>
  );
}
