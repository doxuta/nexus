"use client";

// SLDS filter panel (blueprint `panels/filtering`, SLDS 2.264.1):
//   div.slds-panel.slds-grid.slds-grid_vertical.slds-nowrap.slds-panel_filters
//     > div.slds-form.slds-form_stacked.slds-grow.slds-scrollable_y.slds-grid.slds-grid_vertical > div.slds-filters
//       > div.slds-filters__header.slds-grid.slds-has-divider_bottom-space   (title + close, or Huỷ / Lưu)
//       > div.slds-filters__body > [alert] h3 + ol.slds-list_vertical.slds-list_vertical-space
//           > li.slds-item.slds-hint-parent > div.slds-filters__item[.slds-is-new|.slds-has-error|.slds-is-locked]
//               > button.slds-button_reset.slds-grow.slds-has-blur-focus (Sửa bộ lọc: field / operator value)
//               + button icon (remove)  [+ p error line]
//         [+ h3 "Bộ lọc bị khoá" + lock icon + ol of locked items]
//       > div.slds-filters__footer.slds-grid.slds-shrink-none > "Thêm bộ lọc" · "Xoá tất cả" (text links)
// Editing a filter opens an SLDS popover beside the item (Popover → Radix: Esc / outside click close,
// focus moves in and back) with the same field · operator · value controls as ExpressionBuilder,
// stacked. Same controlled FilterRow[] contract as FilterBuilder (lib/filterModel.ts). A row left
// without a field when its popover closes is dropped — it would never filter anything.
// Focus never falls to the page body: removing a filter focuses the next filter (else the previous,
// else "Thêm bộ lọc"); dropping an unfilled new filter returns focus to "Thêm bộ lọc".
import clsx from "clsx";
import { Lock, Trash2, X } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";
import { t } from "@/lib/i18n";
import { newFilterRow, opsForType, type FilterField, type FilterRow } from "@/lib/filterModel";
import { Button } from "./Button";
import { ButtonIcon } from "./ButtonIcon";
import { fill } from "./DataTable";
import { ConditionFields, conditionText, fieldLabel } from "./ExpressionBuilder";
import { Popover } from "./Popover";

export interface FilterPanelProps {
  fields: FilterField[];
  /** Filter rows (controlled) — the FilterBuilder contract, AND semantics. */
  value: FilterRow[];
  onChange: (rows: FilterRow[]) => void;
  opsFor?: (fieldType: string) => readonly string[];
  /** Header title (default "Bộ lọc"). */
  title?: string;
  /** Close button in the header. */
  onClose?: () => void;
  /** Unsaved edits: the header swaps title + close for Huỷ / Lưu (blueprint "New filter" / "Error"). */
  dirty?: boolean;
  onCancel?: () => void;
  onSave?: () => void;
  /** Errors keyed by row id — the item turns red with the message under it. */
  errors?: Record<string, string>;
  /** Read-only filters the user cannot edit or remove (listed under "Bộ lọc bị khoá"). */
  locked?: FilterRow[];
  className?: string;
}

