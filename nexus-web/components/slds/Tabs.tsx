"use client";

// SLDS tabs (blueprints `tabs/base` and `scoped-tabs/base`, SLDS 2.264.1) on @radix-ui/react-tabs.
// Radix owns roving focus, arrow keys (wrap), Home/End and automatic activation; its parts render
// `asChild`, so the DOM is the blueprint's:
//   div.slds-tabs_<v> > ul.slds-tabs_<v>__nav[role=tablist] > li.slds-tabs_<v>__item[role=presentation]
//     > a.slds-tabs_<v>__link[role=tab]  ·  div.slds-tabs_<v>__content.slds-show|slds-hide[role=tabpanel]
// Every panel stays mounted (blueprint toggles slds-show/slds-hide), so form state in a hidden tab
// survives a switch. The anchors carry no href: Radix does not cancel the click, "#" would navigate.
// Radix's roving focus keeps tabindex=0 on the tablist until first focus and forwards that focus to
// the selected tab (then tabindex 0 on it, -1 on the rest) — same keyboard result as the blueprint.
// Overflow ("Thêm"): tabs past `maxVisible` go into a menu tab. Its button keeps the blueprint's
// role="tab" but sits outside Radix's roving focus, so it is reached with Tab, not the arrows.
// `maxVisible` is a fixed count, or "auto": how many fit the nav's width, measured on a hidden copy of
// the row (useFit, as the global navigation bar does) — a phone gets "Thêm", a desktop every tab.
// The copy draws every label bold (the active tab's weight) so the widest form is what must fit.
// ponytail: the overflow menu is @radix-ui/react-dropdown-menu inline with the menus/dropdown
// markup; swap to components/slds/Menu once that wrapper is merged. "auto" is read at mount only.
import * as DropdownMenu from "@radix-ui/react-dropdown-menu";
import * as RadixTabs from "@radix-ui/react-tabs";
import clsx from "clsx";
import { ChevronDown, CircleAlert, type LucideIcon } from "lucide-react";
import Link from "next/link";
import { useEffect, useRef, useState, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { useEmbedded } from "./EmbeddedSurface";
import { Icon } from "./Icon";
import { revealInScroller } from "./revealInScroller";
import { useFit } from "./useFit";

export interface TabItem {
  value: string;
  label: string;
  content: ReactNode;
  /** Decorative object icon left of the label (blueprint "overflow-icons"). */
  icon?: { glyph: LucideIcon; name: string };
  /** Assistive text of the error icon; set it when the tab's content has a validation error. */
  error?: string;
}

/** Link mode (specs/073): a row of page links, not a tablist — no panels, no roving focus. */
export interface TabLink {
  value: string;
  label: string;
  href: string;
  /** Shown instead of `label` below 768px (the row of four sub tabs must fit a phone). */
  shortLabel?: string;
  /** Work is waiting under this item: 6px dot + hidden text. */
  dot?: boolean;
}

export interface TabsLinksProps {
  mode: "links";
  items: TabLink[];
  /** `value` of the item for the page being shown (`aria-current="page"`). */
  current: string;
  /** Accessible name of the `nav` — required, a page has several. */
  label: string;
  size?: "medium" | "large";
  className?: string;
}

export interface TabsProps {
  items: TabItem[];
  variant?: "default" | "scoped";
  size?: "medium" | "large";
  value?: string;
  defaultValue?: string;
  onValueChange?: (value: string) => void;
  /** Tabs beyond this count move into the "Thêm" overflow menu; "auto" = as many as fit the width. */
  maxVisible?: number | "auto";
  /** Accessible name of the tablist. */
  label?: string;
  className?: string;
}

/** Visible tabs + overflow; an overflowed active tab takes the last visible slot. */
function split(items: TabItem[], active: string, maxVisible?: number): [TabItem[], TabItem[]] {
  if (!maxVisible || items.length <= maxVisible) return [items, []];
  const activeIndex = items.findIndex((i) => i.value === active);
  if (activeIndex < maxVisible) return [items.slice(0, maxVisible), items.slice(maxVisible)];
  return [
    [...items.slice(0, maxVisible - 1), items[activeIndex]],
    items.filter((_, i) => i >= maxVisible - 1 && i !== activeIndex),
  ];
}

function TabsList({
  items,
  variant = "default",
  size,
  value,
  defaultValue,
  onValueChange,
  maxVisible,
  label,
  className,
}: TabsProps) {
  const [inner, setInner] = useState(defaultValue ?? items[0]?.value ?? "");
  const active = value ?? inner;
  const select = (next: string) => {
    setInner(next);
    onValueChange?.(next);
  };
  const navRef = useRef<HTMLUListElement>(null);
  const ghostRef = useRef<HTMLUListElement>(null);
  const fit = useFit(navRef, ghostRef);
  // At least one: the selected tab always stays in the nav.
  const [visible, overflow] = split(items, active, maxVisible === "auto" ? Math.max(1, fit) : maxVisible);
  const p = `slds-tabs_${variant}`;
  return (
    <RadixTabs.Root asChild value={active} onValueChange={select}>
      <div className={clsx(p, size && `slds-tabs_${size}`, className)}>
        <RadixTabs.List asChild aria-label={label}>
          <ul ref={navRef} className={`${p}__nav`}>
            {visible.map((item) => (
              <li
                key={item.value}
                className={clsx(`${p}__item`, item.value === active && "slds-is-active")}
                title={item.label}
                role="presentation"
              >
                <RadixTabs.Trigger asChild value={item.value}>
                  <a className={`${p}__link`}>
                    <TabLabel item={item} />
                  </a>
                </RadixTabs.Trigger>
              </li>
            ))}
            {overflow.length > 0 && <OverflowTab prefix={p} items={overflow} onSelect={select} />}
          </ul>
        </RadixTabs.List>
        {maxVisible === "auto" && (
          // Measuring copy of the whole row (never seen, never focused); last cell = "Thêm".
          <div aria-hidden="true" className="pointer-events-none invisible absolute h-0 w-0 overflow-hidden">
            <ul ref={ghostRef} className={`${p}__nav w-max`}>
              {items.map((item) => (
                <li key={item.value} className={`${p}__item slds-is-active`}>
                  <span className={`${p}__link`}>
                    <TabLabel item={item} />
                  </span>
                </li>
              ))}
              <li className={`${p}__item ${p}__overflow-button`}>
                <span className="slds-button">
                  {t("slds.tabs.more")}
                  <ChevronDown className="slds-button__icon slds-button__icon_x-small slds-button__icon_right nx-lucide" />
                </span>
              </li>
            </ul>
          </div>
        )}
        {items.map((item) => (
          <RadixTabs.Content key={item.value} asChild value={item.value} forceMount>
            <div className={clsx(`${p}__content`, item.value === active ? "slds-show" : "slds-hide")}>{item.content}</div>
          </RadixTabs.Content>
        ))}
      </div>
    </RadixTabs.Root>
  );
}

export function Tabs(props: TabsProps | TabsLinksProps) {
  const embedded = useEmbedded();
  if ("mode" in props && props.mode === "links") return <TabsLinks {...props} />;
  return embedded ? <TabsSections {...(props as TabsProps)} /> : <TabsList {...(props as TabsProps)} />;
}

// Embedded (specs/073 FR-005): the tablist becomes scrolled sections, each under a rule with its heading.
// Every panel stays mounted (same as the tablist, so form state survives). `value`/`onValueChange` have
// nothing to switch; error markers stay on the heading.
function TabsSections({ items, label, className }: TabsProps) {
  return (
    <div role="group" aria-label={label} className={className}>
      {items.map((item) => (
        <section key={item.value} className="slds-border_top slds-p-vertical_medium">
          <h3 className="slds-text-heading_small slds-m-bottom_small">
            <TabLabel item={{ ...item, icon: undefined }} />
          </h3>
          {item.content}
        </section>
      ))}
    </div>
  );
}

function TabsLinks({ items, current, label, size, className }: TabsLinksProps) {
  const refs = useRef<Record<string, HTMLAnchorElement | null>>({});
  // Overflow is a scroll inside the list (not a "Thêm" menu): keep the current item in view.
  useEffect(() => {
    revealInScroller(refs.current[current], refs.current[current]?.closest("ul"));
  }, [current]);
  return (
    <nav aria-label={label} className={clsx("slds-tabs_default", size && `slds-tabs_${size}`, size === "medium" && "nx-tabs-zone", className)}>
      <ul className="slds-tabs_default__nav overflow-x-auto overflow-y-hidden whitespace-nowrap">
        {items.map((item) => {
          const active = item.value === current;
          return (
            <li key={item.value} className={clsx("slds-tabs_default__item shrink-0", active && "slds-is-active")} title={item.label}>
              <Link
                ref={(el) => {
                  refs.current[item.value] = el;
                }}
                href={item.href}
                className="slds-tabs_default__link"
                aria-current={active ? "page" : undefined}
              >
                {item.shortLabel ? (
                  <>
                    <span className="max-md:hidden">{item.label}</span>
                    <span className="md:hidden">{item.shortLabel}</span>
                  </>
                ) : (
                  item.label
                )}
                {item.dot && (
                  <>
                    <span aria-hidden="true" className="slds-theme_warning slds-m-left_x-small inline-block size-1.5 rounded-full align-middle" />
                    <span className="slds-assistive-text">{t("studio.a11y.zoneHasWork")}</span>
                  </>
                )}
              </Link>
            </li>
          );
        })}
      </ul>
    </nav>
  );
}

function TabLabel({ item }: { item: TabItem }) {
  return (
    <>
      {item.icon && (
        <span className="slds-tabs__left-icon">
          <Icon icon={item.icon.glyph} category="standard" name={item.icon.name} size="small" />
        </span>
      )}
      {item.label}
      {item.error && (
        <span className="slds-tabs__right-icon">
          <Icon icon={CircleAlert} name="error" size="x-small" tone="error" label={item.error} />
        </span>
      )}
    </>
  );
}

function OverflowTab({ prefix, items, onSelect }: { prefix: string; items: TabItem[]; onSelect: (value: string) => void }) {
  const [open, setOpen] = useState(false);
  const chose = useRef(false);
  const liRef = useRef<HTMLLIElement>(null);
  return (
    <li
      ref={liRef}
      className={`${prefix}__item ${prefix}__overflow-button`}
      title={t("slds.tabs.moreTitle")}
      role="presentation"
    >
      <DropdownMenu.Root open={open} onOpenChange={setOpen} modal={false}>
        <div className={clsx("slds-dropdown-trigger slds-dropdown-trigger_click", open && "slds-is-open")}>
          <DropdownMenu.Trigger asChild>
            <button className="slds-button" role="tab">
              {t("slds.tabs.more")}
              <ChevronDown aria-hidden="true" className="slds-button__icon slds-button__icon_x-small slds-button__icon_right nx-lucide" />
            </button>
          </DropdownMenu.Trigger>
          <DropdownMenu.Content
            asChild
            side="bottom"
            align="end"
            sideOffset={0}
            loop
            onCloseAutoFocus={(e) => {
              if (!chose.current) return;
              // A tab was picked: focus it (it now sits in the nav) instead of the "Thêm" button.
              chose.current = false;
              e.preventDefault();
              liRef.current?.parentElement?.querySelector<HTMLElement>("[role=tab][aria-selected=true]")?.focus();
            }}
          >
            <div className="slds-dropdown slds-dropdown_right static" aria-label={t("slds.tabs.moreTitle")}>
              <ul className="slds-dropdown__list slds-dropdown_length-with-icon-10" role="presentation">
                {items.map((item) => (
                  <li key={item.value} className="slds-dropdown__item" role="presentation">
                    <DropdownMenu.Item
                      asChild
                      onSelect={() => {
                        chose.current = true;
                        onSelect(item.value);
                      }}
                    >
                      <a>
                        <span title={item.label}>
                          {item.icon ? (
                            // Blueprint "overflow-icons": the object tile beside the label.
                            <span className="slds-media slds-media_center">
                              <span className="slds-media__figure">
                                <Icon icon={item.icon.glyph} category="standard" name={item.icon.name} size="small" />
                              </span>
                              <span className="slds-media__body">{item.label}</span>
                            </span>
                          ) : (
                            item.label
                          )}
                        </span>
                      </a>
                    </DropdownMenu.Item>
                  </li>
                ))}
              </ul>
            </div>
          </DropdownMenu.Content>
        </div>
      </DropdownMenu.Root>
    </li>
  );
}
