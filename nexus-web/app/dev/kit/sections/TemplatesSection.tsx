// Page templates (specs/038 T018–T020, contracts/shell.md "Template trang"): ObjectHomePage,
// RecordHomePage, RecordFormPage (page + quick-create modal) on STATIC crm_accounts sample data.
// Local state only, so the controls can be tried; nothing is fetched.
// Kit artefact: SLDS flattens a card nested in a card (`.slds-card .slds-card` loses border and
// shadow) and this section already sits in the kit's card, so the templates' own cards (list card,
// path, form) render flat here and look right only as page roots. The aside cards pass `boundary`
// to keep their border, like the page-structure section does.
// Stand-ins until the owning groups merge — raw blueprint markup, swap then:
//   TODO(T011): the list body table → components/slds/DataTable
//   TODO(T015): the filter panel content → components/slds/FilterPanel
//   TODO(T014): the empty state → components/slds/EmptyState
//   TODO(T009): the form fields → components/slds/{FormElement,Input}
import { useId, useState, type ReactNode } from "react";
import { Building2, ChevronDown, Contact, Inbox, Plus, RefreshCw } from "lucide-react";
import { Card, RelatedListCard } from "@/components/slds/Card";
import { Icon } from "@/components/slds/Icon";
import { Panel } from "@/components/slds/Panel";
import type { PathStage } from "@/components/slds/Path";
import { ObjectHomePage, type ListDensity, type ListViewMode } from "@/components/slds/templates/ObjectHomePage";
import { RecordFormPage, type RecordFormNotificationTone, type RecordFormSection } from "@/components/slds/templates/RecordFormPage";
import { RecordHomePage } from "@/components/slds/templates/RecordHomePage";

const ACCOUNT_ICON = { glyph: Building2, name: "account" };
const noop = () => {};

const ACCOUNTS = [
  { id: "a1", name: "Công ty TNHH Minh Phát", industry: "Bán lẻ", type: "Khách hàng", phone: "024 3825 1122", city: "Hà Nội", owner: "Trần Thị Bình" },
  { id: "a2", name: "Công ty CP Thực phẩm Sao Việt", industry: "Thực phẩm", type: "Đối tác", phone: "028 3930 4455", city: "TP. Hồ Chí Minh", owner: "Lê Hoàng Cường" },
  { id: "a3", name: "Tập đoàn Giáo dục Ánh Dương", industry: "Giáo dục", type: "Khách hàng", phone: "0236 3811 668", city: "Đà Nẵng", owner: "Nguyễn Văn An" },
  { id: "a4", name: "Công ty TNHH Cơ khí Hòa Bình", industry: "Sản xuất", type: "Tiềm năng", phone: "0225 3746 990", city: "Hải Phòng", owner: "Trần Thị Bình" },
  { id: "a5", name: "Bệnh viện Đa khoa Tâm An", industry: "Y tế", type: "Khách hàng", phone: "0292 3812 345", city: "Cần Thơ", owner: "Phạm Thu Hà" },
];
type Account = (typeof ACCOUNTS)[number];
const COLUMNS: { key: keyof Account; label: string }[] = [
  { key: "name", label: "Tên khách hàng" },
  { key: "industry", label: "Ngành" },
  { key: "type", label: "Loại" },
  { key: "phone", label: "Điện thoại" },
  { key: "city", label: "Tỉnh/thành" },
  { key: "owner", label: "Người phụ trách" },
];
const CONTACTS = [
  { id: "c1", name: "Nguyễn Văn An", title: "Giám đốc điều hành", phone: "0903 112 233" },
  { id: "c2", name: "Trần Thị Bình", title: "Kế toán trưởng", phone: "0912 445 566" },
];
const CONTACT_COLUMNS = [
  { key: "name", label: "Họ tên" },
  { key: "title", label: "Chức danh" },
  { key: "phone", label: "Điện thoại" },
];
const STAGES: PathStage[] = [
  { value: "lead", label: "Tiềm năng" },
  { value: "care", label: "Đang chăm sóc" },
  { value: "signed", label: "Đã ký hợp đồng" },
  { value: "loyal", label: "Khách hàng thân thiết" },
];
const LIST_ACTIONS = [
  { label: "Mới", onSelect: noop },
  { label: "Tạo nhiều", onSelect: noop },
  { label: "Nhập", onSelect: noop },
  { label: "Thùng rác", onSelect: noop },
];

