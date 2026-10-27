"use client";

// Assistant proposal cards (specs/076 ai2): ProposalCard for the 14 draft kinds × the states a card can be in, on
// fixtures. Showcase only, not product. The Studio token layer is on while this section is mounted (same marker as
// StudioSection) so the purple lead line and the .nx-proposed list look as they do inside Studio.
import { useSyncExternalStore } from "react";
import { ProposalCard, type ProposalState } from "@/components/studio/assistant/ProposalCard";
import { ProposalOverlayProvider, useProposalOverlay, useProposedChange, type ProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { StatusText } from "@/components/studio/StatusText";
import type { AiDraftItem } from "@/lib/aiBuilder";

const link = (id: string) => `#kit-assistant-${id}`;

const FIXTURES: Array<[string, AiDraftItem]> = [
  ["entity", { kind: "entity", entity: { name: "benh_nhan", label: "Bệnh nhân", fields: [{ name: "ten", label: "Họ tên", fieldType: "text", required: true }, { name: "ngay_sinh", label: "Ngày sinh", fieldType: "date" }] } }],
  ["field", { kind: "field", field: { entity: "benh_nhan", name: "ngay_tai_kham", label: "Ngày tái khám", fieldType: "date" } }],
  ["relationship", { kind: "relationship", relationship: { name: "benh_nhan_thuoc", entityA: "benh_nhan", entityB: "thuoc" } }],
  ["layout", { kind: "layout", layout: { entity: "benh_nhan", viewType: "edit", layoutJson: { sections: [{ title: "Thông tin chung", columns: 2, fields: ["ten", "ngay_sinh"] }] } } }],
  ["workflow", { kind: "workflow", workflow: { name: "Nhắc lịch hẹn", entityName: "lich_hen", trigger: "after_create", actions: [{ type: "field_update", field: "trang_thai", value: "moi" }], isActive: true } }],
  ["report", { kind: "report", report: { name: "Cơ hội theo giai đoạn", entityName: "co_hoi", type: "summary", config: { filters: [{ field: "stage", op: "eq", values: ["won"] }] } } }],
  ["app", { kind: "app", app: { name: "phong_kham", label: "Phòng khám", description: "Quản lý bệnh nhân và lịch hẹn", navEntities: ["benh_nhan", "lich_hen"] } }],
  ["edit_field", { kind: "edit_field", editField: { entity: "benh_nhan", field: "sdt", before: { label: "SĐT" }, after: { label: "Số điện thoại", isRequired: true } } }],
  ["delete_field", { kind: "delete_field", deleteField: { entity: "benh_nhan", field: "fax", label: "Số fax", fieldType: "text" } }],
  ["delete_entity", { kind: "delete_entity", deleteEntity: { entity: "khuyen_mai", label: "Khuyến mãi", fieldCount: 7 } }],
  ["delete_entity_pack", { kind: "delete_entity", deleteEntity: { entity: "crm_accounts", label: "Khách hàng", fieldCount: 12, packs: ["crm"], recordCount: 4 } }],
  ["records", { kind: "records", records: { entity: "benh_nhan", rows: [{ ten: "An", sdt: "0901" }, { ten: "Bình", sdt: "0902" }] } }],
  ["approval", { kind: "approval", approval: { name: "Duyệt giảm giá lớn", entityName: "bao_gia", steps: [{ approverType: "role", approverRef: "quan_ly", slaSeconds: 3600 }] } }],
  ["dashboard", { kind: "dashboard", dashboard: { name: "Tổng quan bán hàng", widgets: [{ title: "Doanh thu tháng", type: "kpi" }] } }],
  ["workqueue", { kind: "workqueue", workqueue: { name: "cho_duyet", label: "Chờ duyệt", entityName: "bao_gia", filters: [{ field: "status", op: "eq", value: "moi" }] } }],
];

const STATES: Array<[string, ProposalState | undefined]> = [
  ["mở", undefined],
  ["đang áp dụng", { status: "applying" }],
  ["đã áp dụng", { status: "done" }],
  ["áp dụng một phần", { status: "warn", message: "Còn thiếu một bước, xem App và menu." }],
  ["lỗi", { status: "error", message: "Trường đã tồn tại trong bảng này." }],
  ["đã bỏ", { status: "discarded" }],
];

const noop = () => {};

// Probe of the preview contract (specs/076 f4-proposal-overlay-hook): two fake proposed fields; one ghost at a time.
const PROBE_PAINTED = [{ docType: "field", op: "create" as const }];
const probeChange = (f: string): ProposedChange => ({ proposalKey: f, docType: "field", op: "create", label: f, target: { docType: "field", entity: "lich_hen", field: f } });
function ProbeRow({ field }: { field: string }) {
  const ghost = useProposedChange({ docType: "field", entity: "lich_hen", field });
  const o = useProposalOverlay();
  return (
    <div className={ghost ? "nx-proposed slds-p-around_x-small" : "slds-p-around_x-small"} data-testid={`probe-${field}`}>
      <span>{field}</span> {ghost && <StatusText tone="proposed">Đề xuất</StatusText>}
      <button type="button" className="slds-button slds-button_neutral slds-m-left_small" aria-pressed={!!ghost} onClick={() => o?.toggle(probeChange(field))}>
        {ghost ? "Tắt xem trước" : "Xem trước"}
      </button>
    </div>
  );
}

export function AssistantSection() {
  const studio = useSyncExternalStore(() => () => {}, () => true, () => false);
  return (
    <div>
      {studio && <span className="nx-face-studio" hidden />}
      <p className="slds-text-title_caps slds-m-bottom_x-small">Mười bốn loại, trạng thái mở</p>
      <div className="slds-grid slds-wrap" style={{ gap: "var(--slds-g-spacing-4)" }}>
        {FIXTURES.map(([id, item]) => (
          <div key={id} style={{ width: "26rem", maxWidth: "100%" }}>
            <ProposalCard item={item} asSample onAsSampleChange={noop} onApply={noop} onDiscard={noop} link={link} />
          </div>
        ))}
        <div style={{ width: "26rem", maxWidth: "100%" }}>
          <ProposalCard item={{ kind: "hologram" }} asSample onAsSampleChange={noop} onApply={noop} onDiscard={noop} link={link} />
        </div>
      </div>

      <p className="slds-text-title_caps slds-m-top_large slds-m-bottom_x-small">Probe xem trước tại chỗ (bật cái này thì tắt cái kia)</p>
      <ProposalOverlayProvider slug="demo" painted={PROBE_PAINTED} navigate={false}>
        <div style={{ width: "26rem", maxWidth: "100%" }}>
          <ProbeRow field="ngay_tai_kham" />
          <ProbeRow field="ghi_chu" />
        </div>
      </ProposalOverlayProvider>

      <p className="slds-text-title_caps slds-m-top_large slds-m-bottom_x-small">Một loại, mọi trạng thái</p>
      <div className="slds-grid slds-wrap" style={{ gap: "var(--slds-g-spacing-4)" }}>
        {STATES.map(([name, state]) => (
          <div key={name} style={{ width: "26rem", maxWidth: "100%" }}>
            <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_xx-small">{name}</p>
            <ProposalCard item={FIXTURES[1][1]} state={state} asSample onAsSampleChange={noop} onApply={noop} onDiscard={noop} link={link} />
          </div>
        ))}
        <div style={{ width: "26rem", maxWidth: "100%" }}>
          <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_xx-small">sửa cái đang có (Trước/Sau)</p>
          <ProposalCard
            item={{ ...FIXTURES[7][1], change: { docType: "field", op: "update", label: "Trường", changedPaths: ["label", "isRequired"], before: { label: "SĐT", isRequired: false }, after: { label: "Số điện thoại", isRequired: true } } }}
            asSample onAsSampleChange={noop} onApply={noop} onDiscard={noop} link={link}
          />
        </div>
      </div>
    </div>
  );
}
