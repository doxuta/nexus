"use client";

import { AppWindow, Building2, GraduationCap, Headset, Users } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Card } from "@/components/slds/Card";
import { DropZone, DropZoneItem } from "@/components/slds/DropZone";
import { DuelingPicklist, type DuelingOption } from "@/components/slds/DuelingPicklist";
import { ExpressionBuilder } from "@/components/slds/ExpressionBuilder";
import { FileSelector } from "@/components/slds/FileSelector";
import { FilterPanel } from "@/components/slds/FilterPanel";
import { AccessMatrix } from "@/components/studio/AccessMatrix";
import { RelatedLists } from "@/components/studio/RelatedLists";
import type { RoleDetail, RoleGrant } from "@/lib/api";
import { VisualPicker, type VisualPickerOption } from "@/components/slds/VisualPicker";
import type { FilterField, FilterGroup, FilterMatch, FilterRow } from "@/lib/filterModel";

// Blueprints: panels/filtering, expression (base / custom-logic), dueling-picklist, file-selector (base / image / integrated), drop-zone, visual-picker
// (coverable-content / vertical) (SLDS 2.264.1).
// Static sample data; each block names the blueprint variant it copies. The ids on each block
// (kit-visual-picker-…) let the screenshot script shoot one builder part at a time.

function Block({ id, title, wide, children }: { id: string; title: string; wide?: boolean; children: ReactNode }) {
  return (
    <div id={id} className={wide ? "slds-col slds-size_1-of-1 slds-m-bottom_medium" : "slds-col slds-size_1-of-1 slds-medium-size_1-of-2 slds-m-bottom_medium"}>
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

const kg = (entityName: string, action: RoleGrant["action"], accessLevel: RoleGrant["accessLevel"] = "all"): RoleGrant => ({ entityName, action, accessLevel });
const KIT_ROLES: RoleDetail[] = [
  { id: "kit-r1", name: "le_tan", label: "Lễ tân", tenantId: "kit", actions: [kg("invoice", "view"), kg("invoice", "create"), kg("invoice", "edit"), kg("visit", "view")] },
  { id: "kit-r2", name: "quan_ly", label: "Quản lý", tenantId: "kit", actions: [kg("*", "view"), kg("*", "create"), kg("*", "edit")] },
  { id: "kit-r3", name: "ke_toan", label: "Kế toán", tenantId: "kit", actions: [kg("invoice", "create")] },
];

const APPS: VisualPickerOption[] = [
  { value: "crm", label: "Bán hàng", icon: Building2, iconName: "account" },
  { value: "lms", label: "Đào tạo", icon: GraduationCap, iconName: "education" },
  { value: "desk", label: "Chăm sóc", icon: Headset, iconName: "case" },
  { value: "custom", label: "Ứng dụng riêng", icon: AppWindow, disabled: true },
];

const OBJECTS: VisualPickerOption[] = [
  { value: "account", label: "Khách hàng", icon: Building2, iconName: "account" },
  { value: "contact", label: "Liên hệ", icon: Users, iconName: "contact" },
  { value: "student", label: "Học viên", icon: GraduationCap, iconName: "education" },
];

const FIELD_TYPES: VisualPickerOption[] = [
  { value: "text", label: "Văn bản", description: "Một dòng chữ tự do, tối đa 255 ký tự." },
  { value: "number", label: "Số", description: "Số nguyên hoặc thập phân, dùng được trong báo cáo tổng hợp." },
  { value: "picklist", label: "Danh sách chọn", description: "Chọn một giá trị trong danh sách định sẵn." },
];

function AppPicker() {
  const [v, setV] = useState<string | null>("crm");
  return <VisualPicker label="Chọn ứng dụng" options={APPS} value={v} onChange={setV} />;
}

function ObjectPicker() {
  const [v, setV] = useState<string[]>(["account", "student"]);
  return <VisualPicker label="Thêm các đối tượng sau" options={OBJECTS} multiple value={v} onChange={setV} size="small" />;
}

function FieldTypePicker() {
  const [v, setV] = useState<string | null>("number");
  return <VisualPicker label="Kiểu trường" variant="vertical" options={FIELD_TYPES} value={v} onChange={setV} />;
}

const DEAL_FIELDS: FilterField[] = [
  { name: "name", label: "Tên cơ hội", fieldType: "text" },
  { name: "amount", label: "Giá trị hợp đồng", fieldType: "currency" },
  { name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["new", "proposal", "won", "lost"], valueLabels: { new: "Mới", proposal: "Đã gửi báo giá", won: "Thắng", lost: "Thua" } } },
  { name: "vip", label: "Khách VIP", fieldType: "boolean" },
  { name: "closed_at", label: "Ngày chốt", fieldType: "date" },
];

const r = (id: string, field: string, op: string, value = ""): FilterRow => ({ id, field, op, value });

function ExpressionAnd() {
  const [rows, setRows] = useState<FilterRow[]>([r("e1", "amount", "gt", "100000000"), r("e2", "stage", "eq", "proposal")]);
  return <ExpressionBuilder fields={DEAL_FIELDS} value={rows} onChange={setRows} />;
}

function ExpressionGroups() {
  const [rows, setRows] = useState<FilterRow[]>([r("g1", "stage", "in", "new,proposal")]);
  const [match, setMatch] = useState<FilterMatch>("all");
  const [groups, setGroups] = useState<FilterGroup[]>([
    { id: "grp1", match: "any", rows: [r("g2", "vip", "eq", "true"), r("g3", "closed_at", "eq", "this_month")] },
  ]);
  return <ExpressionBuilder fields={DEAL_FIELDS} value={rows} onChange={setRows} match={match} onMatchChange={setMatch} groups={groups} onGroupsChange={setGroups} />;
}

function ExpressionCustomLogic() {
  const [rows, setRows] = useState<FilterRow[]>([r("c1", "stage", "eq", "won"), r("c2", "amount", "between", "50000000,200000000"), r("c3", "name", "notnull")]);
  const [match, setMatch] = useState<FilterMatch>("custom");
  const [logic, setLogic] = useState("1 AND (2 OR 4)");
  return (
    <ExpressionBuilder fields={DEAL_FIELDS} value={rows} onChange={setRows} match={match} onMatchChange={setMatch} customLogic={logic} onCustomLogicChange={setLogic} />
  );
}

function ExpressionError() {
  const [rows, setRows] = useState<FilterRow[]>([r("x1", "stage", "eq", "won"), r("x2", "name", "eq")]);
  return <ExpressionBuilder fields={DEAL_FIELDS} value={rows} onChange={setRows} errors={{ x2: "Nhập giá trị" }} />;
}

const PRODUCT_FIELDS: FilterField[] = [
  { name: "name", label: "Tên sản phẩm", fieldType: "text" },
  { name: "created_at", label: "Ngày tạo", fieldType: "date" },
  { name: "price", label: "Giá niêm yết", fieldType: "currency" },
  { name: "status", label: "Trạng thái", fieldType: "picklist", options: { values: ["active", "paused"], valueLabels: { active: "Đang bán", paused: "Tạm ngưng" } } },
];

function FilterPanelDemo({ variant }: { variant: "base" | "new" | "error" | "locked" }) {
  const [rows, setRows] = useState<FilterRow[]>([
    r("p1", "created_at", "eq", "this_week"),
    r("p2", "price", "gt", "500000"),
    // A row without a field renders as the blueprint's "New filter" item until its popover closes.
    ...(variant === "new" ? [r("p4", "", "")] : []),
    ...(variant === "error" ? [r("p3", "status", "eq", "stopped")] : []),
  ]);
  return (
    // Blueprint demo frame: a fixed-height flex row the panel fills.
    <div className="slds-grid h-120">
      <FilterPanel
        fields={PRODUCT_FIELDS}
        value={rows}
        onChange={setRows}
        onClose={() => {}}
        dirty={variant === "new" || variant === "error"}
        onCancel={() => {}}
        onSave={() => {}}
        errors={variant === "error" ? { p3: "Giá trị không còn trong danh sách" } : undefined}
        locked={variant === "locked" ? [r("p9", "name", "eq", "ACME")] : undefined}
      />
    </div>
  );
}

const COLUMNS: DuelingOption[] = [
  { value: "name", label: "Tên khách hàng", locked: true },
  { value: "phone", label: "Số điện thoại" },
  { value: "email", label: "Email" },
  { value: "owner", label: "Người phụ trách" },
  { value: "stage", label: "Giai đoạn" },
  { value: "created", label: "Ngày tạo" },
  { value: "city", label: "Tỉnh / Thành phố" },
];

function ColumnPicker() {
  const [v, setV] = useState<string[]>(["name", "stage", "owner"]);
  return <DuelingPicklist label="Cột hiển thị" options={COLUMNS} value={v} onChange={setV} sourceLabel="Cột có sẵn" selectedLabel="Cột đã chọn" />;
}

function LanguagePicker() {
  const [v, setV] = useState<string[]>(["email"]);
  return <DuelingPicklist label="Trường bắt buộc" options={COLUMNS.slice(1)} value={v} onChange={setV} reorder={false} required />;
}

function PickFiles() {
  const [names, setNames] = useState<string[]>([]);
  return (
    <>
      <FileSelector label="Tệp đính kèm" multiple onFiles={(files) => setNames(files.map((f) => f.name))} />
      {names.length > 0 && <p className="slds-text-body_small slds-m-top_x-small">Đã chọn: {names.join(", ")}</p>}
    </>
  );
}

const ACCOUNT = { glyph: Building2, name: "account" };

export function BuildersSection() {
  return (
    <div className="slds-grid slds-wrap slds-gutters">
      <Block id="kit-filter-panel" title="panels/filtering — mặc định · bộ lọc mới · lỗi · bộ lọc bị khoá" wide>
        <div className="slds-grid slds-wrap slds-gutters">
          {(["base", "new", "error", "locked"] as const).map((v) => (
            <div key={v} className="slds-col slds-size_1-of-2 slds-large-size_1-of-4 slds-m-bottom_small">
              <FilterPanelDemo variant={v} />
            </div>
          ))}
        </div>
      </Block>
      <Block id="kit-expression" title="expression/base — tất cả điều kiện (VÀ)" wide>
        <ExpressionAnd />
      </Block>
      <Block id="kit-expression-group" title="expression/base — chọn cách khớp, nhóm điều kiện" wide>
        <ExpressionGroups />
      </Block>
      <Block id="kit-expression-custom-logic" title="expression/custom-logic — logic sai (điều kiện 4 không tồn tại)" wide>
        <ExpressionCustomLogic />
      </Block>
      <Block id="kit-expression-error" title="expression/base — lỗi giá trị" wide>
        <ExpressionError />
      </Block>
      <Block id="kit-expression-disabled" title="expression/base — vô hiệu" wide>
        <ExpressionBuilder fields={DEAL_FIELDS} value={[r("d1", "amount", "gte", "1000000")]} onChange={() => {}} disabled />
      </Block>
      <Block id="kit-dueling-picklist" title="dueling-picklist — sắp thứ tự, mục khoá" wide>
        <ColumnPicker />
      </Block>
      <Block id="kit-dueling-picklist-plain" title="dueling-picklist — không sắp thứ tự, bắt buộc" wide>
        <LanguagePicker />
      </Block>
      <Block id="kit-dueling-picklist-disabled" title="dueling-picklist — vô hiệu / chế độ xem" wide>
        <div className="slds-grid slds-gutters">
          <div className="slds-col">
            <DuelingPicklist label="Cột hiển thị" options={COLUMNS} value={["name", "email"]} onChange={() => {}} disabled />
          </div>
          <div className="slds-col">
            <DuelingPicklist label="Cột hiển thị" options={COLUMNS} value={["name", "email", "stage"]} onChange={() => {}} readOnly />
          </div>
        </div>
      </Block>
      <Block id="kit-file-selector" title="file-selector/base">
        <PickFiles />
      </Block>
      <Block id="kit-file-selector-error" title="file-selector/base — lỗi">
        <FileSelector label="Tệp đính kèm" accept="image/png" error="Không hỗ trợ kiểu tệp này." onFiles={() => {}} />
      </Block>
      <Block id="kit-file-selector-image" title="file-selector/image">
        <FileSelector label="Logo" variant="images" accept="image/*" onFiles={() => {}} />
      </Block>
      <Block id="kit-file-selector-integrated" title="file-selector/integrated — kéo tệp vào vùng này">
        <FileSelector variant="integrated" onFiles={() => {}}>
          {/* min-h-56: the overlay's drop card is 12rem square; shorter content clips it. */}
          <p className="slds-p-around_medium min-h-56">Kéo một tệp từ máy vào khung này: lớp phủ &quot;Thả tệp vào đây&quot; hiện lên trên nội dung.</p>
        </FileSelector>
      </Block>
      <Block id="kit-drop-zone" title="drop-zone — vùng có thành phần">
        {/* Blueprint demo wrapper: padding-top 2rem so the label tab above the zone stays clear. */}
        <div className="slds-p-top_large">
          <DropZone>
            <DropZoneItem label="Thông tin khách hàng" hovered onEdit={() => {}} onRemove={() => {}} moveHandle={{}}>
              <Card boundary title="Khách hàng" icon={ACCOUNT} className="slds-m-bottom_small">
                Công ty Cổ phần Acme Việt Nam
              </Card>
            </DropZoneItem>
            <DropZoneItem label="Liên hệ chính" onEdit={() => {}} onRemove={() => {}} moveHandle={{}}>
              <Card boundary title="Liên hệ" icon={ACCOUNT} className="slds-m-top_small">
                Nguyễn Thị Lan · 0912 345 678
              </Card>
            </DropZoneItem>
          </DropZone>
        </div>
      </Block>
      <Block id="kit-drop-zone-drag" title="drop-zone — đang kéo qua (điểm chèn)">
        <div className="slds-p-top_large">
          <DropZone dragOver label="Nội dung" slot={1}>
            <DropZoneItem label="Thông tin khách hàng" onEdit={() => {}}>
              <Card boundary title="Khách hàng" icon={ACCOUNT}>
                Công ty Cổ phần Acme Việt Nam
              </Card>
            </DropZoneItem>
            <DropZoneItem label="Liên hệ chính" onEdit={() => {}}>
              <Card boundary title="Liên hệ" icon={ACCOUNT}>
                Nguyễn Thị Lan · 0912 345 678
              </Card>
            </DropZoneItem>
          </DropZone>
        </div>
      </Block>
      <Block id="kit-visual-picker" title="visual-picker/coverable-content — chọn một" wide>
        <AppPicker />
      </Block>
      <Block id="kit-visual-picker-multi" title="visual-picker/coverable-content — chọn nhiều, nhỏ">
        <ObjectPicker />
      </Block>
      <Block id="kit-visual-picker-vertical" title="visual-picker/vertical">
        <FieldTypePicker />
      </Block>
      <Block id="kit-related-lists" title="studio/RelatedLists — đang tải · lỗi · rỗng · có dữ liệu (mỗi mục độc lập)" wide>
        <RelatedLists
          label="Việc liên quan (mẫu)"
          sections={[
            { id: "a", title: "Màn hình", state: "loading", items: [], empty: "" },
            { id: "b", title: "Liên kết", state: "error", error: "Không tải được mục này.", items: [], empty: "", onRetry: () => {} },
            { id: "c", title: "Báo cáo", state: "ready", count: 0, items: [], empty: "Chưa có báo cáo nào dùng bảng này." },
            { id: "d", title: "Dữ liệu", state: "ready", count: 2, items: [<a key="1" href="#kit-related-lists">Khách hàng</a>, <a key="2" href="#kit-related-lists">Lịch hẹn</a>], empty: "" },
          ]}
        />
      </Block>
      <Block id="kit-access-matrix" title="studio/AccessMatrix — vai × bảng, ba mức, cột vai cố định (ô đổi gọi máy chủ thật)" wide>
        <AccessMatrix
          roles={KIT_ROLES}
          tables={[{ name: "invoice", label: "Hoá đơn" }, { name: "visit", label: "Lịch hẹn" }, { name: "patient", label: "Bệnh nhân" }]}
          roleHref={() => "#kit-access-matrix"}
          proposed={new Set(["kit-r1|visit"])}
        />
      </Block>
      <Block id="kit-visual-picker-error" title="visual-picker — bắt buộc, lỗi">
        <VisualPicker label="Chọn ứng dụng" options={APPS.slice(0, 2)} size="small" required error="Hãy chọn một ứng dụng." />
      </Block>
    </div>
  );
}
