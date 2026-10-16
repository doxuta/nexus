"use client";

// SLDS dueling picklist (blueprint `dueling-picklist`, SLDS 2.264.1):
//   div.slds-form-element[role=group][aria-labelledby] > span.slds-form-element__label.slds-form-element__legend
//     + div.slds-form-element__control > div.slds-dueling-list
//       > div.slds-assistive-text[aria-live=assertive] (drag live region) + div.slds-assistive-text (instructions)
//       > div.slds-dueling-list__column (source) > span.slds-form-element__label + div.slds-dueling-list__options
//           > ul.slds-listbox.slds-listbox_vertical[role=listbox][aria-multiselectable]
//             > li.slds-listbox__item[role=presentation] > div.slds-listbox__option…[role=option][aria-selected]
//       > div.slds-dueling-list__column (→ / ← buttons) > column (selected) > column (↑ / ↓, reorder only)
// Keyboard (blueprint docs, "second multi-select model"): ↑/↓ move focus + selection; Shift+↑/↓ extend;
// Ctrl+↑/↓ move focus only; Ctrl+Space toggles; Ctrl+A selects all; Cmd/Ctrl+→/← move the selection
// across (focus follows the item); Space toggles drag mode in the selected list, where ↑/↓ reorder.
// One tab stop per list (roving tabindex). Mouse reordering is @dnd-kit/sortable, pointer sensor only —
// its keyboard sensor and ARIA attributes are left out because the blueprint keyboard model above owns
// the keys and the listbox semantics.
// Deviations: `draggable` is only set on options that can actually be dragged (the selected list with
// reorder on) — the blueprint marks source options draggable too, but reordering is within a list.
import {
  DndContext,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from "@dnd-kit/core";
import { SortableContext, arrayMove, useSortable, verticalListSortingStrategy } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import clsx from "clsx";
import { ChevronDown, ChevronLeft, ChevronRight, ChevronUp, Lock } from "lucide-react";
import { useEffect, useId, useRef, useState, type KeyboardEvent, type MouseEvent, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { ButtonIcon } from "./ButtonIcon";
import { fill } from "./DataTable";
import { RequiredMark, describedBy } from "./FormElement";

export interface DuelingOption {
  value: string;
  label: string;
  /** Selected-list item that can be neither removed nor reordered (lock icon, aria-disabled). */
  locked?: boolean;
}

export interface DuelingPicklistProps {
  /** Group label (legend). */
  label: string;
  options: DuelingOption[];
  /** Chosen values, in order. */
  value: string[];
  onChange: (value: string[]) => void;
  /** Column labels; default "Có sẵn" / "Đã chọn". */
  sourceLabel?: string;
  selectedLabel?: string;
  /** Up/down column + drag reordering of the selected list (default true). */
  reorder?: boolean;
  /** Lists take the available width (`slds-dueling-list__column_responsive`). */
  responsive?: boolean;
  disabled?: boolean;
  required?: boolean;
  /** View mode: the label and the chosen labels as static text. */
  readOnly?: boolean;
  error?: string;
  /** Slot after the group label (help button + tooltip). */
  fieldLevelHelp?: ReactNode;
  className?: string;
}

type ListKey = "source" | "target";

// dnd-kit's own live region speaks English and would double ours: the blueprint live region above
// announces every move (keyboard and pointer), so dnd-kit stays silent.
const SILENT = {
  announcements: { onDragStart: () => undefined, onDragOver: () => undefined, onDragEnd: () => undefined, onDragCancel: () => undefined },
  screenReaderInstructions: { draggable: "" },
};

/** Moves every selected, unlocked item one step (`dir` -1 up / 1 down); a selected block moves as one
 *  and stops at the list edge or a locked neighbour. */
export function moveWithin(list: string[], selected: Set<string>, dir: -1 | 1, locked: Set<string> = new Set()): string[] {
  const out = [...list];
  const order = out.map((_, i) => i);
  if (dir > 0) order.reverse();
  for (const i of order) {
    const j = i + dir;
    if (!selected.has(out[i]) || locked.has(out[i]) || j < 0 || j >= out.length) continue;
    if (selected.has(out[j]) || locked.has(out[j])) continue;
    [out[i], out[j]] = [out[j], out[i]];
  }
  return out;
}

export function DuelingPicklist({
  label,
  options,
  value,
  onChange,
  sourceLabel = t("slds.dueling.source"),
  selectedLabel = t("slds.dueling.selected"),
  reorder = true,
  responsive,
  disabled,
  required,
  readOnly,
  error,
  fieldLevelHelp,
  className,
}: DuelingPicklistProps) {
  const id = useId();
  const byValue = new Map(options.map((o) => [o.value, o]));
  const lists: Record<ListKey, DuelingOption[]> = {
    source: options.filter((o) => !value.includes(o.value)),
    target: value.flatMap((v) => byValue.get(v) ?? []),
  };
  const locked = new Set(options.filter((o) => o.locked).map((o) => o.value));
  const listLabel: Record<ListKey, string> = { source: sourceLabel, target: selectedLabel };

  const [sel, setSel] = useState<Record<ListKey, string[]>>({ source: [], target: [] });
  const [focus, setFocus] = useState<Record<ListKey, string | null>>({ source: null, target: null });
  const [grabbed, setGrabbed] = useState(false);
  const [live, setLive] = useState("");
  const refs = useRef(new Map<string, HTMLElement>());
  const pendingFocus = useRef<string | null>(null);
  const pointerDown = useRef(false);
  useEffect(() => {
    if (!pendingFocus.current) return;
    refs.current.get(pendingFocus.current)?.focus();
    pendingFocus.current = null;
  });

  const sensors = useSensors(useSensor(PointerSensor, { activationConstraint: { distance: 4 } }));
  const labelOf = (v: string) => byValue.get(v)?.label ?? v;
  const names = (vs: string[]) => vs.map(labelOf).join(", ");
  const position = (v: string, list: string[]) =>
    fill(t("slds.dueling.position"), { item: labelOf(v), pos: list.indexOf(v) + 1, n: list.length });
  // Selected values of a list, in list order.
  const picked = (list: ListKey) => lists[list].filter((o) => sel[list].includes(o.value)).map((o) => o.value);

  if (readOnly) {
    return (
      <div className={clsx("slds-dueling-list", className)}>
        <div className="slds-form-element">
          <span className="slds-form-element__label">{label}</span>
          <div className="slds-form-element__control">
            <span className="slds-form-element__static">{names(value)}</span>
          </div>
        </div>
      </div>
    );
  }

  const focusItem = (list: ListKey, v: string) => {
    setFocus((f) => ({ ...f, [list]: v }));
    pendingFocus.current = v;
  };

  /** Moves the list's selection to the other list. `follow` = keyboard shortcut: the moved items stay
   *  selected (after any selection already there) and focus follows; the buttons leave them unselected. */
  const moveAcross = (from: ListKey, follow: boolean) => {
    const to: ListKey = from === "source" ? "target" : "source";
    const moving = picked(from).filter((v) => !locked.has(v));
    if (!moving.length) return;
    onChange(from === "source" ? [...value, ...moving] : value.filter((v) => !moving.includes(v)));
    const focused = focus[from];
    setSel((s) => ({ [from]: s[from].filter((v) => !moving.includes(v)), [to]: follow ? [...s[to], ...moving] : s[to] }) as Record<ListKey, string[]>);
    setFocus((f) => ({ ...f, [from]: null }));
    // The grabbed item may be the one leaving: drag mode ends with the move.
    setGrabbed(false);
    if (follow) focusItem(to, focused && moving.includes(focused) ? focused : moving[0]);
    setLive(fill(t("slds.dueling.movedTo"), { items: names(moving), list: listLabel[to] }));
  };

  const reorderBy = (dir: -1 | 1) => {
    const next = moveWithin(value, new Set(sel.target), dir, locked);
    if (next.join() === value.join()) return;
    onChange(next);
    const lead = picked("target").find((v) => !locked.has(v));
    if (lead) setLive(position(lead, next));
    return next;
  };

  // Grabbing needs a movable lead item; dropping always ends drag mode (announced when a lead is left).
  const grab = (on: boolean, lead = picked("target").find((v) => !locked.has(v))) => {
    const movable = !!lead && !locked.has(lead);
    if (on && !movable) return;
    setGrabbed(on);
    if (!movable) return;
    setLive(on ? `${position(lead, value)} ${t("slds.dueling.grabHint")}` : fill(t("slds.dueling.dropped"), { item: labelOf(lead), pos: value.indexOf(lead) + 1, n: value.length }));
  };

  const onKeyDown = (list: ListKey, e: KeyboardEvent) => {
    const items = lists[list].map((o) => o.value);
    const current = focus[list] ?? items[0];
    if (!current) return;
    const at = items.indexOf(current);
    const mod = e.ctrlKey || e.metaKey;
    const key = e.key;
    if (key === "ArrowUp" || key === "ArrowDown") {
      e.preventDefault();
      const dir = key === "ArrowUp" ? -1 : 1;
      if (grabbed && list === "target") {
        if (reorderBy(dir)) pendingFocus.current = current;
        return;
      }
      const next = items[at + dir];
      if (!next) return;
      focusItem(list, next);
      if (e.shiftKey) setSel((s) => ({ ...s, [list]: s[list].includes(next) ? s[list] : [...s[list], next] }));
      else if (!mod) setSel((s) => ({ ...s, [list]: [next] }));
    } else if ((key === "ArrowRight" && list === "source") || (key === "ArrowLeft" && list === "target")) {
      if (!mod) return;
      e.preventDefault();
      moveAcross(list, true);
    } else if (key === " ") {
      e.preventDefault();
      if (mod) {
        setSel((s) => ({ ...s, [list]: s[list].includes(current) ? s[list].filter((v) => v !== current) : [...s[list], current] }));
      } else if (list === "target" && reorder) {
        // Nothing selected yet: Space grabs the focused item.
        if (!grabbed && !sel.target.length) {
          setSel((s) => ({ ...s, target: [current] }));
          grab(true, current);
        } else grab(!grabbed);
      }
    } else if (key === "Escape" && grabbed) {
      e.preventDefault();
      grab(false);
    } else if (mod && key.toLowerCase() === "a") {
      e.preventDefault();
      setSel((s) => ({ ...s, [list]: items }));
    }
  };

  const onDragEnd = ({ active, over }: DragEndEvent) => {
    const from = value.indexOf(String(active.id));
    const to = over ? value.indexOf(String(over.id)) : -1;
    if (from < 0 || to < 0 || from === to || locked.has(value[to])) return;
    const next = arrayMove(value, from, to);
    onChange(next);
    setLive(fill(t("slds.dueling.dropped"), { item: labelOf(value[from]), pos: to + 1, n: next.length }));
  };

  const instructionsId = `${id}-instructions`;
  const errorId = `${id}-error`;
  const renderList = (list: ListKey) => {
    const items = lists[list];
    const tabStop = focus[list] && items.some((o) => o.value === focus[list]) ? focus[list] : items[0]?.value;
    const sortable = list === "target" && reorder && !disabled;
    const ul = (
      <ul
        aria-describedby={describedBy(instructionsId, error && errorId)}
        aria-invalid={error ? true : undefined}
        aria-labelledby={`${id}-${list}`}
        aria-multiselectable="true"
        aria-disabled={disabled || undefined}
        className="slds-listbox slds-listbox_vertical"
        role="listbox"
        onKeyDown={disabled ? undefined : (e) => onKeyDown(list, e)}
        onBlur={(e) => {
          // Leaving the list drops a grabbed item.
          if (!grabbed) return;
          const ul = e.currentTarget;
          if (e.relatedTarget) {
            if (!ul.contains(e.relatedTarget as Node)) grab(false);
            return;
          }
          // No relatedTarget: a click on the page background, or a DOM move during reorder — the latter
          // refocuses the item in the same commit, so check where focus sits once it has settled.
          setTimeout(() => {
            if (!ul.contains(document.activeElement)) grab(false);
          });
        }}
      >
        {items.map((o) => (
          <Option
            key={o.value}
            option={o}
            sortable={sortable && !o.locked}
            selected={sel[list].includes(o.value)}
            grabbed={grabbed && list === "target" && sel.target.includes(o.value) && !o.locked}
            tabIndex={!disabled && o.value === tabStop ? 0 : -1}
            lockedText={fill(t("slds.dueling.lockedHint"), { list: listLabel[list] })}
            refCallback={(node) => {
              if (node) refs.current.set(o.value, node);
              else refs.current.delete(o.value);
            }}
            onPointerDown={() => {
              pointerDown.current = true;
            }}
            onFocus={() => {
              setFocus((f) => ({ ...f, [list]: o.value }));
              // Blueprint: an item that takes focus becomes selected — when nothing is selected yet.
              if (!pointerDown.current && !sel[list].length) setSel((s) => ({ ...s, [list]: [o.value] }));
              pointerDown.current = false;
            }}
            onClick={(e) => {
              if (disabled) return;
              if (grabbed) grab(false);
              const mod = e.ctrlKey || e.metaKey;
              setSel((s) => ({
                ...s,
                [list]: mod
                  ? s[list].includes(o.value)
                    ? s[list].filter((v) => v !== o.value)
                    : [...s[list], o.value]
                  : e.shiftKey
                    ? [...s[list].filter((v) => v !== o.value), o.value]
                    : [o.value],
              }));
            }}
          />
        ))}
      </ul>
    );
    return (
      <div className={clsx("slds-dueling-list__column", responsive && "slds-dueling-list__column_responsive")}>
        <span className="slds-form-element__label" id={`${id}-${list}`}>
          {listLabel[list]}
        </span>
        <div className={clsx("slds-dueling-list__options", disabled && "slds-is-disabled")} tabIndex={disabled ? -1 : undefined}>
          {sortable ? (
            <SortableContext items={value} strategy={verticalListSortingStrategy}>
              {ul}
            </SortableContext>
          ) : (
            ul
          )}
        </div>
      </div>
    );
  };

  const moveTo = (to: ListKey) => fill(t("slds.dueling.moveTo"), { list: listLabel[to] });

  return (
    <div className={clsx("slds-form-element", error && "slds-has-error", className)} role="group" aria-labelledby={`${id}-label`}>
      <span id={`${id}-label`} className="slds-form-element__label slds-form-element__legend">
        {required && <RequiredMark />}
        {label}
      </span>
      {fieldLevelHelp && <div className="slds-form-element__icon">{fieldLevelHelp}</div>}
      <div className="slds-form-element__control">
        <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onDragEnd} accessibility={SILENT}>
          <div className="slds-dueling-list">
            <div className="slds-assistive-text" aria-live="assertive">
              {live}
            </div>
            <div className="slds-assistive-text" id={instructionsId}>
              {grabbed ? "" : t("slds.dueling.instructions")}
            </div>
            {renderList("source")}
            <div className="slds-dueling-list__column">
              <ButtonIcon icon={ChevronRight} label={moveTo("target")} variant="container" disabled={disabled} onClick={() => moveAcross("source", false)} />
              <ButtonIcon icon={ChevronLeft} label={moveTo("source")} variant="container" disabled={disabled} onClick={() => moveAcross("target", false)} />
            </div>
            {renderList("target")}
            {reorder && (
              <div className="slds-dueling-list__column">
                <ButtonIcon icon={ChevronUp} label={t("slds.dueling.moveUp")} variant="container" disabled={disabled} onClick={() => reorderBy(-1)} />
                <ButtonIcon icon={ChevronDown} label={t("slds.dueling.moveDown")} variant="container" disabled={disabled} onClick={() => reorderBy(1)} />
              </div>
            )}
          </div>
        </DndContext>
      </div>
      {error && (
        <div className="slds-form-element__help" id={errorId}>
          {error}
        </div>
      )}
    </div>
  );
}

interface OptionProps {
  option: DuelingOption;
  sortable: boolean;
  selected: boolean;
  grabbed: boolean;
  tabIndex: number;
  lockedText: string;
  refCallback: (node: HTMLElement | null) => void;
  onPointerDown: () => void;
  onFocus: () => void;
  onClick: (e: MouseEvent) => void;
}

function Option({ option: o, sortable, selected, grabbed, tabIndex, lockedText, refCallback, onPointerDown, onFocus, onClick }: OptionProps) {
  const { setNodeRef, listeners, transform, transition, isDragging } = useSortable({ id: o.value, disabled: !sortable });
  return (
    <li role="presentation" className="slds-listbox__item">
      <div
        ref={(node) => {
          setNodeRef(node);
          refCallback(node);
        }}
        className={clsx(
          "slds-listbox__option slds-listbox__option_plain slds-media slds-media_small slds-media_inline",
          (grabbed || isDragging) && "slds-is-grabbed",
          selected && "slds-is-selected",
        )}
        aria-selected={selected}
        aria-disabled={o.locked || undefined}
        draggable={sortable || undefined}
        role="option"
        tabIndex={tabIndex}
        style={transform ? { transform: CSS.Translate.toString(transform), transition } : undefined}
        {...listeners}
        onPointerDown={(e) => {
          onPointerDown();
          listeners?.onPointerDown?.(e);
        }}
        onFocus={onFocus}
        onClick={onClick}
      >
        <span className="slds-media__body">
          <span className="slds-truncate" title={o.label}>
            {o.label}
          </span>
        </span>
        {o.locked && (
          <span className="slds-media__figure slds-media__figure_reverse">
            <span className="slds-icon_container slds-current-color" title={t("slds.dueling.locked")}>
              <Lock aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
              <span className="slds-assistive-text">{lockedText}</span>
            </span>
          </span>
        )}
      </div>
    </li>
  );
}
