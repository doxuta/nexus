"use client";

// SLDS drop zone (blueprint `drop-zone` base / drag-over / with-populated-region(-and-actions) /
// dragging-with-cards, SLDS 2.264.1) — the screen-builder region components are dragged into
// (Layout Builder), not a file drop (that is FileSelector).
//   div.slds-drop-zone[.slds-drop-zone_drag]
//     [> div.slds-drop-zone__label > media(builder icon + h2 label)]   while dragging over
//     > children, with div.slds-drop-zone_drag__slot at the insertion index
//   DropZoneItem: div.slds-drop-zone__container[.slds-is-hovered][tabindex=-1]
//     > div.slds-drop-zone__label.slds-drop-zone__label_container > media(icon + h2 > button "Sửa: <name>")
//     + content + div.slds-drop-zone__actions > div.slds-button-group[role=group] > Move / Remove
// Presentation only: the owner runs the drag (@dnd-kit useDroppable / useSortable) and passes the
// state in; the Move button takes the sortable's activator props so it doubles as the drag handle.
import clsx from "clsx";
import { Blocks, GripVertical, LayoutPanelTop, X, type LucideIcon } from "lucide-react";
import { Children, type ComponentProps, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { ButtonIcon } from "./ButtonIcon";

export interface DropZoneProps {
  /** Something is being dragged over the zone (`slds-drop-zone_drag`). */
  dragOver?: boolean;
  /** Label shown in the drag-over tab, e.g. the region name ("Nội dung"). */
  label?: string;
  /** Insertion point while dragging over: index among the children (0 = before the first). */
  slot?: number;
  className?: string;
  children?: ReactNode;
}

export function DropZone({ dragOver, label, slot, className, children }: DropZoneProps) {
  const items = Children.toArray(children);
  if (dragOver && slot !== undefined) {
    // Blueprint: an empty zone centres the slot; otherwise it sits before/after a component.
    items.splice(Math.max(0, Math.min(slot, items.length)), 0, <div key="nx-drop-slot" className="slds-drop-zone_drag__slot" />);
  }
  return (
    <div className={clsx("slds-drop-zone", dragOver && "slds-drop-zone_drag", className)}>
      {dragOver && label && (
        <div className="slds-drop-zone__label">
          <ZoneLabel icon={Blocks} name="builder">
            {label}
          </ZoneLabel>
        </div>
      )}
      {items}
    </div>
  );
}

function ZoneLabel({ icon: Glyph, name, children }: { icon: LucideIcon; name: string; children: ReactNode }) {
  return (
    <div className="slds-media slds-media_center">
      <div className="slds-media__figure">
        <span className={clsx("slds-icon_container slds-current-color", `slds-icon-utility-${name}`)}>
          <Glyph aria-hidden="true" className="slds-icon slds-icon_x-small slds-icon-text-default nx-lucide" />
        </span>
      </div>
      <div className="slds-media__body">
        <h2>{children}</h2>
      </div>
    </div>
  );
}

export interface DropZoneItemProps {
  /** Component name in the container label ("Thông tin chung"). */
  label: string;
  icon?: LucideIcon;
  /** Opens the component's properties (the label button). */
  onEdit?: () => void;
  /** Removes the component from the region (the Close action). */
  onRemove?: () => void;
  /** Spread onto the Move button — pass @dnd-kit `{ ref: setActivatorNodeRef, ...attributes, ...listeners }`. */
  moveHandle?: Partial<ComponentProps<"button">>;
  /** Forces the hover look (selected in the builder); plain :hover / :focus-within still apply. */
  hovered?: boolean;
  className?: string;
  children?: ReactNode;
}

export function DropZoneItem({ label, icon = LayoutPanelTop, onEdit, onRemove, moveHandle, hovered, className, children }: DropZoneItemProps) {
  const actions = moveHandle || onRemove;
  return (
    <div className={clsx("slds-drop-zone__container", hovered && "slds-is-hovered", className)} tabIndex={-1}>
      <div className="slds-drop-zone__label slds-drop-zone__label_container">
        <ZoneLabel icon={icon} name="connected_apps">
          <button type="button" className="slds-drop-zone__label_button slds-button_reset" onClick={onEdit}>
            <span className="slds-assistive-text">{t("slds.dropZone.edit")}</span>
            <span>{label}</span>
          </button>
        </ZoneLabel>
      </div>
      {children}
      {actions && (
        <div className="slds-drop-zone__actions">
          <div className="slds-button-group" role="group">
            {moveHandle && (
              <ButtonIcon icon={GripVertical} label={t("slds.dropZone.move")} variant="brand" size="x-small" {...moveHandle} />
            )}
            {onRemove && <ButtonIcon icon={X} label={t("slds.dropZone.remove")} variant="brand" size="x-small" onClick={onRemove} />}
          </div>
        </div>
      )}
    </div>
  );
}
