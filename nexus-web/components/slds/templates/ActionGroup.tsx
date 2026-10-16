"use client";

// Page-level action group shared by the page templates (blueprints page-headers/object-home and
// page-headers/record-home, SLDS 2.264.1): the first actions are neutral buttons in a
// `slds-button-group-list`, the rest go into an overflow menu on a border-filled icon button:
//   ul.slds-button-group-list > li > button.slds-button.slds-button_neutral
//                             > li > div.slds-dropdown-trigger > button.slds-button_icon-border-filled
// An overflow action that opens a dialog: the menu item is unmounted before the dialog mounts, so
// focus is parked on the overflow button first — the dialog then remembers it as its opener and Esc
// returns there (modals docs), not to <body>.
// TODO(T008): swap the raw button markup for components/slds/{Button,ButtonGroup} once merged.
import { ChevronDown } from "lucide-react";
import { useEffect, useRef } from "react";
import { t } from "@/lib/i18n";
import { Menu, MenuItem } from "../Menu";

export interface PageAction {
  label: string;
  onSelect: () => void;
  disabled?: boolean;
  /** Tooltip (`title`) on the visible button, e.g. what "In" prints. */
  title?: string;
  /** Id of the panel this action opens (panels docs: aria-controls + aria-expanded on the trigger).
   *  Only a visible button carries it; an overflow menu item cannot. */
  controls?: string;
  /** Whether that panel is showing this action's content. */
  expanded?: boolean;
}

// One visible action. While it runs it may turn disabled (busy label, e.g. "Đang xuất…"); the browser
// then blurs it (focus fixup: focus falls to <body>). When it is enabled again and focus is still on
// <body>, focus comes back to it; if the user moved on meanwhile, nothing is taken.
function ActionButton({ action: a }: { action: PageAction }) {
  const ref = useRef<HTMLButtonElement>(null);
  const blurredWhileDisabled = useRef(false);
  useEffect(() => {
    if (a.disabled) return;
    if (blurredWhileDisabled.current && document.activeElement === document.body) ref.current?.focus();
    blurredWhileDisabled.current = false;
  }, [a.disabled]);
  return (
    <button
      ref={ref}
      type="button"
      className="slds-button slds-button_neutral"
      title={a.title}
      disabled={a.disabled}
      aria-controls={a.controls}
      aria-expanded={a.controls ? !!a.expanded : undefined}
      onClick={a.onSelect}
      onBlur={(e) => {
        blurredWhileDisabled.current = e.currentTarget.disabled;
      }}
    >
      {a.label}
    </button>
  );
}

export function ActionGroup({ actions, maxVisible }: { actions: PageAction[]; maxVisible: number }) {
  const visible = actions.slice(0, maxVisible);
  const overflow = actions.slice(maxVisible);
  const more = t("slds.templates.moreActions");
  const moreRef = useRef<HTMLButtonElement>(null);
  return (
    <ul className="slds-button-group-list">
      {/* Keyed by position, not label: a busy label ("Xuất CSV" → "Đang xuất…") must not remount the
          button, or keyboard focus drops to <body> at the start and the end of the action. */}
      {visible.map((a, i) => (
        <li key={i}>
          <ActionButton action={a} />
        </li>
      ))}
      {overflow.length > 0 && (
        <li>
          <Menu
            align="right"
            trigger={
              <button ref={moreRef} type="button" className="slds-button slds-button_icon slds-button_icon-border-filled" title={more}>
                <ChevronDown aria-hidden="true" className="slds-button__icon nx-lucide" />
                <span className="slds-assistive-text">{more}</span>
              </button>
            }
          >
            {overflow.map((a) => (
              <MenuItem
                key={a.label}
                label={a.label}
                disabled={a.disabled}
                onSelect={() => {
                  moreRef.current?.focus();
                  a.onSelect();
                }}
              />
            ))}
          </Menu>
        </li>
      )}
    </ul>
  );
}
