"use client";

// Studio screen editor family (specs/075 L1a): ScreenBar, CanvasCell (hover / selected look = the SLDS drop-zone
// container), UnplacedFieldList, FieldProperties. Static sample data; every handler is a no-op.
import { createRef } from "react";
import { FormElement } from "@/components/slds/FormElement";
import type { ApiRecord, EntityMeta, FieldMeta } from "@/lib/api";
import { draftFromSections, type DraftFieldCell } from "@/lib/layoutDraft";
import { CanvasCell } from "@/components/studio/screen/CanvasCell";
import { FieldProperties } from "@/components/studio/screen/FieldProperties";
import { SectionProperties, SectionsPanel } from "@/components/studio/screen/SectionProperties";
import { BulkBar } from "@/components/studio/screen/BulkBar";
import { BulkFieldTable } from "@/components/studio/screen/BulkFieldTable";
import { ScreenBar } from "@/components/studio/screen/ScreenBar";
import { RecordPageCanvas } from "@/components/studio/screen/RecordPageCanvas";
import { ListCanvas } from "@/components/studio/screen/ListCanvas";
import { ListColumnsPanel } from "@/components/studio/screen/ListColumnsPanel";
import { ViewFieldsPanel } from "@/components/studio/screen/ViewFieldsPanel";
import { listDraftFromView } from "@/lib/screenEditorModel";
import { RelatedListSettings } from "@/components/studio/screen/RelatedListSettings";
import { UnplacedFieldList } from "@/components/studio/screen/UnplacedFieldList";

const noop = () => {};
const F = (name: string, label: string, isRequired = false): FieldMeta => ({ name, label, fieldType: "text", isRequired }) as FieldMeta;
const entities = [{ name: "lich_hen", label: "Lịch hẹn" }, { name: "benh_nhan", label: "Bệnh nhân" }] as EntityMeta[];
const sections = draftFromSections([{ title: "Thông tin lịch hẹn", columns: 2, fields: ["benh_nhan", "ghi_chu"] }]);
const cell = sections[0].cells[0] as DraftFieldCell;
const unplaced = [F("ngay_tai_kham", "Ngày tái khám"), F("bac_si", "Bác sĩ", true)];

