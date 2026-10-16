"use client";

// SLDS accordion (blueprint `accordion/base`, SLDS 2.264.1):
//   ul.slds-accordion > li.slds-accordion__list-item > section.slds-accordion__section[.slds-is-open]
//     > div.slds-accordion__summary > h2.slds-accordion__summary-heading
//         > button.slds-button.slds-button_reset.slds-accordion__summary-action[aria-expanded][aria-controls][title]
//           > svg.slds-accordion__summary-action-icon + span.slds-accordion__summary-content
//       [+ actions, e.g. <Menu> — blueprint "Has Action Overflow Menu"]
//     + div.slds-accordion__content[hidden]
// The sprite `switch` glyph is Lucide ChevronDown (SLDS rotates it while closed).
// Keyboard (docs → WAI-ARIA accordion practices): Enter/Space are the native button's; Up/Down move
// between this accordion's own headers (wrapping), Home/End jump to the first/last. A nested
// accordion keeps its own list, so arrows never cross levels.
// One section open at a time by default (lightning-accordion); `multiple` lets several stay open.
import clsx from "clsx";
import { ChevronDown } from "lucide-react";
import { useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

export interface AccordionSection {
  id: string;
  summary: string;
  content: ReactNode;
  /** Rendered after the heading inside the summary row (overflow menu). */
  actions?: ReactNode;
}

export interface AccordionProps {
  sections: AccordionSection[];
  /** Let several sections stay open (lightning-accordion allow-multiple-sections-open). */
  multiple?: boolean;
  /** Controlled open section ids. */
  open?: string[];
  /** Uncontrolled initial open ids; default = the first section (blueprint default). */
  defaultOpen?: string[];
  onOpenChange?: (open: string[]) => void;
  className?: string;
}

const MOVE: Record<string, (i: number, n: number) => number> = {
  ArrowDown: (i, n) => (i + 1) % n,
  ArrowUp: (i, n) => (i - 1 + n) % n,
  Home: () => 0,
  End: (_, n) => n - 1,
};

export function Accordion({ sections, multiple, open, defaultOpen, onOpenChange, className }: AccordionProps) {
  const baseId = useId();
  const [innerOpen, setInnerOpen] = useState<string[]>(defaultOpen ?? sections.slice(0, 1).map((s) => s.id));
  const openIds = open ?? innerOpen;
  const buttons = useRef<(HTMLButtonElement | null)[]>([]);

  const toggle = (id: string) => {
    const isOpen = openIds.includes(id);
    const next = isOpen ? openIds.filter((x) => x !== id) : multiple ? [...openIds, id] : [id];
    setInnerOpen(next);
    onOpenChange?.(next);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLButtonElement>, i: number) => {
    const move = MOVE[e.key];
    if (!move) return;
    e.preventDefault();
    buttons.current[move(i, sections.length)]?.focus();
  };

  return (
    <ul className={clsx("slds-accordion", className)}>
      {sections.map((s, i) => {
        const isOpen = openIds.includes(s.id);
        const contentId = `${baseId}-${i}`;
        return (
          <li key={s.id} className="slds-accordion__list-item">
            <section className={clsx("slds-accordion__section", isOpen && "slds-is-open")}>
              <div className="slds-accordion__summary">
                <h2 className="slds-accordion__summary-heading">
                  <button
                    ref={(el) => {
                      buttons.current[i] = el;
                    }}
                    type="button"
                    className="slds-button slds-button_reset slds-accordion__summary-action"
                    aria-controls={contentId}
                    aria-expanded={isOpen}
                    title={s.summary}
                    onClick={() => toggle(s.id)}
                    onKeyDown={(e) => onKeyDown(e, i)}
                  >
                    <ChevronDown aria-hidden="true" className="slds-accordion__summary-action-icon slds-button__icon slds-button__icon_left nx-lucide" />
                    <span className="slds-accordion__summary-content">{s.summary}</span>
                  </button>
                </h2>
                {s.actions}
              </div>
              <div className="slds-accordion__content" id={contentId} hidden={!isOpen}>
                {s.content}
              </div>
            </section>
          </li>
        );
      })}
    </ul>
  );
}
