"use client";

// RelationNode (specs/075 T148, FR-088): a table on the relation map — plain text block, no initials tile, no colour
// per table. The whole node is ONE button: Tab reaches it, Enter selects it, arrow keys move it 12px. Selected = a 2px
// brand border plus aria-pressed (never colour alone). The handles stay invisible; they only anchor the edges (and the
// drag-to-connect).
import { Handle, Position, type NodeProps, type Node } from "@xyflow/react";
import { tf } from "@/lib/i18n";

export interface RelationNodeData extends Record<string, unknown> {
  name: string;
  label: string;
  fields: ReadonlyArray<{ name: string; label?: string }>;
  selected: boolean;
  match: boolean;
  onSelect: (name: string) => void;
  onMove: (name: string, dx: number, dy: number) => void;
}
export type RelationNodeType = Node<RelationNodeData, "table">;

const STEP = 12;
const ARROWS: Record<string, [number, number]> = { ArrowLeft: [-STEP, 0], ArrowRight: [STEP, 0], ArrowUp: [0, -STEP], ArrowDown: [0, STEP] };
// Invisible 14px strips on the left/right edge: dragging from one onto another table starts a new link (parent = where it began).
const hidden = { opacity: 0, width: 14, height: 28, cursor: "crosshair" } as const;

export function RelationNode({ data }: NodeProps<RelationNodeType>) {
  const extra = data.fields.slice(0, 3);
  return (
    <>
      <Handle type="target" position={Position.Left} style={hidden} />
      <button
        type="button"
        aria-pressed={data.selected}
        className="slds-box slds-box_x-small slds-theme_default slds-text-align_left"
        style={{
          width: "100%",
          borderWidth: data.selected || data.match ? 2 : 1,
          borderColor: data.selected || data.match ? "var(--slds-g-color-brand-base-50)" : undefined,
        }}
        onClick={() => data.onSelect(data.name)}
        onKeyDown={(e) => {
          const d = ARROWS[e.key];
          if (!d) return;
          e.preventDefault();
          data.onMove(data.name, d[0], d[1]);
        }}
      >
        <span className="slds-text-heading_small slds-truncate" style={{ display: "block", fontWeight: 700 }}>{data.label}</span>
        <span className="slds-text-body_small slds-text-color_weak" style={{ display: "block" }}>{tf("studio.build.relMap.node.fields", { n: data.fields.length })}</span>
        {extra.length > 0 && (
          <ul className="slds-text-body_small slds-text-color_weak slds-m-top_xx-small">
            {extra.map((f) => <li key={f.name} className="slds-truncate">{f.label ?? f.name}</li>)}
          </ul>
        )}
        {data.fields.length > 3 && (
          <span className="slds-text-body_small slds-text-color_weak">{tf("studio.build.relMap.node.more", { n: data.fields.length - 3 })}</span>
        )}
      </button>
      <Handle type="source" position={Position.Right} style={hidden} />
    </>
  );
}
