"use client";

// One cell of the form stage (specs/075 FR-011/FR-020/FR-027). The real widget is drawn inside `inert`, so it is a
// picture: never in the Tab order, never typed into. The cell itself is the only control: role="button" with a
// computed accessible name, selected = `aria-pressed`. Roving tabindex is owned by FormCanvas (one Tab stop for the
// whole stage). Hover/selected look = the SLDS drop-zone container hook (blueprint drop-zone), no chip on the cell.
// Drag (FR-027): the cell is a draggable + droppable (id = cell id); the dnd-kit keyboard/pointer listeners sit on the
// HANDLE only (setActivatorNodeRef), a sibling button, so Enter/Space on the cell keep meaning "select". The handle
// shows on hover/focus/selection and always on touch ((hover: none)). A spacer has no field: it can be dragged and
// removed but not selected.
import clsx from "clsx";
import { useDraggable, useDroppable } from "@dnd-kit/core";
import { GripVertical, X } from "lucide-react";
import type { KeyboardEvent, ReactNode, Ref } from "react";
import { t, tf } from "@/lib/i18n";

export interface CanvasCellProps {
  /** Cell id of the draft (the dnd id). */
  cellId: string;
  ariaLabel: string;
  /** Name for the handle ("Kéo {name}"). */
  dragName: string;
  selected: boolean;
  /** The one cell of the stage that is in the Tab order. */
  tabbable: boolean;
  /** A drag is in progress: the pointer over this cell shows where the dragged one would land. */
  dragOver?: boolean;
  /** viaPointer: a mouse/touch click (focus goes to the properties column); false = keyboard (focus stays here). */
  onSelect: (viaPointer: boolean) => void;
  onNavKey: (e: KeyboardEvent<HTMLDivElement>) => void;
  /** Spacer only: the "Xoá ô trống" button. */
  onRemove?: () => void;
  cellRef: Ref<HTMLDivElement>;
  /** An assistant proposal on this cell (specs/075 US9): drawn under the cell, outside the control, no control of its own. */
  ghost?: ReactNode;
  children: ReactNode;
}

export function CanvasCell({ cellId, ariaLabel, dragName, selected, tabbable, dragOver, onSelect, onNavKey, onRemove, cellRef, ghost, children }: CanvasCellProps) {
  const { attributes, listeners, setActivatorNodeRef, isDragging, setNodeRef: setDragRef } = useDraggable({ id: cellId });
  const { setNodeRef: setDropRef } = useDroppable({ id: cellId });
  // The handle's own attributes would make it role="button" tabIndex=0 on every cell: only the Tab stop keeps one.
  const { role: _role, tabIndex: _tab, ...handleAttrs } = attributes;
  void _role; void _tab;
  // SLDS paints the hover/selected frame with a `:after` overlay that covers the whole container, so the container is
  // what the pointer hits: it IS the control. Its picture (the real widget) is inert and can never be hit or focused.
  return (
    <div ref={(el) => { setDragRef(el); setDropRef(el); }} className={clsx("relative group w-full min-w-0", isDragging && "opacity-50", ghost && "nx-proposed")}>
      <div
        ref={cellRef}
        role="button"
        tabIndex={tabbable ? 0 : -1}
        aria-pressed={selected}
        aria-label={ariaLabel}
        className={clsx("slds-drop-zone__container cursor-pointer", (selected || dragOver) && "slds-is-hovered")}
        onClick={(e) => onSelect(e.detail > 0)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onSelect(false);
          } else onNavKey(e);
        }}
      >
        <div inert>{children}</div>
      </div>
      {ghost}
      <div
        className={clsx(
          "absolute top-0 right-0 flex opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 [@media(hover:none)]:opacity-100",
          (selected || isDragging) && "opacity-100",
        )}
        style={{ zIndex: 8001 }}
      >
        <button
          type="button"
          ref={setActivatorNodeRef}
          {...handleAttrs}
          {...listeners}
          tabIndex={tabbable ? 0 : -1}
          aria-label={tf("studio.build.screens.dnd.handle", { name: dragName })}
          className="slds-button slds-button_icon slds-button_icon-brand slds-button_icon-x-small"
          style={{ touchAction: "none", minWidth: 24, minHeight: 24 }}
        >
          <GripVertical aria-hidden="true" className="slds-button__icon nx-lucide" />
        </button>
        {onRemove && (
          <button
            type="button"
            tabIndex={tabbable ? 0 : -1}
            aria-label={t("studio.build.screens.spacer.remove")}
            className="slds-button slds-button_icon slds-button_icon-brand slds-button_icon-x-small"
            style={{ minWidth: 24, minHeight: 24 }}
            onClick={onRemove}
          >
            <X aria-hidden="true" className="slds-button__icon nx-lucide" />
          </button>
        )}
      </div>
    </div>
  );
}
