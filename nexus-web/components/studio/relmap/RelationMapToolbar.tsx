"use client";

// RelationMapToolbar (specs/075 T148): one tight row on the stage's top-left corner — zoom in/out, fit, re-layout.
// No minimap, no snap switch (FR-088). Search lives in the header row of the map, not here.
import { LayoutGrid, Maximize, ZoomIn, ZoomOut } from "lucide-react";
import { t } from "@/lib/i18n";
import { ButtonGroup } from "@/components/slds/ButtonGroup";
import { ButtonIcon } from "@/components/slds/ButtonIcon";

export interface RelationMapToolbarProps {
  onZoomIn: () => void;
  onZoomOut: () => void;
  onFit: () => void;
  onRelayout: () => void;
}

export function RelationMapToolbar({ onZoomIn, onZoomOut, onFit, onRelayout }: RelationMapToolbarProps) {
  return (
    <ButtonGroup>
      <ButtonIcon variant="border-filled" icon={ZoomIn} label={t("studio.build.relMap.tool.zoomIn")} onClick={onZoomIn} />
      <ButtonIcon variant="border-filled" icon={ZoomOut} label={t("studio.build.relMap.tool.zoomOut")} onClick={onZoomOut} />
      <ButtonIcon variant="border-filled" icon={Maximize} label={t("studio.build.relMap.tool.fit")} onClick={onFit} />
      <ButtonIcon variant="border-filled" icon={LayoutGrid} label={t("studio.build.relMap.tool.relayout")} onClick={onRelayout} />
    </ButtonGroup>
  );
}
