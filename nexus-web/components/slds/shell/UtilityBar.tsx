// SLDS docked utility bar (blueprint `docked-utility-bar/base`, SLDS 2.264.1):
//   footer.slds-utility-bar_container[aria-label] > h2.slds-assistive-text
//     + ul.slds-utility-bar > li.slds-utility-bar__item > .slds-button.slds-utility-bar__action
//         > svg.slds-button__icon.slds-button__icon_left + span.slds-utility-bar__text
// Deviations:
// - The bar is `static`: SLDS fixes it to the bottom of the viewport; here it is the last row of the
//   screen that owns it (Studio's workspace), so nothing is covered and no spacer is needed.
// - Items are links that open the utility's own screen; the blueprint's toggle buttons open a
//   docked `slds-utility-panel`, which no Nexus utility has yet (no aria-pressed without a panel).
import clsx from "clsx";
import Link from "next/link";
import type { LucideIcon } from "lucide-react";

export interface UtilityBarItem {
  key: string;
  label: string;
  icon: LucideIcon;
  href: string;
  title?: string;
}

export interface UtilityBarProps {
  label: string;
  items: UtilityBarItem[];
  className?: string;
}

export function UtilityBar({ label, items, className }: UtilityBarProps) {
  return (
    <footer className={clsx("slds-utility-bar_container", className)} aria-label={label}>
      <h2 className="slds-assistive-text">{label}</h2>
      <ul className="slds-utility-bar static">
        {items.map(({ key, label: text, icon: Glyph, href, title }) => (
          <li key={key} className="slds-utility-bar__item">
            <Link href={href} title={title} className="slds-button slds-utility-bar__action">
              <Glyph aria-hidden="true" className="slds-button__icon slds-button__icon_left nx-lucide" />
              <span className="slds-utility-bar__text">{text}</span>
            </Link>
          </li>
        ))}
      </ul>
    </footer>
  );
}
