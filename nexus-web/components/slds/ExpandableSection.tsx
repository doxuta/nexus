"use client";

// SLDS expandable section (blueprint `expandable-section/base`, SLDS 2.264.1):
//   div.slds-section[.slds-is-open] > h3.slds-section__title > button.slds-section__title-action[aria-expanded][aria-controls]
//     > svg.slds-section__title-action-icon + span.slds-truncate[title]
//   + div.slds-section__content[hidden]
// The sprite `switch` glyph (a down chevron SLDS rotates -90° while closed) is Lucide ChevronDown.
// Non-collapsible variant: shaded h3 with a plain span, always open. Enter/Space are the native
// button's — no key handling needed (docs: "The button should behave as a normal button").
import clsx from "clsx";
import { ChevronDown } from "lucide-react";
import { useId, useState, type ReactNode } from "react";

export interface ExpandableSectionProps {
  title: string;
  children: ReactNode;
  /** false = blueprint "Non-collapsable": shaded title, no button, always open. Default true. */
  collapsible?: boolean;
  open?: boolean;
  /** Uncontrolled initial state. Default true (blueprint default). */
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  className?: string;
}

export function ExpandableSection({
  title,
  children,
  collapsible = true,
  open,
  defaultOpen = true,
  onOpenChange,
  className,
}: ExpandableSectionProps) {
  const id = useId();
  const [innerOpen, setInnerOpen] = useState(defaultOpen);
  const isOpen = !collapsible || (open ?? innerOpen);
  const toggle = () => {
    setInnerOpen(!isOpen);
    onOpenChange?.(!isOpen);
  };
  if (!collapsible) {
    return (
      <div className={clsx("slds-section slds-is-open", className)}>
        <h3 className="slds-section__title slds-theme_shade">
          <span className="slds-truncate slds-p-horizontal_small" title={title}>
            {title}
          </span>
        </h3>
        <div className="slds-section__content">{children}</div>
      </div>
    );
  }
  return (
    <div className={clsx("slds-section", isOpen && "slds-is-open", className)}>
      <h3 className="slds-section__title">
        <button type="button" aria-controls={id} aria-expanded={isOpen} className="slds-button slds-section__title-action" onClick={toggle}>
          <ChevronDown aria-hidden="true" className="slds-section__title-action-icon slds-button__icon slds-button__icon_left nx-lucide" />
          <span className="slds-truncate" title={title}>
            {title}
          </span>
        </button>
      </h3>
      <div className="slds-section__content" id={id} hidden={!isOpen}>
        {children}
      </div>
    </div>
  );
}