function Block({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <div id={id} className="slds-m-bottom_large">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

const SWITCH_LIST = (
  <div className="slds-dropdown-trigger slds-dropdown-trigger_click">
    <button type="button" className="slds-button slds-button_icon slds-button_icon-small" title="Đổi danh sách">
      <ChevronDown aria-hidden="true" className="slds-button__icon slds-icon_x-small nx-lucide" />
      <span className="slds-assistive-text">Đổi danh sách</span>
    </button>
  </div>
);
const REFRESH = (
  <button key="refresh" type="button" className="slds-button slds-button_icon slds-button_icon-border-filled" title="Làm mới danh sách">
    <RefreshCw aria-hidden="true" className="slds-button__icon nx-lucide" />
    <span className="slds-assistive-text">Làm mới danh sách</span>
  </button>
);

/** data-tables/base (TODO(T011)). */
function AccountsTable({ rows }: { rows: Account[] }) {
  return (
    <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label="Danh sách khách hàng">
      <thead>
        <tr className="slds-line-height_reset">
          {COLUMNS.map((c) => (
            <th key={c.key} scope="col">
              <div className="slds-truncate" title={c.label}>
                {c.label}
              </div>
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {rows.map((r) => (
          <tr key={r.id} className="slds-hint-parent">
            {COLUMNS.map((c, i) =>
              i === 0 ? (
                <th key={c.key} scope="row" data-label={c.label}>
                  <div className="slds-truncate" title={r[c.key]}>
                    <a href={`#${r.id}`}>{r[c.key]}</a>
                  </div>
                </th>
              ) : (
                <td key={c.key} data-label={c.label}>
                  <div className="slds-truncate" title={r[c.key]}>
                    {r[c.key]}
                  </div>
                </td>
              ),
            )}
          </tr>
        ))}
      </tbody>
    </table>
  );
}

/** illustration/base frame + text; the picture is a Lucide glyph, never the CC BY-ND art (TODO(T014)). */
function Empty({ title, body }: { title: string; body: string }) {
  return (
    <div className="slds-illustration slds-illustration_small slds-p-around_large">
      <div className="slds-m-bottom_small">
        <Icon icon={Inbox} size="large" />
      </div>
      <div className="slds-text-longform">
        <h3 className="slds-text-heading_medium">{title}</h3>
        <p className="slds-text-body_regular">{body}</p>
      </div>
    </div>
  );
}

/** panels/filtering content (TODO(T015)). */
function FilterList() {
  const items = [
    { field: "Ngành", value: "bằng Bán lẻ, Giáo dục" },
    { field: "Tỉnh/thành", value: "bằng Hà Nội" },
  ];
  return (
    <div className="slds-filters">
      <h3 className="slds-text-body_small slds-m-vertical_x-small">Khớp tất cả điều kiện</h3>
      <ol className="slds-list_vertical slds-list_vertical-space">
        {items.map((f) => (
          <li key={f.field} className="slds-item slds-hint-parent">
            <div className="slds-filters__item slds-grid slds-grid_vertical-align-center">
              <button type="button" className="slds-button_reset slds-grow slds-has-blur-focus">
                <span className="slds-assistive-text">Sửa bộ lọc:</span>
                <span className="slds-show slds-text-body_small">{f.field}</span>
                <span className="slds-show">{f.value}</span>
              </button>
            </div>
          </li>
        ))}
      </ol>
    </div>
  );
}

function ObjectHomeDemo() {
  const [q, setQ] = useState("");
  const [view, setView] = useState<ListViewMode>("table");
  const [density, setDensity] = useState<ListDensity>("normal");
  const [filtersOpen, setFiltersOpen] = useState(false);
  const panelId = useId();
  const rows = ACCOUNTS.filter((a) => a.name.toLowerCase().includes(q.trim().toLowerCase()));
  return (
    <ObjectHomePage
      icon={ACCOUNT_ICON}
      label="Khách hàng"
      title="Tất cả khách hàng"
      titleMenu={SWITCH_LIST}
      actions={LIST_ACTIONS}
      maxVisibleActions={2}
      meta={`${rows.length} mục · sắp theo tên khách hàng · cập nhật 3 phút trước`}
      search={{ value: q, onChange: setQ }}
      density={{ value: density, onChange: setDensity }}
      view={{ value: view, onChange: setView }}
      controls={[REFRESH]}
      filter={{ open: filtersOpen, onToggle: () => setFiltersOpen(!filtersOpen), panelId }}
      filterPanel={
        <Panel id={panelId} open={filtersOpen} title="Bộ lọc" position="right" size="small" onClose={() => setFiltersOpen(false)}>
          <FilterList />
        </Panel>
      }
      empty={rows.length === 0 ? <Empty title="Không có khách hàng khớp" body="Thử từ khoá khác hoặc bỏ bớt bộ lọc." /> : undefined}
    >
      {view === "table" ? (
        <AccountsTable rows={rows} />
      ) : (
        <p className="slds-p-around_medium slds-text-color_weak">Dạng {view === "kanban" ? "Kanban" : "Lịch"} gắn ở lượt di trú.</p>
      )}
    </ObjectHomePage>
  );
}

function DetailsTab() {
  const fields = [
    ["Tên khách hàng", "Công ty TNHH Minh Phát"],
    ["Ngành", "Bán lẻ"],
    ["Mã số thuế", "0101234567"],
    ["Số nhân viên", "320"],
    ["Điện thoại", "024 3825 1122"],
    ["Website", "minhphat.vn"],
  ];
  const rows = [fields.slice(0, 2), fields.slice(2, 4), fields.slice(4, 6)];
  return (
    <div className="slds-section slds-is-open">
      <h3 className="slds-section__title slds-theme_shade">
        <span className="slds-truncate slds-p-horizontal_small" title="Thông tin chung">
          Thông tin chung
        </span>
      </h3>
      <div className="slds-section__content">
        <div className="slds-form" role="list">
          {rows.map((row) => (
            <div key={row[0][0]} className="slds-form__row">
              {row.map(([label, value]) => (
                <div key={label} className="slds-form__item" role="listitem">
                  <div className="slds-form-element slds-form-element_readonly slds-form-element_stacked slds-hint-parent">
                    <span className="slds-form-element__label">{label}</span>
                    <div className="slds-form-element__control">
                      <div className="slds-form-element__static">{value}</div>
                    </div>
                  </div>
                </div>
              ))}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

function RecordHomeDemo({ withPath }: { withPath: boolean }) {
  const [stage, setStage] = useState("care");
  return (
    <RecordHomePage
      icon={ACCOUNT_ICON}
      objectLabel="Khách hàng"
      title="Công ty TNHH Minh Phát"
      actions={[
        { label: "Sửa", onSelect: noop },
        { label: "Nhân bản", onSelect: noop },
        { label: "Xoá", onSelect: noop },
        { label: "Đổi người phụ trách", onSelect: noop },
        { label: "In", onSelect: noop },
      ]}
      details={[
        { label: "Ngành", value: "Bán lẻ" },
        { label: "Điện thoại", value: "024 3825 1122" },
        { label: "Website", value: <a href="#minhphat">minhphat.vn</a> },
        { label: "Người phụ trách", value: "Trần Thị Bình" },
      ]}
      path={
        withPath
          ? {
              stages: STAGES,
              current: stage,
              onAction: (action, s) => {
                const i = STAGES.findIndex((x) => x.value === s.value);
                setStage(action === "complete" ? STAGES[Math.min(i + 1, STAGES.length - 1)].value : s.value);
              },
            }
          : undefined
      }
      tabsLabel="Hồ sơ khách hàng"
      tabs={[
        { value: "details", label: "Chi tiết", content: <DetailsTab /> },
        {
          value: "related",
          label: "Liên quan",
          content: (
            <RelatedListCard
              title="Liên hệ"
              icon={{ glyph: Contact, name: "contact" }}
              columns={CONTACT_COLUMNS}
              rows={CONTACTS}
              rowKey={(r) => r.id}
              rowHref={(r) => `#${r.id}`}
            />
          ),
        },
        { value: "history", label: "Lịch sử", content: <p>Trần Thị Bình đổi Giai đoạn từ Tiềm năng sang Đang chăm sóc · 12/09/2026.</p> },
      ]}
      aside={
        <>
          <RelatedListCard
            title="Liên hệ"
            icon={{ glyph: Contact, name: "contact" }}
            actions={
              <button type="button" className="slds-button slds-button_neutral">
                Mới
              </button>
            }
            columns={CONTACT_COLUMNS.slice(0, 2)}
            rows={CONTACTS}
            rowKey={(r) => r.id}
            rowHref={(r) => `#${r.id}`}
            boundary
          />
          <Card title="Ghi chú" boundary>
            Khách muốn gia hạn hợp đồng bảo trì trước 30/10.
          </Card>
        </>
      }
    />
  );
}

/** form-element/record-detail "edit-stacked" (TODO(T009)). */
function Field({ label, required, error, defaultValue }: { label: string; required?: boolean; error?: string; defaultValue?: string }) {
  const id = useId();
  return (
    <div className="slds-form__item" role="listitem">
      <div className={`slds-form-element slds-form-element_stacked slds-is-editing${error ? " slds-has-error" : ""}`}>
        <label className="slds-form-element__label" htmlFor={id}>
          {required && (
            <abbr className="slds-required" title="bắt buộc">
              *{" "}
            </abbr>
          )}
          {label}
        </label>
        <div className="slds-form-element__control">
          <input
            id={id}
            className="slds-input"
            defaultValue={defaultValue}
            aria-invalid={error ? true : undefined}
            aria-describedby={error ? `${id}-error` : undefined}
          />
        </div>
        {error && (
          <div className="slds-form-element__help" id={`${id}-error`}>
            {error}
          </div>
        )}
      </div>
    </div>
  );
}

function formSections(taxError?: string): RecordFormSection[] {
  return [
    {
      id: "info",
      title: "Thông tin chung",
      body: (
        <div className="slds-form" role="list">
          <div className="slds-form__row">
            <Field label="Tên khách hàng" required defaultValue="Công ty TNHH Minh Phát" />
            <Field label="Ngành" defaultValue="Bán lẻ" />
          </div>
          <div className="slds-form__row">
            <Field label="Mã số thuế" required defaultValue="01012345" error={taxError} />
            <Field label="Điện thoại" defaultValue="024 3825 1122" />
          </div>
        </div>
      ),
      hasError: Boolean(taxError),
    },
    {
      id: "address",
      title: "Địa chỉ",
      collapsed: true,
      body: (
        <div className="slds-form" role="list">
          <div className="slds-form__row">
            <Field label="Địa chỉ" defaultValue="12 Tràng Tiền" />
            <Field label="Tỉnh/thành" defaultValue="Hà Nội" />
          </div>
        </div>
      ),
    },
  ];
}

function FormPageDemo() {
  const [saving, setSaving] = useState(false);
  const [notification, setNotification] = useState<{ tone: RecordFormNotificationTone; body: ReactNode } | undefined>({
    tone: "warning",
    body: "Bản ghi đã được người khác sửa. Tải lại để xem thay đổi mới nhất rồi nhập lại.",
  });
  return (
    // Kit only: the transform makes this box the containing block of the position:fixed docked
    // footer, so the footer docks at the bottom of the demo instead of over the whole kit page.
    <div style={{ transform: "translateZ(0)" }}>
      <RecordFormPage
        icon={ACCOUNT_ICON}
        objectLabel="Khách hàng"
        title="Sửa: Công ty TNHH Minh Phát"
        meta="Các trường có * là bắt buộc"
        sections={formSections("Mã số thuế phải có 10 hoặc 13 chữ số.")}
        notification={notification}
        saving={saving}
        onSubmit={(e) => {
          e.preventDefault();
          setSaving(true);
          setNotification(undefined);
          setTimeout(() => setSaving(false), 1500);
        }}
        onCancel={noop}
        extraActions={
          <button type="button" className="slds-button slds-button_neutral" disabled={saving}>
            Lưu &amp; tạo mới
          </button>
        }
      />
    </div>
  );
}

function FormModalDemo() {
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  return (
    <RecordFormPage
      variant="modal"
      open={open}
      onOpenChange={setOpen}
      trigger={
        <button type="button" className="slds-button slds-button_neutral">
          <Plus aria-hidden="true" className="slds-button__icon slds-button__icon_left nx-lucide" />
          Tạo nhanh khách hàng
        </button>
      }
      title="Khách hàng mới"
      meta="Các trường có * là bắt buộc"
      sections={formSections()}
      saving={saving}
      onSubmit={(e) => {
        e.preventDefault();
        setSaving(true);
        setTimeout(() => {
          setSaving(false);
          setOpen(false);
        }, 1000);
      }}
      // Huỷ, Esc and the X all come through onOpenChange(false); nothing else to undo here.
      onCancel={noop}
    />
  );
}

export function TemplatesSection() {
  return (
    <div>
      <Block id="tpl-object-home" title="ObjectHomePage — page-headers/object-home + data-tables/base + panels/filtering (gõ 'zzz' để thấy illustration)">
        <ObjectHomeDemo />
      </Block>
      <Block id="tpl-record-home" title="RecordHomePage — page-headers/record-home + path + tabs, cột phải 1/3">
        <RecordHomeDemo withPath />
      </Block>
      <Block id="tpl-record-home-no-path" title="RecordHomePage — không có trường giai đoạn (ẩn path)">
        <RecordHomeDemo withPath={false} />
      </Block>
      <Block id="tpl-form-page" title="RecordFormPage — expandable-section + scoped-notifications + docked-form-footer (bấm Lưu để thấy trạng thái đang lưu)">
        <FormPageDemo />
      </Block>
      <Block id="tpl-form-modal" title="RecordFormPage variant='modal' — modals/base (tạo nhanh)">
        <FormModalDemo />
      </Block>
    </div>
  );
}
