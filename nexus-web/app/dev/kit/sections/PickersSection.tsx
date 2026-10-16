"use client";

import { Building2 } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Combobox, matches, type ComboboxOption } from "@/components/slds/Combobox";
import { DatePicker, DateRangePicker } from "@/components/slds/DatePicker";
import { DateTimePicker } from "@/components/slds/DateTimePicker";
import { Lookup } from "@/components/slds/Lookup";
import { TimePicker } from "@/components/slds/TimePicker";
import { Picklist } from "@/components/slds/Picklist";
import { AppFilter } from "@/components/studio/AppFilter";

// Blueprints: combobox (base / autocomplete / grouped options), picklist, lookups, timepicker, datepickers (base / range),
// datetime-picker (SLDS 2.264.1).
// Static sample data; each block names the blueprint variant it copies. The ids on each block
// (kit-combobox-…) let the screenshot script open and shoot one picker at a time.

function Block({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <div id={id} className="slds-col slds-size_1-of-1 slds-medium-size_1-of-3 slds-m-bottom_medium">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

const OBJECTS: ComboboxOption[] = [
  { value: "account", label: "Khách hàng" },
  { value: "report", label: "Báo cáo" },
  { value: "contact", label: "Liên hệ" },
  { value: "file", label: "Tệp" },
  { value: "group", label: "Nhóm" },
  { value: "lead", label: "Khách tiềm năng" },
  { value: "note", label: "Ghi chú" },
];

const GROUPED: ComboboxOption[] = [
  { value: "acme", label: "Công ty Cổ phần Acme Việt Nam", group: "Gần đây" },
  { value: "dieu", label: "Cánh Diều Education", group: "Gần đây" },
  { value: "hoa", label: "Hoa Sen Group", group: "Tất cả" },
  { value: "minh", label: "Minh Phát Logistics", group: "Tất cả" },
];

const STAGES: ComboboxOption[] = [
  { value: "new", label: "Mới", group: "Đang mở" },
  { value: "work", label: "Đang xử lý", group: "Đang mở" },
  { value: "won", label: "Thắng", group: "Đã đóng" },
  { value: "lost", label: "Thua", group: "Đã đóng" },
];

const ACCOUNT = { glyph: Building2, name: "account", label: "Khách hàng" };
const ACCOUNTS: ComboboxOption[] = [
  { value: "a1", label: "Công ty Cổ phần Acme Việt Nam", meta: "Khách hàng • Hà Nội", icon: ACCOUNT },
  { value: "a2", label: "Acme Logistics", meta: "Khách hàng • Đà Nẵng", icon: ACCOUNT },
  { value: "a3", label: "Cánh Diều Education", meta: "Khách hàng • TP. Hồ Chí Minh", icon: ACCOUNT },
  { value: "a4", label: "Hoa Sen Group", meta: "Khách hàng • Bình Dương", icon: ACCOUNT },
];
// Stands in for the record-search API: filters the sample list after a short wait.
const searchAccounts = (term: string) =>
  new Promise<ComboboxOption[]>((resolve) => setTimeout(() => resolve(ACCOUNTS.filter((a) => matches(a.label, term))), 400));

function LookupMulti() {
  const [value, setValue] = useState<ComboboxOption[]>([ACCOUNTS[0], ACCOUNTS[2]]);
  return <Lookup label="Khách hàng liên quan" search={searchAccounts} multiple value={value} onChange={setValue} />;
}

function PicklistMulti() {
  const [value, setValue] = useState<string[]>(["new", "won"]);
  return <Picklist label="Giai đoạn" options={STAGES} multiple value={value} onChange={setValue} />;
}

function SelectSingle() {
  const [value, setValue] = useState<string | null>("account");
  return <Combobox label="Liên quan tới" options={OBJECTS} value={value} onChange={setValue} />;
}

function SelectMulti() {
  const [value, setValue] = useState<string[]>(["account", "contact"]);
  return <Combobox label="Đối tượng" options={OBJECTS} multiple value={value} onChange={setValue} />;
}

function AutocompleteMulti() {
  const [value, setValue] = useState<string[]>(["report", "note"]);
  return <Combobox label="Liên quan tới" variant="autocomplete" options={OBJECTS} multiple value={value} onChange={setValue} />;
}

export function PickersSection() {
  return (
    <div className="slds-grid slds-wrap slds-gutters">
      <Block id="kit-combobox-base" title="combobox/base — chọn một">
        <SelectSingle />
      </Block>
      <Block id="kit-combobox-multi" title="combobox/base — chọn nhiều">
        <SelectMulti />
      </Block>
      <Block id="kit-combobox-grouped" title="combobox/base — nhóm">
        <Combobox label="Tài khoản" options={GROUPED} />
      </Block>
      <Block id="kit-combobox-autocomplete" title="combobox/autocomplete">
        <Combobox label="Liên quan tới" variant="autocomplete" options={OBJECTS} />
      </Block>
      <Block id="kit-combobox-autocomplete-multi" title="combobox/autocomplete — chọn nhiều">
        <AutocompleteMulti />
      </Block>
      <Block id="kit-combobox-loading" title="combobox/base — đang tải thêm">
        <Combobox label="Liên quan tới" options={OBJECTS.slice(0, 3)} loading />
      </Block>
      <Block id="kit-combobox-error" title="combobox — lỗi">
        <Combobox label="Liên quan tới" options={OBJECTS} required error="Hãy chọn một mục." />
      </Block>
      <Block id="kit-combobox-disabled" title="combobox — vô hiệu">
        <Combobox label="Liên quan tới" options={OBJECTS} disabled defaultValue="report" />
      </Block>
      <Block id="kit-picklist" title="picklist — nhãn nhóm">
        <Picklist label="Giai đoạn" options={STAGES} defaultValue="work" />
      </Block>
      <Block id="kit-picklist-multi" title="picklist — chọn nhiều">
        <PicklistMulti />
      </Block>
      <Block id="kit-lookup" title="lookups — tìm bản ghi">
        <Lookup label="Khách hàng" search={searchAccounts} />
      </Block>
      <Block id="kit-lookup-selected" title="lookups — đã chọn">
        <Lookup label="Khách hàng" search={searchAccounts} defaultValue={ACCOUNTS[1]} />
      </Block>
      <Block id="kit-lookup-multi" title="lookups — chọn nhiều">
        <LookupMulti />
      </Block>
      <Block id="kit-datepicker" title="datepickers/base">
        <DatePicker label="Ngày hẹn" />
      </Block>
      <Block id="kit-datepicker-selected" title="datepickers/base — đã chọn ngày">
        <DatePicker label="Ngày ký hợp đồng" defaultValue="2026-09-24" formatVisible />
      </Block>
      <Block id="kit-datepicker-error" title="datepickers/base — bắt buộc, lỗi / vô hiệu">
        <div className="slds-grid slds-gutters_x-small">
          <div className="slds-col">
            <DatePicker label="Ngày sinh" required error="Hãy nhập ngày sinh." />
          </div>
          <div className="slds-col">
            <DatePicker label="Ngày tạo" defaultValue="2026-01-05" disabled />
          </div>
        </div>
      </Block>
      <Block id="kit-daterange" title="datepickers/range">
        <DateRangePicker label="Thời gian khoá học" defaultValue={{ from: "2026-09-24", to: "2026-09-30" }} />
      </Block>
      <Block id="kit-datetime" title="datetime-picker">
        <DateTimePicker label="Lịch hẹn tư vấn" required defaultValue="2026-09-24T14:30:00+07:00" />
      </Block>
      <Block id="kit-datetime-error" title="datetime-picker — bắt buộc, lỗi">
        <DateTimePicker label="Lịch hẹn tư vấn" required error="Hãy chọn ngày và giờ hẹn." />
      </Block>
      <Block id="kit-timepicker" title="timepicker — đã chọn giờ">
        <TimePicker label="Giờ hẹn" defaultValue="08:00" />
      </Block>
      <Block id="kit-timepicker-error" title="timepicker — bắt buộc, lỗi">
        <TimePicker label="Giờ hẹn" required error="Hãy nhập giờ hẹn." />
      </Block>
      <Block id="kit-timepicker-readonly" title="timepicker — chỉ đọc / vô hiệu">
        <div className="slds-grid slds-gutters_x-small">
          <div className="slds-col">
            <TimePicker label="Giờ mở cửa" defaultValue="07:30" readOnly />
          </div>
          <div className="slds-col">
            <TimePicker label="Giờ đóng cửa" disabled />
          </div>
        </div>
      </Block>
      <Block id="kit-app-filter" title="AppFilter — lọc theo app của khu Dựng (chỉ hiện khi dự án có hơn một app)">
        <AppFilter
          slug="northwind"
          route="dung/du-lieu"
          hiddenNoun="bảng"
          value="crm"
          hiddenCount={3}
          apps={[
            { id: "1", name: "crm", label: "CRM", icon: null, color: null, isDefault: true, sortOrder: 0 },
            { id: "2", name: "tk", label: "Ticketing", icon: null, color: null, isDefault: false, sortOrder: 1 },
          ]}
        />
      </Block>
    </div>
  );
}
