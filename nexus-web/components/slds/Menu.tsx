"use client";

// SLDS dropdown menu (blueprint `menus/dropdown`, SLDS 2.264.1) on @radix-ui/react-dropdown-menu.
// Radix owns open/close, roving focus, arrow keys, typeahead, Esc and focus return; its parts are
// rendered `asChild` so the DOM is the blueprint's:
//   div.slds-dropdown-trigger.slds-dropdown-trigger_click.slds-is-open > button + div.slds-dropdown > ul.slds-dropdown__list > li.slds-dropdown__item > a[role=menuitem] > span[title]
// Deviations, both forced by the behaviour lib:
// - role="menu" sits on div.slds-dropdown (the element Radix focuses and names from the trigger),
//   so ul.slds-dropdown__list is role="presentation" instead of carrying the role itself.
// - The content is NOT portaled: it stays inside the trigger wrapper like the blueprint, which keeps
//   it inside an open Modal's stacking context (.slds-modal is z-index 9001, a portaled
//   .slds-dropdown would be 7000 behind it). Radix positions it with `position:fixed`, so it still
//   escapes overflow clipping; `static` cancels SLDS's `position:absolute` so the popper wrapper can
//   measure it.
// - Tab: Radix swallows it inside the menu; SLDS closes the menu and moves on (menus docs), so Tab
//   closes here and focus goes to the next focusable after the menu (Shift+Tab: back to the trigger).
// - Choosing an item: focus goes back to the trigger BEFORE the item's action renders, not after the
//   menu unmounts. An action that opens a Panel or a Modal then sees the trigger as the element to
//   return focus to (panels/base + modals/base docs) instead of a menu item that is about to vanish,
//   and Radix's own late return-to-trigger is skipped so it cannot pull focus back out of them.
import * as DropdownMenu from "@radix-ui/react-dropdown-menu";
import clsx from "clsx";
import { Check, type LucideIcon } from "lucide-react";
import { createContext, useContext, useId, useRef, useState, type ReactElement, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { focusNextAfter } from "./focus";
import { GLYPH, VARIANT_LABEL } from "./Toast";

export interface MenuProps {
  /** The element that opens the menu (usually an SLDS icon button); rendered as-is. It also names
   *  the menu (Radix sets aria-labelledby to it), so it must have an accessible name. */
  trigger: ReactElement;
  /** `slds-dropdown_left` (default, left edges aligned) or `slds-dropdown_right`. */
  align?: "left" | "right";
  /** `slds-dropdown_small`: 15rem minimum width. */
  size?: "small";
  /** `slds-dropdown_length-N`: caps the list at N items tall and scrolls the rest. */
  length?: 5 | 7 | 10;
  /** Buttons placed before the trigger in one `div.slds-button-group`, so their borders join
   *  (global-header favorites: ☆ toggle + ▾ trigger). */
  leading?: ReactNode;
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  className?: string;
  children: ReactNode;
}

/** Set by Menu: MenuItem calls it once its action has run (and was not cancelled). */
const ItemChosen = createContext<(() => void) | null>(null);

export function Menu({ trigger, align = "left", size, length, leading, open, defaultOpen = false, onOpenChange, className, children }: MenuProps) {
  // slds-is-open on the wrapper is what un-hides .slds-dropdown, so the open state is needed here.
  const [innerOpen, setInnerOpen] = useState(defaultOpen);
  const isOpen = open ?? innerOpen;
  const setOpen = (next: boolean) => {
    setInnerOpen(next);
    onOpenChange?.(next);
  };
  const wrapperRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const tabbedOut = useRef(false);
  const chosen = useRef(false);
  // Runs inside Radix's flushSync'd select event, so it lands before the action's render commits.
  // An action that already moved focus out of the menu (sync .focus()) keeps it there.
  const onItemChosen = () => {
    chosen.current = true;
    if (wrapperRef.current?.contains(document.activeElement)) triggerRef.current?.focus();
  };
  const triggerEl = <DropdownMenu.Trigger asChild ref={triggerRef}>{trigger}</DropdownMenu.Trigger>;
  return (
    <DropdownMenu.Root open={isOpen} onOpenChange={setOpen} modal={false}>
      <div ref={wrapperRef} className={clsx("slds-dropdown-trigger slds-dropdown-trigger_click", isOpen && "slds-is-open", className)}>
        {leading ? (
          <div className="slds-button-group">
            {leading}
            {triggerEl}
          </div>
        ) : (
          triggerEl
        )}
        <DropdownMenu.Content
          asChild
          side="bottom"
          align={align === "right" ? "end" : "start"}
          sideOffset={0}
          loop
          onKeyDown={(e) => {
            if (e.key !== "Tab") return;
            tabbedOut.current = !e.shiftKey;
            setOpen(false);
          }}
          onFocusOutside={(e) => {
            // The chosen item's own hand-off to the trigger (or on into a panel/modal) is not a dismissal;
            // left alone, Radix would also mark it "interacted outside" and skip its next focus return.
            if (chosen.current) e.preventDefault();
          }}
          onCloseAutoFocus={(e) => {
            // Runs after the content unmounted; preventDefault skips Radix's return-to-trigger.
            if (chosen.current) {
              chosen.current = false; // focus is on the trigger, or wherever the item's action moved it
              e.preventDefault();
              return;
            }
            if (!tabbedOut.current) return;
            tabbedOut.current = false;
            if (wrapperRef.current && focusNextAfter(wrapperRef.current)) e.preventDefault();
          }}
        >
          <div className={clsx("slds-dropdown static", align === "right" ? "slds-dropdown_right" : "slds-dropdown_left", size && `slds-dropdown_${size}`, length && `slds-dropdown_length-${length}`)}>
            <ul className="slds-dropdown__list" role="presentation">
              <ItemChosen.Provider value={onItemChosen}>{children}</ItemChosen.Provider>
            </ul>
          </div>
        </DropdownMenu.Content>
      </div>
    </DropdownMenu.Root>
  );
}

const ICON_LEFT = "slds-icon slds-icon_x-small slds-icon-text-default slds-m-right_x-small nx-lucide";
const ICON_RIGHT = "slds-icon slds-icon_x-small slds-icon-text-default slds-m-left_small slds-shrink-none nx-lucide";

export interface MenuItemProps {
  label: string;
  /** Glyph before the label ("Single Icon - Left"). */
  icon?: LucideIcon;
  /** Glyph after the label ("Single Icon - Right"). */
  iconRight?: LucideIcon;
  /** Alert item from "Menu with alert notifications": coloured row + utility icon. */
  tone?: "success" | "warning" | "error";
  disabled?: boolean;
  /** The item names what is current (e.g. the open project): announced as `aria-current`, not only drawn as a glyph. */
  current?: boolean;
  onSelect?: (event: Event) => void;
  /** Extra classes on the `li` (responsive visibility). */
  className?: string;
}

export function MenuItem({ label, icon: Glyph, iconRight: GlyphRight, tone, disabled, current, onSelect, className }: MenuItemProps) {
  const ToneGlyph = tone && GLYPH[tone];
  const itemChosen = useContext(ItemChosen);
  return (
    <li className={clsx("slds-dropdown__item", className)} role="presentation">
      {/* textValue: typeahead must not match the tone item's assistive text ("LỗiXoá"). */}
      <DropdownMenu.Item
        asChild
        textValue={label}
        disabled={disabled}
        onSelect={(e) => {
          onSelect?.(e);
          // preventDefault keeps the menu open (Radix): focus stays on the item then.
          if (!e.defaultPrevented) itemChosen?.();
        }}
      >
        <a className={clsx(tone && `slds-has-${tone}`)} aria-current={current ? "true" : undefined}>
          <span title={label}>
            {tone && ToneGlyph ? (
              <>
                <span className="slds-m-horizontal_xx-small">
                  <span className={clsx("slds-icon_container", `slds-icon-utility-${tone}`, tone === "warning" && "slds-current-color")}>
                    <ToneGlyph className="slds-icon slds-icon_x-small nx-lucide" aria-hidden="true" />
                    <span className="slds-assistive-text">{t(VARIANT_LABEL[tone])}</span>
                  </span>
                </span>
                <span>{label}</span>
              </>
            ) : (
              <>
                {Glyph && <Glyph className={ICON_LEFT} aria-hidden="true" />}
                {label}
              </>
            )}
          </span>
          {GlyphRight && <GlyphRight className={ICON_RIGHT} aria-hidden="true" />}
        </a>
      </DropdownMenu.Item>
    </li>
  );
}

export interface MenuCheckboxItemProps {
  label: string;
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
  /** Glyph after the label ("Double Icon - Left/Right"). */
  iconRight?: LucideIcon;
  disabled?: boolean;
}

/** Selectable item: the ✓ is always rendered and SLDS shows it only under `.slds-is-selected`. */
export function MenuCheckboxItem({ label, checked, onCheckedChange, iconRight: GlyphRight, disabled }: MenuCheckboxItemProps) {
  return (
    <li className={clsx("slds-dropdown__item", checked && "slds-is-selected")} role="presentation">
      <DropdownMenu.CheckboxItem asChild textValue={label} checked={checked} onCheckedChange={onCheckedChange} disabled={disabled}>
        <a>
          <span title={label}>
            <Check className={clsx(ICON_LEFT, "slds-icon_selected")} aria-hidden="true" />
            {label}
          </span>
          {GlyphRight && <GlyphRight className={ICON_RIGHT} aria-hidden="true" />}
        </a>
      </DropdownMenu.CheckboxItem>
    </li>
  );
}

/** Items under a sub-heading (`slds-dropdown__header`), announced as a named group. */
export function MenuGroup({ label, children }: { label: string; children: ReactNode }) {
  const id = useId();
  return (
    <li role="presentation">
      <DropdownMenu.Group asChild>
        <ul aria-labelledby={id}>
          <li className="slds-dropdown__header" title={label} role="presentation" id={id}>
            <span>{label}</span>
          </li>
          {children}
        </ul>
      </DropdownMenu.Group>
    </li>
  );
}

export function MenuDivider() {
  return (
    <DropdownMenu.Separator asChild>
      <li className="slds-has-divider_top-space" role="separator" />
    </DropdownMenu.Separator>
  );
}
