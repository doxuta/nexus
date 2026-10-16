"use client";

// SLDS pill (blueprint `pills` base + listbox-of-pill-options, SLDS 2.264.1).
//   linked (href):  <span class="slds-pill slds-pill_link">[icon]<a class="slds-pill__action" title>
//                     <span class="slds-pill__label"/></a><button class="slds-button slds-button_icon slds-pill__remove"/></span>
//   unlinked:       <span class="slds-pill">[icon]<span class="slds-pill__label" title/>
//                     <span class="slds-icon_container slds-pill__remove" title><button …/></span></span>
// PillContainer: base "Pill with Container" (div.slds-pill_container) or the listbox of pill options
// (div.slds-pill_container > ul.slds-listbox.slds-listbox_horizontal > li.slds-listbox-item), or the same
// listbox as a combobox/lookup selection group (div.slds-listbox_selection-group > ul…, blueprint combobox).
// Listbox keyboard, per the pills docs: one pill in the tab order, Left/Right move focus and wrap,
// Delete/Backspace removes, and focus then goes to the next pill (the previous one when the last was
// removed; the associated input or the listbox itself when none are left). No headless lib covers this.
import clsx from "clsx";
import { TriangleAlert, X, type LucideIcon } from "lucide-react";
import {
  Children,
  createContext,
  isValidElement,
  useContext,
  useId,
  useLayoutEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
} from "react";
import { Avatar } from "@/components/slds/Avatar";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Icon } from "@/components/slds/Icon";
import { t } from "@/lib/i18n";

interface Roving {
  index: number;
  active: number;
  willRemove: (index: number) => void;
}
const RovingContext = createContext<Roving | null>(null);

export interface PillProps {
  label: string;
  /** Linked pill (`slds-pill_link` + `a.slds-pill__action`). Omit for the listbox-option pill. */
  href?: string;
  /** Object icon tile left of the label. */
  icon?: { glyph: LucideIcon; name: string; label: string };
  avatar?: { name: string; src?: string };
  /** Shows the remove button; also fired by Delete/Backspace while the pill has focus. */
  onRemove?: () => void;
  /** Remove verb (default "Loại bỏ"): the button's title; its assistive text is "<verb> <label>". */
  removeLabel?: string;
  /** `slds-has-error` + an error glyph in place of the icon. */
  error?: boolean;
  /** No border/background (`slds-pill_bare`). */
  bare?: boolean;
  className?: string;
}

export function Pill({ label, href, icon, avatar, onRemove, removeLabel, error, bare, className }: PillProps) {
  const roving = useContext(RovingContext);
  const tabIndex = roving ? (roving.index === roving.active ? 0 : -1) : undefined;
  const linked = href != null;
  const rmLabel = removeLabel ?? t("slds.pill.remove");
  // Inside a listbox the remove button is often the pill's only focus target, so its name must say
  // which pill it removes, and the error (announced only inside the icon) is wired to both targets.
  const errorId = useId();
  const describedBy = error ? errorId : undefined;
  const remove = onRemove
    ? () => {
        roving?.willRemove(roving.index);
        onRemove();
      }
    : undefined;

  let media: ReactNode = null;
  if (error) {
    media = (
      <span className="slds-pill__icon_container" id={errorId}>
        <Icon icon={TriangleAlert} size="medium" tone="error" label={t("slds.pill.error")} />
      </span>
    );
  } else if (icon) {
    media = linked ? (
      <span className="slds-pill__icon_container">
        <Icon icon={icon.glyph} category="standard" name={icon.name} size="medium" label={icon.label} />
      </span>
    ) : (
      <Icon icon={icon.glyph} category="standard" name={icon.name} size="medium" label={icon.label} className="slds-pill__icon_container" />
    );
  } else if (avatar) {
    media = linked ? (
      <span className="slds-pill__icon_container">
        <Avatar {...avatar} shape="circle" />
      </span>
    ) : (
      <Avatar {...avatar} size="x-small" className="slds-pill__icon_container" />
    );
  }

  const removeButton = remove && (
    <ButtonIcon
      icon={X}
      label={`${rmLabel} ${label}`}
      title={rmLabel}
      aria-describedby={describedBy}
      className="slds-pill__remove"
      tabIndex={tabIndex}
      onClick={remove}
    />
  );

  const onKeyDown = (e: KeyboardEvent) => {
    if (remove && (e.key === "Delete" || e.key === "Backspace")) {
      e.preventDefault();
      remove();
    }
  };

  return (
    <span
      className={clsx("slds-pill", linked && "slds-pill_link", bare && "slds-pill_bare", error && "slds-has-error", className)}
      onKeyDown={onKeyDown}
    >
      {media}
      {linked ? (
        <a href={href} className="slds-pill__action" title={label} tabIndex={tabIndex} aria-describedby={describedBy}>
          <span className="slds-pill__label">{label}</span>
        </a>
      ) : (
        <span className="slds-pill__label" title={label}>
          {label}
        </span>
      )}
      {removeButton &&
        (linked ? (
          removeButton
        ) : (
          <span className="slds-icon_container slds-pill__remove" title={rmLabel}>
            {removeButton}
          </span>
        ))}
    </span>
  );
}