function Demo({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="slds-m-bottom_large">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

const listFields = [
  F("benh_nhan", "Bệnh nhân"),
  { name: "trang_thai", label: "Trạng thái", fieldType: "picklist", options: { values: ["moi", "da_den"], valueLabels: { moi: "Mới", da_den: "Đã đến" } } } as unknown as FieldMeta,
  { name: "ngay", label: "Ngày hẹn", fieldType: "date" } as FieldMeta,
];
const listDraft = { ...listDraftFromView(null, listFields), cols: ["benh_nhan", "trang_thai"], kanban: "trang_thai", calendar: "ngay" };
const sampleRecord = { id: "r1", version: 1, data: { benh_nhan: "Nguyễn Văn An", ghi_chu: "Tái khám sau 2 tuần" } } as ApiRecord;

export function ScreenEditorSection() {
  return (
    <div>
      <Demo title="BulkBar + BulkFieldTable: chế độ Danh sách">
        <BulkBar count={2} sections={[{ id: sections[0].id, title: "Thông tin lịch hẹn" }]} message="Bỏ qua 1 trường bắt buộc: Bác sĩ." onRequired={noop} onRemove={noop} onMove={noop} onClear={noop} />
        <BulkFieldTable fields={[F("benh_nhan", "Bệnh nhân", true), F("ghi_chu", "Ghi chú"), ...unplaced]} draft={sections} selected={["ghi_chu", "bac_si"]} onSelect={noop} onSection={noop} onRequired={noop} />
      </Demo>
      <Demo title="ScreenBar: đã đủ / còn thiếu / chưa lưu">
        <ScreenBar entities={entities} bang="lich_hen" man="form" missing={[]} dirty={false} saving={false} locked={false} canReset={false} onPick={noop} onSave={noop} onDiscard={noop} onReset={noop} />
        <ScreenBar entities={entities} bang="lich_hen" man="form" missing={[F("bac_si", "Bác sĩ", true)]} dirty saving={false} locked={false} canReset onPick={noop} onSave={noop} onDiscard={noop} onReset={noop} />
      </Demo>
      <Demo title="CanvasCell: thường · đang chọn">
        <div className="grid grid-cols-2 gap-4">
          <CanvasCell cellId="k1" dragName="Bệnh nhân" ariaLabel="Trường Bệnh nhân" selected={false} tabbable onSelect={noop} onNavKey={noop} cellRef={createRef()}>
            <FormElement label="Bệnh nhân" required><input className="slds-input" placeholder="Tìm bệnh nhân…" /></FormElement>
          </CanvasCell>
          <CanvasCell cellId="k2" dragName="Bác sĩ" ariaLabel="Trường Bác sĩ" selected tabbable={false} onSelect={noop} onNavKey={noop} cellRef={createRef()}>
            <FormElement label="Bác sĩ" required><input className="slds-input" placeholder="Chọn người" /></FormElement>
          </CanvasCell>
        </div>
      </Demo>
      <Demo title="Trạng thái kéo: danh sách chưa đặt thành vùng thả, viền chấm chỉ có khi đang kéo">
        <div className="grid grid-cols-2 gap-4">
          <UnplacedFieldList fields={unplaced} dragging onPlace={noop} />
          <UnplacedFieldList fields={unplaced} onPlace={noop} />
        </div>
      </Demo>
      <Demo title="SectionProperties: danh sách khung và thuộc tính khung">
        <div className="grid grid-cols-2 gap-4">
          <SectionsPanel sections={sections} selectedId={sections[0].id} onPick={noop} />
          <SectionProperties section={sections[0]} canUp={false} canDown removeBlocked onTitle={noop} onColumns={noop} onUp={noop} onDown={noop} onAddSpacer={noop} onRemove={noop} />
        </div>
      </Demo>
      <div className="grid grid-cols-2 gap-4">
        <Demo title="UnplacedFieldList">
          <UnplacedFieldList fields={unplaced} highlight="bac_si" onPlace={noop} />
        </Demo>
        <Demo title="FieldProperties">
          <FieldProperties
            cell={cell} field={F("benh_nhan", "Bệnh nhân", true)} unplaced={null} sections={sections} fields={[F("benh_nhan", "Bệnh nhân", true)]} dependents={[]} onWhen={noop} requiredOn onRequired={noop} sectionId={sections[0].id}
            canUp={false} canDown removeBlocked locked={false} tableHref="#kit-screen-editor" labelRef={createRef()}
            onLabel={noop} onUp={noop} onDown={noop} onToSection={noop} onRemove={noop} onPlace={noop} onDone={noop} onOption={noop}
          />
        </Demo>
      </div>
      <Demo title="RecordPageCanvas (tab Tổng quan)">
        <RecordPageCanvas
          slug="demo" entity="lich_hen" tableLabel="Lịch hẹn" fields={[F("benh_nhan", "Bệnh nhân"), F("ghi_chu", "Ghi chú")]} draft={sections}
          record={sampleRecord} appName="Phòng khám" selected="ghi_chu" relId={null} onRel={noop} onSelect={noop} onDeselect={noop} onAddSection={noop}
        />
      </Demo>
      <Demo title="RelatedListSettings">
        <div style={{ maxWidth: 320 }}>
          <RelatedListSettings slug="demo" relId="kit" relType="1-N" parentLabel="Khách hàng" childLabel="Lịch hẹn" childFields={[F("ngay", "Ngày hẹn"), F("ghi_chu", "Ghi chú")]} />
        </div>
      </Demo>
      <Demo title="ListCanvas (danh sách trên màn thật)">
        <ListCanvas tableLabel="Lịch hẹn" fields={listFields} draft={listDraft} records={[sampleRecord]} appName="Phòng khám" selected="trang_thai" onSelect={noop} />
      </Demo>
      <div className="grid grid-cols-2 gap-4">
        <Demo title="ListColumnsPanel">
          <ListColumnsPanel fields={listFields} draft={listDraft} selected="trang_thai" onSelect={noop} onChange={noop} />
        </Demo>
        <Demo title="ViewFieldsPanel">
          <ViewFieldsPanel fields={listFields} draft={listDraft} onChange={noop} />
        </Demo>
      </div>
    </div>
  );
}
