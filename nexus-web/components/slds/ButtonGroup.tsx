// SLDS button group (blueprint `button-groups` base / list / row, SLDS 2.264.1).
//   base: <div class="slds-button-group" role="group">…buttons…</div>
//   list: <ul class="slds-button-group-list"><li>…</li></ul>
//   row:  <ul class="slds-button-group-row"><li class="slds-button-group-item">…</li></ul>
// `overflow` is the trigger slot of the blueprint "With Overflow Menu" examples: it is wrapped in
// `div.slds-dropdown-trigger.slds-dropdown-trigger_click.slds-button_last` so SLDS joins its borders to
// the group. The menu itself (Radix DropdownMenu) is the caller's — pass the trigger, e.g.
// `<DropdownMenu.Trigger asChild><ButtonIcon more … /></DropdownMenu.Trigger>`.
import clsx from "clsx";
import { Children, isValidElement, type ReactNode } from "react";

export interface ButtonGroupProps {
  /** Default "base". "row" spaces buttons apart instead of joining them. */
  variant?: "base" | "list" | "row";
  /** Accessible name for the base group (`role="group"`). */
  label?: string;
  overflow?: ReactNode;
  className?: string;
  children: ReactNode;
}

export function ButtonGroup({ variant = "base", label, overflow, className, children }: ButtonGroupProps) {
  const last = overflow != null && (
    <div className="slds-dropdown-trigger slds-dropdown-trigger_click slds-button_last">{overflow}</div>
  );
  if (variant === "base") {
    return (
      <div className={clsx("slds-button-group", className)} role="group" aria-label={label}>
        {children}
        {last}
      </div>
    );
  }
  const itemClass = variant === "row" ? "slds-button-group-item" : undefined;
  return (
    <ul className={clsx(variant === "row" ? "slds-button-group-row" : "slds-button-group-list", className)} aria-label={label}>
      {/* Key by the child's own key (toArray gives ".0", ".2"… from the source slot, nulls included), not
          the index: a conditional button appearing must not remount its neighbours and drop focus. */}
      {Children.toArray(children).map((child, i) => (
        <li key={isValidElement(child) && child.key != null ? child.key : i} className={itemClass}>
          {child}
        </li>
      ))}
      {last && <li className={itemClass}>{last}</li>}
    </ul>
  );
}
