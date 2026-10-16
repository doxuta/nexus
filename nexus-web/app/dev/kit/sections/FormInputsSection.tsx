"use client";

import { Ban, CalendarDays, Columns3, Info, Mail, Search, Table } from "lucide-react";
import type { ReactNode } from "react";
import { Checkbox, CheckboxGroup } from "@/components/slds/Checkbox";
import { FormElement } from "@/components/slds/FormElement";
import { Input } from "@/components/slds/Input";
import { RadioButtonGroup, RadioGroup } from "@/components/slds/RadioGroup";
import { Select } from "@/components/slds/Select";
import { Textarea } from "@/components/slds/Textarea";
import { Toggle } from "@/components/slds/Toggle";

// Blueprints: form-element (base/stacked/horizontal/record-detail), input (SLDS 2.264.1).
// Static sample data; each block names the blueprint variant it copies.

function Block({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="slds-col slds-size_1-of-1 slds-medium-size_1-of-3 slds-m-bottom_medium">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

// Blueprint "help-text-icon": the icon button that a Tooltip wraps.
const helpButton = (
  <button type="button" className="slds-button slds-button_icon">
    <Info className="slds-button__icon nx-lucide" aria-hidden="true" />
    <span className="slds-assistive-text">Trợ giúp</span>
  </button>
);

const noop = () => {};

const STAGES = [
  { value: "new", label: "Mới" },
  { value: "qualified", label: "Đã đánh giá" },
  { value: "won", label: "Thắng" },
  { value: "lost", label: "Thua" },
];
const DAYS = [
  { value: "mon", label: "Thứ Hai" },
  { value: "wed", label: "Thứ Tư" },
  { value: "fri", label: "Thứ Sáu" },
  { value: "sun", label: "Chủ Nhật", disabled: true },
];
const PRIORITY = [
  { value: "low", label: "Thấp" },
  { value: "mid", label: "Trung bình" },
  { value: "high", label: "Cao" },
];
const CONTACT = [
  { value: "call", label: "Gọi điện" },
  { value: "mail", label: "Email" },
  { value: "zalo", label: "Zalo" },
];
const WEEKDAYS = [
  { value: "mon", label: "T2" },
  { value: "tue", label: "T3" },
  { value: "wed", label: "T4" },
  { value: "thu", label: "T5" },
  { value: "fri", label: "T6" },
];
const VIEWS = [
  { value: "table", label: "Bảng", icon: Table },
  { value: "kanban", label: "Kanban", icon: Columns3 },
  { value: "calendar", label: "Lịch", icon: CalendarDays },
];
const SOURCES = [
  { value: "web", label: "Website" },
  { value: "phone", label: "Điện thoại" },
  { value: "event", label: "Sự kiện" },
];

export function FormInputsSection() {
  return (
    <div>
      <h3 className="slds-text-heading_small slds-m-bottom_small">Form element</h3>
      <div className="slds-grid slds-wrap slds-gutters">
        <Block title="base · required · tooltip help">
          <Input label="Tên khách hàng" required fieldLevelHelp={helpButton} />
        </Block>
        <Block title="base · inline help">
          <Input label="Điện thoại" type="tel" help="VD: 0912 345 678" />
        </Block>
        <Block title="base · error">
          <Input label="Email" required error="Nhập giá trị." />
        </Block>
      </div>
      <div className="slds-grid slds-wrap slds-gutters">
        <div className="slds-col slds-size_1-of-1 slds-medium-size_1-of-2 slds-m-bottom_medium">
          <p className="slds-text-title_caps slds-m-bottom_x-small">record-detail · stacked (xem)</p>
          <div className="slds-form" role="list">
            <div className="slds-form__row">
              <div className="slds-form__item" role="listitem">
                <FormElement label="Chủ sở hữu" layout="stacked" required readOnly onEdit={noop}>
                  <a href="#">Nguyễn Minh Anh</a>
                </FormElement>
              </div>
              <div className="slds-form__item" role="listitem">
                <FormElement label="Doanh thu năm" layout="stacked" readOnly onEdit={noop}>
                  <span className="tabular-nums">12.500.000.000 ₫</span>
                </FormElement>
              </div>
            </div>
          </div>
        </div>
        <div className="slds-col slds-size_1-of-1 slds-medium-size_1-of-2 slds-m-bottom_medium">
          <p className="slds-text-title_caps slds-m-bottom_x-small">record-detail · horizontal (sửa)</p>
          <div className="slds-form" role="list">
            <div className="slds-form__row">
              <div className="slds-form__item" role="listitem">
                <Input label="Tên công ty" layout="horizontal" editing defaultValue="Công ty Cổ phần Sao Mai" />
              </div>
            </div>
            <div className="slds-form__row">
              <div className="slds-form__item" role="listitem">
                <Input label="Mã số thuế" layout="horizontal" editing required error="Nhập giá trị." />
              </div>
            </div>
          </div>
        </div>
      </div>

      <h3 className="slds-text-heading_small slds-m-bottom_small">Input</h3>
      <div className="slds-grid slds-wrap slds-gutters">
        <Block title="left icon">
          <Input label="Tìm kiếm" iconLeft={Search} placeholder="Tìm khách hàng…" />
        </Block>
        <Block title="right icon">
          <Input label="Email" type="email" iconRight={Mail} placeholder="ten@congty.vn" />
        </Block>
        <Block title="left icon + clear">
          <Input label="Tỉnh / thành" iconLeft={Search} clearable defaultValue="Đà Nẵng" />
        </Block>
        <Block title="clear + spinner">
          <Input label="Tìm kiếm" iconLeft={Search} clearable loading defaultValue="Nguyễn" />
        </Block>
        <Block title="fixed text">
          <Input label="Đơn giá" fixedTextLeft="₫" fixedTextRight="/tháng" numeric defaultValue="1.200.000" />
        </Block>
        <Block title="number">
          <Input label="Số lượng" type="number" defaultValue={42} />
        </Block>
        <Block title="error with icon">
          <Input label="Mã số thuế" required iconLeft={Ban} error="Nhập giá trị." />
        </Block>
        <Block title="disabled">
          <Input label="Mã khách hàng" disabled defaultValue="KH-00128" />
        </Block>
        <Block title="readonly">
          <Input label="Ngày tạo" readOnly defaultValue="28/09/2026" />
        </Block>
        <Block title="static">
          <FormElement label="Người tạo" readOnly="static">
            Trần Thu Hà
          </FormElement>
        </Block>
      </div>

      <h3 className="slds-text-heading_small slds-m-bottom_small">Textarea</h3>
      <div className="slds-grid slds-wrap slds-gutters">
        <Block title="default">
          <Textarea label="Ghi chú" placeholder="Nhập ghi chú…" />
        </Block>
        <Block title="error">
          <Textarea label="Lý do từ chối" required error="Nhập giá trị." />
        </Block>
        <Block title="read only">
          <Textarea
            label="Mô tả"
            staticValue={"Khách hàng thân thiết từ 2019, ưu tiên liên hệ qua email trước 10 giờ sáng.\nHợp đồng gia hạn hằng năm vào tháng Ba."}
          />
        </Block>
      </div>

      <h3 className="slds-text-heading_small slds-m-bottom_small">Select</h3>
      <div className="slds-grid slds-wrap slds-gutters">
        <Block title="default · required">
          <Select label="Giai đoạn" required options={STAGES} />
        </Block>
        <Block title="error">
          <Select label="Nguồn khách" required options={SOURCES} error="Chọn một giá trị." />
        </Block>
        <Block title="disabled">
          <Select label="Giai đoạn" disabled options={STAGES} defaultValue="won" />
        </Block>
        <Block title="multiple (narrow)">
          <Select label="Kênh liên hệ" multiple options={SOURCES} />
        </Block>
      </div>

      <h3 className="slds-text-heading_small slds-m-bottom_small">Checkbox</h3>
      <div className="slds-grid slds-wrap slds-gutters">
        <Block title="default · checked · indeterminate">
          <Checkbox label="Gửi email chào mừng" />
          <Checkbox label="Khách VIP" defaultChecked />
          <Checkbox label="Chọn tất cả liên hệ" indeterminate />
        </Block>
        <Block title="required · error">
          <Checkbox label="Đồng ý điều khoản sử dụng" required error="Bắt buộc phải đồng ý." />
        </Block>
        <Block title="disabled · checked disabled">
          <Checkbox label="Đã xác minh" disabled />
          <Checkbox label="Đã kích hoạt" disabled defaultChecked />
        </Block>
        <Block title="group · required">
          <CheckboxGroup label="Ngày học trong tuần" required options={DAYS} defaultValue={["mon", "wed"]} />
        </Block>
        <Block title="group · error">
          <CheckboxGroup label="Ngày học trong tuần" required options={DAYS} error="Chọn ít nhất một ngày." />
        </Block>
        <Block title="form-element (standalone)">
          <Checkbox label="Cần đánh giá bảo mật?" variant="standalone" required fieldLevelHelp={helpButton} />
        </Block>
      </div>

      <h3 className="slds-text-heading_small slds-m-bottom_small">Toggle</h3>
      <div className="slds-grid slds-wrap slds-gutters">
        <Block title="off · on">
          <Toggle label="Nhận thông báo qua email" />
          <Toggle label="Tự động gán phụ trách" defaultChecked />
        </Block>
        <Block title="disabled · checked disabled">
          <Toggle label="Đồng bộ lịch" disabled />
          <Toggle label="Nhật ký thay đổi" disabled defaultChecked />
        </Block>
        <Block title="custom text">
          <Toggle label="Trạng thái" onText="Hoạt động" offText="Ngừng" defaultChecked />
        </Block>
      </div>

      <h3 className="slds-text-heading_small slds-m-bottom_small">Radio group</h3>
      <div className="slds-grid slds-wrap slds-gutters">
        <Block title="default">
          <RadioGroup label="Mức ưu tiên" options={PRIORITY} defaultValue="mid" />
        </Block>
        <Block title="required · error">
          <RadioGroup label="Hình thức liên hệ" required options={CONTACT} error="Chọn một hình thức." />
        </Block>
        <Block title="disabled">
          <RadioGroup label="Mức ưu tiên" disabled options={PRIORITY} defaultValue="high" />
        </Block>
      </div>

      <h3 className="slds-text-heading_small slds-m-bottom_small">Radio button group</h3>
      <div className="slds-grid slds-wrap slds-gutters">
        <Block title="default">
          <RadioButtonGroup label="Ngày làm việc" options={WEEKDAYS} defaultValue="tue" />
        </Block>
        <Block title="with icons">
          <RadioButtonGroup label="Chế độ xem" options={VIEWS} defaultValue="table" />
        </Block>
        <Block title="required · error">
          <RadioButtonGroup label="Ngày làm việc" required options={WEEKDAYS} error="Chọn một ngày." />
        </Block>
        <Block title="disabled">
          <RadioButtonGroup label="Ngày làm việc" disabled options={WEEKDAYS} />
        </Block>
      </div>
    </div>
  );
}
