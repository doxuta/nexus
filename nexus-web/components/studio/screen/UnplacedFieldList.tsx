"use client";

// "Trường chưa đặt" (specs/075 US1): the table's fields that are not on the form, each with an "Đặt" button.
// The accent-insensitive search shows only past 8 entries (the old builder's palette search).
import { useState } from "react";
import clsx from "clsx";
import { useDraggable, useDroppable } from "@dnd-kit/core";
import { GripVertical } from "lucide-react";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { StatusText, draftStateLabel } from "@/components/studio/StatusText";
import type { FieldMeta } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { typeLabelOf } from "./FormCanvas";
import { UNPLACED_ZONE, unplacedDragId } from "./dnd";

export const SEARCH_ABOVE = 8;

const fold = (s: string) => s.normalize("NFD").replace(/\p{M}/gu, "").replace(/\u0111/g, "d").replace(/\u0110/g, "D").toLowerCase();

export interface UnplacedFieldListProps {
  fields: FieldMeta[];
  /** The unplaced field currently opened through `?truong=` — its row is tinted. */
  highlight?: string | null;
  disabled?: boolean;
  /** A drag is in progress: the list becomes the drop zone that takes a field off the form. */
  dragging?: boolean;
  /** Record page wording ("hồ sơ" instead of "form"; "bắt buộc" is not shown). */
  detail?: boolean;
  onPlace: (name: string) => void;
}

export function UnplacedFieldList({ fields, highlight, disabled, dragging, detail, onPlace }: UnplacedFieldListProps) {
  const [q, setQ] = useState("");
  const { setNodeRef: zoneRef } = useDroppable({ id: UNPLACED_ZONE });
  const shown = q.trim() ? fields.filter((f) => fold(f.label || f.name).includes(fold(q.trim())) || fold(f.name).includes(fold(q.trim()))) : fields;
  return (
    <section aria-labelledby="nx-unplaced-title" ref={zoneRef} className={clsx(dragging && "slds-drop-zone slds-drop-zone_drag slds-p-around_x-small")}>
      <h3 id="nx-unplaced-title" className="slds-text-title_caps slds-m-bottom_x-small">
        {t("studio.build.screens.unplaced.title")} <span className="slds-text-color_weak">{fields.length}</span>
      </h3>
      {fields.length > SEARCH_ABOVE && (
        <Input label={t("studio.build.screens.unplaced.search")} labelHidden type="search" value={q} onChange={(e) => setQ(e.target.value)} placeholder={t("studio.build.screens.unplaced.search")} />
      )}
      {fields.length === 0 ? (
        <p className="slds-text-color_weak">{t(detail ? "studio.build.screens.record.unplacedEmpty" : "studio.build.screens.unplaced.empty")}</p>
      ) : (
        <ul className="slds-has-dividers_bottom">
          {shown.map((f) => (
            <UnplacedRow key={f.name} field={f} highlight={highlight === f.name} disabled={disabled} detail={detail} onPlace={onPlace} />
          ))}
        </ul>
      )}
      {dragging && <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("studio.build.screens.unplaced.dropHint")}</p>}
    </section>
  );
}

function UnplacedRow({ field: f, highlight, disabled, detail, onPlace }: { field: FieldMeta; highlight: boolean; disabled?: boolean; detail?: boolean; onPlace: (name: string) => void }) {
  const { attributes, listeners, setNodeRef, setActivatorNodeRef, isDragging } = useDraggable({ id: unplacedDragId(f.name), disabled });
  const { role: _role, tabIndex: _tab, ...attrs } = attributes;
  void _role; void _tab;
  return (
    <li ref={setNodeRef} className={clsx("slds-item slds-grid slds-grid_vertical-align-center slds-p-vertical_xx-small", highlight && "slds-theme_shade", isDragging && "opacity-50")}>
      <button
        type="button"
        ref={setActivatorNodeRef}
        {...attrs}
        {...listeners}
        aria-label={tf("studio.build.screens.dnd.handle", { name: f.label || f.name })}
        className="slds-button slds-button_icon slds-button_icon-x-small slds-m-right_x-small"
        style={{ touchAction: "none", minWidth: 24, minHeight: 24 }}
      >
        <GripVertical aria-hidden="true" className="slds-button__icon nx-lucide" />
      </button>
      <div className="slds-col slds-truncate">
        <div className="slds-truncate" title={f.label || f.name}>{f.label || f.name}</div>
        <div className="slds-text-body_small slds-text-color_weak">
          {tf(f.isRequired && !detail ? "studio.build.screens.unplaced.subRequired" : "studio.build.screens.unplaced.sub", { type: typeLabelOf(f) })}
        </div>
        {f.draftState && f.draftState !== "edited" && (() => {
          const l = draftStateLabel(f.draftState);
          return l ? <div className="slds-text-body_small"><StatusText tone={l.tone}>{l.label}</StatusText></div> : null;
        })()}
      </div>
      <Button
        variant="neutral"
        disabled={disabled}
        aria-label={tf(detail ? "studio.build.screens.record.placeLabel" : "studio.build.screens.unplaced.placeLabel", { label: f.label || f.name })}
        onClick={() => onPlace(f.name)}
      >
        {t("studio.build.screens.unplaced.place")}
      </Button>
    </li>
  );
}
