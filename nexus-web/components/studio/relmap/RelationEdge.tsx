"use client";

// RelationEdge (specs/075 T148, FR-088): the kind of a link is told by arrowheads + dash + a text label, never by colour.
// Neutral grey stroke, brand + thicker when selected. Edges are not Tab stops (they are reached through the link list).
import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath, type Edge, type EdgeProps } from "@xyflow/react";
import { edgeKindOf, type RelType } from "@/lib/relCanvas";
import { t } from "@/lib/i18n";

export interface RelationEdgeData extends Record<string, unknown> {
  relType: RelType;
  selected: boolean;
  parallelIndex: number;
}
export type RelationEdgeType = Edge<RelationEdgeData, "link">;

const NEUTRAL = "var(--slds-g-color-neutral-base-30)";
const BRAND = "var(--slds-g-color-brand-base-50)";

/** Hidden defs, mounted ONCE next to the canvas: one arrowhead per state (url(#…) resolves document-wide). */
export function RelationMarkerDefs() {
  return (
    <svg width="0" height="0" style={{ position: "absolute" }} aria-hidden>
      <defs>
        {([["nx-rm-arrow", NEUTRAL], ["nx-rm-arrow-sel", BRAND]] as const).map(([id, color]) => (
          <marker key={id} id={id} viewBox="0 0 10 10" markerWidth="10" markerHeight="10" markerUnits="userSpaceOnUse" refX="9" refY="5" orient="auto-start-reverse">
            <path d="M0 0 L10 5 L0 10 z" fill={color} />
          </marker>
        ))}
      </defs>
    </svg>
  );
}

export function RelationEdge({ id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, data }: EdgeProps<RelationEdgeType>) {
  const kind = edgeKindOf(data?.relType ?? "1-N");
  const selected = !!data?.selected;
  // Parallel links between the same pair fan out so they do not draw as one line.
  const off = (data?.parallelIndex ?? 0) * 18;
  const [path, labelX, labelY] = getSmoothStepPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, centerX: (sourceX + targetX) / 2 + off });
  const marker = selected ? "url(#nx-rm-arrow-sel)" : "url(#nx-rm-arrow)";
  return (
    <>
      <BaseEdge
        id={id}
        path={path}
        markerEnd={kind.endArrow ? marker : undefined}
        markerStart={kind.startArrow ? marker : undefined}
        interactionWidth={16}
        style={{ stroke: selected ? BRAND : NEUTRAL, strokeWidth: selected ? 3 : 1.5, strokeDasharray: kind.dashed ? "6 4" : undefined }}
      />
      <EdgeLabelRenderer>
        <span
          className="slds-text-body_small slds-text-color_weak nodrag nopan"
          style={{ position: "absolute", transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`, background: "var(--slds-g-color-neutral-base-95)", padding: "0 0.25rem", pointerEvents: "none" }}
        >
          {t(kind.labelKey)}
        </span>
      </EdgeLabelRenderer>
    </>
  );
}
