"use client";

// SLDS vertical tabs (blueprint `vertical-tabs/base`, SLDS 2.264.1: default, with-icons,
// with-badges) on @radix-ui/react-tabs with orientation="vertical" (ArrowUp/ArrowDown, wrap,
// automatic activation). Same Radix mapping and deviations as Tabs.tsx: anchors without href,
// every panel mounted and toggled with slds-show/slds-hide, tablist forwards first focus to the
// selected tab.
import * as RadixTabs from "@radix-ui/react-tabs";
import clsx from "clsx";
import type { LucideIcon } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Icon } from "./Icon";

export interface VerticalTabItem {
  value: string;
  label: string;
  content: ReactNode;
  /** Decorative object icon left of the label. */
  icon?: { glyph: LucideIcon; name: string };
  /** Inverse badge right of the label (blueprint "with-badges"). */
  badge?: string;
}

export interface VerticalTabsProps {
  items: VerticalTabItem[];
  value?: string;
  defaultValue?: string;
  onValueChange?: (value: string) => void;
  /** Accessible name of the tablist. */
  label?: string;
  className?: string;
}

export function VerticalTabs({ items, value, defaultValue, onValueChange, label, className }: VerticalTabsProps) {
  const [inner, setInner] = useState(defaultValue ?? items[0]?.value ?? "");
  const active = value ?? inner;
  const select = (next: string) => {
    setInner(next);
    onValueChange?.(next);
  };
  return (
    <RadixTabs.Root asChild orientation="vertical" value={active} onValueChange={select}>
      <div className={clsx("slds-vertical-tabs", className)}>
        <RadixTabs.List asChild aria-label={label}>
          <ul className="slds-vertical-tabs__nav">
            {items.map((item) => (
              <li
                key={item.value}
                className={clsx("slds-vertical-tabs__nav-item", item.value === active && "slds-is-active")}
                title={item.label}
                role="presentation"
              >
                <RadixTabs.Trigger asChild value={item.value}>
                  <a className="slds-vertical-tabs__link">
                    <span className="slds-vertical-tabs__left-icon">
                      {item.icon && <Icon icon={item.icon.glyph} category="standard" name={item.icon.name} size="small" />}
                    </span>
                    <span className="slds-truncate" title={item.label}>
                      {item.label}
                    </span>
                    <span className="slds-vertical-tabs__right-icon">
                      {item.badge && <span className="slds-badge slds-badge_inverse">{item.badge}</span>}
                    </span>
                  </a>
                </RadixTabs.Trigger>
              </li>
            ))}
          </ul>
        </RadixTabs.List>
        {items.map((item) => (
          <RadixTabs.Content key={item.value} asChild value={item.value} forceMount>
            <div className={clsx("slds-vertical-tabs__content", item.value === active ? "slds-show" : "slds-hide")}>
              {item.content}
            </div>
          </RadixTabs.Content>
        ))}
      </div>
    </RadixTabs.Root>
  );
}