export function FilterPanel({
  fields,
  value,
  onChange,
  opsFor = opsForType,
  title = t("slds.filterPanel.title"),
  onClose,
  dirty,
  onCancel,
  onSave,
  errors,
  locked = [],
  className,
}: FilterPanelProps) {
  const uid = useId();
  const [openId, setOpenId] = useState<string | null>(null);
  const byName = new Map(fields.map((f) => [f.name, f]));
  const hasErrors = value.some((r) => errors?.[r.id]);

  // Row whose edit button takes focus after the next render; "" = the "Thêm bộ lọc" link.
  const focusTarget = useRef<string | null>(null);
  const addRef = useRef<HTMLButtonElement>(null);
  const itemDom = (rowId: string) => `${uid}-filter-${rowId}`;
  useEffect(() => {
    const target = focusTarget.current;
    if (target === null) return;
    focusTarget.current = null;
    // Only when the focused control went away with its row — never steal focus the user moved elsewhere.
    if (document.activeElement && document.activeElement !== document.body) return;
    (target ? document.getElementById(itemDom(target))?.querySelector("button") : addRef.current)?.focus();
  });

  const remove = (id: string) => {
    const i = value.findIndex((r) => r.id === id);
    focusTarget.current = value[i + 1]?.id ?? value[i - 1]?.id ?? "";
    onChange(value.filter((r) => r.id !== id));
  };
  const setOpen = (id: string, open: boolean) => {
    if (open) return setOpenId(id);
    setOpenId(null);
    if (value.some((r) => r.id === id && !r.field)) {
      focusTarget.current = "";
      onChange(value.filter((r) => r.id !== id));
    }
  };
  const add = () => {
    const row = newFilterRow("", "");
    onChange([...value, row]);
    setOpenId(row.id);
  };

  const item = (row: FilterRow, isLocked: boolean) => {
    const field = byName.get(row.field);
    const name = field ? fieldLabel(field) : row.field;
    const text = row.field ? conditionText(row, field) : t("slds.filterPanel.new");
    const error = errors?.[row.id];
    const errorId = `nx-filter-error-${row.id}`;
    const edit = (
      <button type="button" className="slds-button_reset slds-grow slds-has-blur-focus" disabled={isLocked} aria-describedby={error ? errorId : undefined}>
        <span className="slds-assistive-text">{t("slds.filterPanel.edit")}</span>
        {row.field && <span className="slds-show slds-text-body_small">{name}</span>}
        <span className="slds-show">{text}</span>
      </button>
    );
    return (
      <li key={row.id} id={itemDom(row.id)} className="slds-item slds-hint-parent">
        <div
          className={clsx(
            "slds-filters__item slds-grid slds-grid_vertical-align-center",
            !row.field && "slds-is-new",
            error && "slds-has-error",
            isLocked && "slds-is-locked",
          )}
        >
          {isLocked ? (
            edit
          ) : (
            <Popover
              trigger={edit}
              title={row.field ? name : t("slds.filterPanel.new")}
              side="left"
              open={openId === row.id}
              onOpenChange={(open) => setOpen(row.id, open)}
              footer={
                <div className="slds-grid slds-grid_vertical-align-center">
                  <Button className="slds-col_bump-left" onClick={() => setOpen(row.id, false)}>
                    {t("slds.filterPanel.done")}
                  </Button>
                </div>
              }
            >
              <ConditionFields
                layout="stacked"
                row={row}
                fields={fields}
                opsFor={opsFor}
                error={error}
                onChange={(next) => onChange(value.map((r) => (r.id === next.id ? next : r)))}
              />
            </Popover>
          )}
          {!isLocked && (
            <ButtonIcon
              icon={Trash2}
              size="small"
              hint
              title={fill(t("slds.filterPanel.remove"), { text })}
              label={fill(t("slds.filterPanel.removeFilter"), { field: name, text }).replace(/\s+/g, " ").trim()}
              onClick={() => remove(row.id)}
            />
          )}
        </div>
        {error && (
          <p id={errorId} className="slds-text-color_error slds-m-top_xx-small">
            {error}
          </p>
        )}
      </li>
    );
  };

  return (
    <div className={clsx("slds-panel slds-grid slds-grid_vertical slds-nowrap slds-panel_filters", className)}>
      <div className="slds-form slds-form_stacked slds-grow slds-scrollable_y slds-grid slds-grid_vertical">
        <div className="slds-filters">
          {dirty ? (
            <div className="slds-filters__header slds-grid slds-has-divider_bottom-space slds-grid_align-spread">
              <Button onClick={onCancel}>{t("action.cancel")}</Button>
              <Button variant="brand" onClick={onSave}>
                {t("action.save")}
              </Button>
            </div>
          ) : (
            <div className="slds-filters__header slds-grid slds-has-divider_bottom-space">
              <h2 className="slds-align-middle slds-text-heading_small">{title}</h2>
              {onClose && (
                <ButtonIcon icon={X} size="small" label={t("slds.filterPanel.close")} className="slds-col_bump-left" onClick={onClose} />
              )}
            </div>
          )}
          <div className="slds-filters__body">
            {hasErrors && (
              <div className="slds-text-color_error slds-m-bottom_x-small" role="alert">
                {t("slds.filterPanel.invalid")}
              </div>
            )}
            <h3 className="slds-text-body_small slds-m-vertical_x-small">{t("slds.filterPanel.matchAll")}</h3>
            <ol className="slds-list_vertical slds-list_vertical-space">{value.map((r) => item(r, false))}</ol>
            {locked.length > 0 && (
              <>
                <h3 className="slds-text-body_small slds-m-vertical_x-small slds-grid">
                  {t("slds.filterPanel.locked")}
                  <Lock aria-hidden="true" className="slds-icon slds-icon_x-small slds-icon-text-default slds-m-left_x-small nx-lucide" />
                </h3>
                <ol className="slds-list_vertical slds-list_vertical-space">{locked.map((r) => item(r, true))}</ol>
              </>
            )}
          </div>
          <div className="slds-filters__footer slds-grid slds-shrink-none">
            <button type="button" ref={addRef} className="slds-button_reset slds-text-link" onClick={add}>
              {t("slds.filterPanel.add")}
            </button>
            <button type="button" className="slds-button_reset slds-text-link slds-col_bump-left" onClick={() => onChange([])}>
              {t("slds.filterPanel.removeAll")}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
