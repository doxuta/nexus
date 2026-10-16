"use client";

// Data table family (specs/038 §5.4): DataTable (data-tables/advanced, inline-edit) and TreeGrid
// (tree-grid/base). Static sample data only; each block names the blueprint/variant it copies and
// carries an id so it can be screenshotted on its own.
import type { ColumnDef } from "@tanstack/react-table";
import { useState, type ReactNode } from "react";
import { DataTable, type CellDrafts } from "@/components/slds/DataTable";
import { MenuItem } from "@/components/slds/Menu";
import { TreeGrid } from "@/components/slds/TreeGrid";

interface Opportunity {
  id: string;
  name: string;
  account: string;
  closeDate: string;
  stage: string;
  confidence: string;
  amount: string;
  contact: string;
}

const OPPORTUNITIES: Opportunity[] = [
  { id: "o1", name: "Minh Phát - 1.200 bộ thiết bị", account: "Công ty Minh Phát", closeDate: "10/04/2026", stage: "Đề xuất giá trị", confidence: "30%", amount: "25.000.000.000 ₫", contact: "an.nguyen@minhphat.vn" },
  { id: "o2", name: "An Khang - 200 bộ thiết bị", account: "Tập đoàn An Khang", closeDate: "31/01/2026", stage: "Tìm hiểu nhu cầu", confidence: "60%", amount: "5.000.000.000 ₫", contact: "binh.tran@ankhang.vn" },
  { id: "o3", name: "Bảo Tín - 1.000 bộ thiết bị", account: "Bảo Tín", closeDate: "31/01/2026 15:45", stage: "Xác định người quyết định", confidence: "70%", amount: "25.000.000 ₫", contact: "cuong.le@baotin.vn" },
];

// Record-name and email cells are links in the blueprint.
const link = (text: string) => <a href="#">{text}</a>;

const OPP_COLUMNS: ColumnDef<Opportunity>[] = [
  { accessorKey: "name", header: "Tên cơ hội", cell: (c) => link(c.getValue<string>()) },
  { accessorKey: "account", header: "Khách hàng" },
  { accessorKey: "closeDate", header: "Ngày chốt" },
  { accessorKey: "stage", header: "Giai đoạn" },
  { accessorKey: "confidence", header: "Khả năng", meta: { numeric: true } },
  { accessorKey: "amount", header: "Giá trị", meta: { numeric: true } },
  { accessorKey: "contact", header: "Liên hệ", cell: (c) => link(c.getValue<string>()) },
];

// Inline edit: every column but "Khả năng" (a computed value) is editable; the name is required.
const EDIT_COLUMNS: ColumnDef<Opportunity>[] = OPP_COLUMNS.map((col) => ({
  ...col,
  meta: { ...col.meta, editable: col.header !== "Khả năng", required: col.header === "Tên cơ hội" },
}));

function InlineEditDemo() {
  const [drafts, setDrafts] = useState<CellDrafts>({ o1: { account: "Công ty CP Minh Phát" } });
  return (
    <DataTable
      label="Sửa nhanh cơ hội"
      columns={EDIT_COLUMNS}
      data={OPPORTUNITIES}
      editable
      selectable
      sortable
      resizable
      rowActions={ROW_ACTIONS}
      drafts={drafts}
      onDraftsChange={setDrafts}
      cellErrors={{ o3: { stage: "Giai đoạn không hợp lệ cho cơ hội đã đóng" } }}
    />
  );
}

// 1,000 rows: past 100 the body is windowed; the header stays pinned while it scrolls.
const STAGES = ["Tìm hiểu nhu cầu", "Đề xuất giá trị", "Đàm phán", "Đã chốt"];
const MANY: Opportunity[] = Array.from({ length: 1000 }, (_, i) => ({
  id: `m${i + 1}`,
  name: `Cơ hội số ${i + 1}`,
  account: `Khách hàng ${(i % 37) + 1}`,
  closeDate: `${String((i % 28) + 1).padStart(2, "0")}/${String((i % 12) + 1).padStart(2, "0")}/2026`,
  stage: STAGES[i % STAGES.length],
  confidence: `${(i * 7) % 100}%`,
  amount: `${((i * 37) % 900) + 100}.000.000 ₫`,
  contact: `lienhe${i + 1}@khachhang.vn`,
}));

interface AccountNode {
  id: string;
  name: string;
  employees: string;
  phone: string;
  owner: string;
  city: string;
  children?: AccountNode[];
}

