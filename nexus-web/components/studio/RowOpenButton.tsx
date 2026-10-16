"use client";

// The name button in a data-table row that opens that row in a docked panel. panels/base docs: the
// element that toggles a panel names it (aria-controls) and reports whether it shows (aria-expanded).
// The open row's id arrives through OpenRowId, not through the column defs: rebuilding the columns
// remounts every cell (flexRender renders cell functions as components), which would take the
// just-clicked button — the panel's focus-return target — out of the page.
import { createContext, useContext } from "react";

/** Id of the row whose content the docked panel shows (undefined: none). */
export const OpenRowId = createContext<string | undefined>(undefined);

export function RowOpenButton({ rowId, label, panelId, onOpen }: {
  rowId: string;
  label: string;
  panelId: string;
  onOpen: () => void;
}) {
  const openId = useContext(OpenRowId);
  return (
    // The span truncates: a flex button (slds-button) cannot ellipsize its own text.
    <button type="button" className="slds-button min-w-0 max-w-full" title={label}
      aria-controls={panelId} aria-expanded={openId === rowId} onClick={onOpen}>
      <span className="slds-truncate min-w-0">{label}</span>
    </button>
  );
}
