"use client";

// Studio relation map family (specs/075 T153): RelationNode, RelationEdge on a static canvas, RelationPanel (nothing
// selected). Static sample data; every handler is a no-op.
import { ReactFlow, ReactFlowProvider } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { RelationEdge, RelationMarkerDefs } from "@/components/studio/relmap/RelationEdge";
import { RelationNode } from "@/components/studio/relmap/RelationNode";
import { RelationPanel } from "@/components/studio/relmap/RelationPanel";

const noop = () => {};
const NODE_TYPES = { table: RelationNode };
const EDGE_TYPES = { link: RelationEdge };
const node = (id: string, label: string, x: number, selected = false) => ({
  id, type: "table", position: { x, y: 20 }, initialWidth: 200, initialHeight: 90,
  data: { name: id, label, fields: [{ name: "ten", label: "Tên" }, { name: "sdt", label: "Số điện thoại" }], selected, match: false, onSelect: noop, onMove: noop },
});
const nodes = [node("khach_hang", "Khách hàng", 0, true), node("lich_hen", "Lịch hẹn", 320), node("bac_si", "Bác sĩ", 640)];
const edges = [
  { id: "e1", source: "khach_hang", target: "lich_hen", type: "link", data: { relType: "1-N", selected: true, parallelIndex: 0 } },
  { id: "e2", source: "lich_hen", target: "bac_si", type: "link", data: { relType: "N-N", selected: false, parallelIndex: 0 } },
];

export function RelationMapSection() {
  return (
    <div>
      <p className="slds-text-title_caps slds-m-bottom_x-small">RelationNode + RelationEdge: nút đang chọn, nét liền 1-N, nét đứt N-N</p>
      <div style={{ position: "relative", height: 180 }} className="slds-m-bottom_large">
        <ReactFlowProvider>
          <RelationMarkerDefs />
          <ReactFlow nodes={nodes} edges={edges} nodeTypes={NODE_TYPES} edgeTypes={EDGE_TYPES} nodesDraggable={false} fitView proOptions={{ hideAttribution: true }} />
        </ReactFlowProvider>
      </div>
      <p className="slds-text-title_caps slds-m-bottom_x-small">RelationPanel: chưa chọn gì</p>
      <RelationPanel slug="demo" selection={null} entities={[]} edges={[]} onSelect={noop} onChanged={noop} />
    </div>
  );
}