const ACCOUNT_TREE: AccountNode[] = [
  { id: "t1", name: "Công ty Minh Phát", employees: "3.100", phone: "028 3822 1212", owner: "Trần Thị Bình", city: "TP. Hồ Chí Minh" },
  {
    id: "t2",
    name: "Tập đoàn An Khang",
    employees: "10.000",
    phone: "024 3933 1212",
    owner: "Nguyễn Văn An",
    city: "Hà Nội",
    children: [
      { id: "t2-1", name: "An Khang Đà Nẵng", employees: "745", phone: "0236 355 1212", owner: "Nguyễn Văn An", city: "Đà Nẵng" },
      {
        id: "t2-2",
        name: "An Khang Cần Thơ",
        employees: "578",
        phone: "0292 373 1212",
        owner: "Lê Hoàng Cường",
        city: "Cần Thơ",
        children: [{ id: "t2-2-1", name: "An Khang Sóc Trăng", employees: "90", phone: "0299 382 1212", owner: "Lê Hoàng Cường", city: "Sóc Trăng" }],
      },
    ],
  },
  { id: "t3", name: "Bảo Tín", employees: "6.000", phone: "0225 384 1212", owner: "Nguyễn Văn An", city: "Hải Phòng" },
  { id: "t4", name: "Phúc Long Foods", employees: "1.234", phone: "028 3930 1212", owner: "Trần Thị Bình", city: "Bình Dương" },
];

const TREE_COLUMNS: ColumnDef<AccountNode>[] = [
  { accessorKey: "name", header: "Khách hàng", cell: (c) => link(c.getValue<string>()) },
  { accessorKey: "employees", header: "Nhân sự", meta: { numeric: true } },
  { accessorKey: "phone", header: "Điện thoại" },
  { accessorKey: "owner", header: "Người phụ trách", cell: (c) => link(c.getValue<string>()) },
  { accessorKey: "city", header: "Thành phố" },
];

const ROW_ACTIONS = () => (
  <>
    <MenuItem label="Sửa" />
    <MenuItem label="Xoá" />
  </>
);

function Block({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <div id={id} className="slds-m-bottom_large">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

// specs/073 u0-be-mat: stacked under 768px, row class (a proposed row), whole-row click + Enter.
// The row itself is the link, so the name column is plain text here (no inner control to steal Enter).
const ROW_COLUMNS: ColumnDef<Opportunity>[] = [{ accessorKey: "name", header: "Tên cơ hội" }, ...OPP_COLUMNS.slice(1, 5)];

function RowClickDemo() {
  const [opened, setOpened] = useState<string | null>(null);
  return (
    <>
      <DataTable
        label="Cơ hội bấm cả dòng"
        columns={ROW_COLUMNS}
        data={OPPORTUNITIES}
        stacked
        rowClassName={(r) => (r.id === "o2" ? "nx-proposed" : undefined)}
        onRowClick={(r) => setOpened(r.name)}
      />
      <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small" aria-live="polite">
        {opened ? `Đã mở: ${opened}` : "Bấm một dòng, hoặc chọn ô rồi nhấn Enter."}
      </p>
    </>
  );
}

export function DataTableSection() {
  return (
    <>
      <Block id="kit-data-table-row" title="DataTable (xếp chồng dưới 768px · lớp dòng · bấm cả dòng)">
        <RowClickDemo />
      </Block>
      <Block id="kit-data-table" title="DataTable (data-tables/advanced — default)">
        <DataTable label="Danh sách cơ hội" columns={OPP_COLUMNS} data={OPPORTUNITIES} sortable selectable resizable rowActions={ROW_ACTIONS} />
      </Block>
      <Block id="kit-data-table-sorted" title="DataTable (data-tables/advanced — sorted-column-asc, row-selected)">
        <DataTable
          label="Cơ hội đã sắp xếp theo tên"
          columns={OPP_COLUMNS}
          data={OPPORTUNITIES}
          sortable
          selectable
          rowActions={ROW_ACTIONS}
          defaultSorting={[{ id: "name", desc: false }]}
          defaultRowSelection={{ o2: true }}
        />
      </Block>
      <Block id="kit-data-table-edit" title="DataTable (data-tables/inline-edit — edited, error)">
        <InlineEditDemo />
      </Block>
      <Block id="kit-data-table-virtual" title="DataTable (1.000 dòng — ảo hoá, header dính, đổi rộng cột)">
        <DataTable
          label="Cơ hội (1.000 dòng)"
          columns={OPP_COLUMNS}
          data={MANY}
          sortable
          selectable
          resizable
          rowActions={ROW_ACTIONS}
          maxHeight="20rem"
        />
      </Block>
      <Block id="kit-tree-grid" title="TreeGrid (tree-grid/base — expanded, selected-row)">
        <TreeGrid
          label="Khách hàng theo công ty mẹ"
          columns={TREE_COLUMNS}
          data={ACCOUNT_TREE}
          getSubRows={(a) => a.children}
          defaultExpanded={{ t2: true }}
          defaultRowSelection={{ t2: true }}
          selectable
          sortable
          resizable
          rowActions={ROW_ACTIONS}
        />
      </Block>
      <Block id="kit-data-table-states" title="DataTable (đang tải · rỗng)">
        <div className="slds-grid slds-gutters">
          <div className="slds-col">
            <DataTable label="Cơ hội đang tải" columns={OPP_COLUMNS.slice(0, 3)} data={OPPORTUNITIES.slice(0, 1)} loading />
          </div>
          <div className="slds-col">
            <DataTable label="Cơ hội rỗng" columns={OPP_COLUMNS.slice(0, 3)} data={[]} />
          </div>
        </div>
      </Block>
    </>
  );
}