export interface PillContainerProps {
  /** "plain" = base "Pill with Container"; "listbox" = listbox of pill options (keyboard roving);
   *  "selection" = that listbox under a combobox/lookup (`slds-listbox_selection-group`). */
  variant?: "plain" | "listbox" | "selection";
  /** Listbox accessible name; default "Các mục đã chọn:". */
  label?: string;
  /** Where focus goes when the last pill is removed (e.g. the lookup/combobox input). */
  emptyFocusRef?: RefObject<HTMLElement | null>;
  className?: string;
  children: ReactNode;
}

export function PillContainer({ variant = "plain", label, emptyFocusRef, className, children }: PillContainerProps) {
  const items = Children.toArray(children);
  const count = items.length;
  const [active, setActive] = useState(0);
  const pending = useRef<{ index: number; count: number } | null>(null);
  const root = useRef<HTMLDivElement>(null);
  const list = useRef<HTMLUListElement>(null);

  const pills = () => Array.from(root.current?.querySelectorAll<HTMLElement>(".slds-pill") ?? []);
  const focusPill = (i: number) => pills()[i]?.querySelector<HTMLElement>("a, button")?.focus();
  const indexOf = (target: EventTarget) => pills().findIndex((p) => p.contains(target as Node));

  // After a removal re-renders the list, move focus to the neighbour (docs' "nearest pill" rule) —
  // but only when the removal took focus away with it. A removal that lands late (async onRemove) or a
  // cancelled one followed by an unrelated shrink must not pull focus off wherever the user now is,
  // including another pill (the stale index would point at the wrong neighbour).
  useLayoutEffect(() => {
    const p = pending.current;
    pending.current = null;
    if (!p || count >= p.count) return;
    const a = document.activeElement;
    if (a && a !== document.body && a.isConnected) return;
    if (count === 0) (emptyFocusRef?.current ?? list.current)?.focus();
    else pills()[Math.min(p.index, count - 1)]?.querySelector<HTMLElement>("a, button")?.focus();
  }, [count, emptyFocusRef]);

  if (variant === "plain") {
    return <div className={clsx("slds-pill_container", className)}>{children}</div>;
  }

  const onKeyDown = (e: KeyboardEvent) => {
    if (e.key !== "ArrowRight" && e.key !== "ArrowLeft") return;
    const i = indexOf(e.target);
    if (i < 0) return;
    e.preventDefault();
    focusPill((i + (e.key === "ArrowRight" ? 1 : -1) + count) % count);
  };

  const current = Math.min(active, Math.max(0, count - 1));
  return (
    <div
      ref={root}
      className={clsx(variant === "selection" ? "slds-listbox_selection-group" : "slds-pill_container", className)}
      onKeyDown={onKeyDown}
      onFocus={(e) => {
        const i = indexOf(e.target);
        if (i >= 0) setActive(i);
      }}
    >
      <ul ref={list} className="slds-listbox slds-listbox_horizontal" aria-label={label ?? t("slds.pill.selectedOptions")} tabIndex={-1}>
        {items.map((child, i) => (
          <li key={isValidElement(child) && child.key != null ? child.key : i} className="slds-listbox-item">
            <RovingContext.Provider
              value={{ index: i, active: current, willRemove: (index) => (pending.current = { index, count }) }}
            >
              {child}
            </RovingContext.Provider>
          </li>
        ))}
      </ul>
    </div>
  );
}
