"use client";

// SLDS expression builder (blueprint `expression` base / custom-logic, SLDS 2.264.1):
//   div.slds-expression > h2.slds-expression__title
//     + div.slds-expression__options (match select, opt-in) + div.slds-expression__custom-logic (custom only)
//     + ul > li.slds-expression__row > fieldset > legend.slds-expression__legend (conjunction + assistive)
//              + div.slds-grid.slds-gutters_xx-small > col (field) · col.slds-grow-none (operator) · col (value)
//                · col.slds-grow-none (delete, border-filled button icon under a no-break-space label)
//          li.slds-expression__group > fieldset > legend.slds-expression__legend_group + options + ul (rows
//              with slds-expression__row_group) + div.slds-expression__buttons ("Thêm điều kiện")
//     + div.slds-expression__buttons ("Thêm điều kiện", "Thêm nhóm")
// Replaces components/ui/FilterBuilder.tsx with the same data contract (lib/filterModel.ts): a
// controlled FilterRow[] (`value`/`onChange`) using the per-field-type operator map and the same
// value encoding (in/between comma-joined, relative-date tokens, "true"/"false"), so the serialized
// ?field[op]=value query is unchanged. Match mode, custom logic and one level of groups are opt-in
// props — omit them and the builder is the AND-only row editor every current backend expects.
// Inside a react-hook-form form, bind it with <Controller> (field.value / field.onChange): the rows
// are one controlled value, so useFieldArray's own row state would only duplicate it.
// Focus (blueprint docs "Focus Management"): "Thêm điều kiện" focuses the new row's first combobox,
// "Thêm nhóm" the new group's mode combobox; deleting a row focuses the next item's first combobox, or
// the previous one's when it was the last (the "Thêm điều kiện" button once nothing is left). The
// blueprint's one-row minimum is not kept: an empty list is the FilterBuilder "no filter" value.
import clsx from "clsx";
import { Plus, Trash2 } from "lucide-react";
import { useEffect, useId, useRef, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import {
  DEFAULT_OPS,
  RELATIVE_DATE_TOKENS,
  RELATIVE_OK_OPS,
  checkCustomLogic,
  isFilterableType,
  isNullOp,
  isRelativeDateToken,
  newFilterGroup,
  newFilterRow,
  opsForType,
  filterTypeOf,
  type FilterField,
  type FilterGroup,
  type FilterMatch,
  type FilterRow,
} from "@/lib/filterModel";
import { RecordPicker, UserPicker } from "@/components/fields/widgets";
import { Button } from "./Button";
import { ButtonIcon } from "./ButtonIcon";
import { Combobox, type ComboboxOption } from "./Combobox";
import { fill } from "./DataTable";
import { Input } from "./Input";
import { Select } from "./Select";

export type { FilterField, FilterGroup, FilterMatch, FilterRow };

type OpsFor = (fieldType: string) => readonly string[];

// ── Shared helpers (FilterPanel reuses them) ──────────────────────────────────────────

export const opLabel = (op: string) => t(`slds.expression.op.${op}` as I18nKey);
const relativeLabel = (token: string) => t(`slds.expression.date.${token}` as I18nKey);
export const fieldLabel = (f: FilterField) => f.label ?? f.name;

function picklistValues(field: FilterField | undefined): string[] {
  const v = field?.options?.values;
  return Array.isArray(v) ? (v as unknown[]).map((x) => String(x)) : [];
}

/** Display text for one raw value: picklist valueLabels ("New" → "Mới"), Có/Không, relative dates. */
export function valueLabel(field: FilterField | undefined, v: string): string {
  if (field?.fieldType === "boolean" && (v === "true" || v === "false")) return t(v === "true" ? "slds.expression.true" : "slds.expression.false");
  if (isRelativeDateToken(v)) return relativeLabel(v);
  const labels = field?.options?.valueLabels as Record<string, string> | undefined;
  return labels?.[v] ?? v;
}

/** "bằng "Mới"" / "lớn hơn "500"" / "bằng Tuần này" / "rỗng" — the operator + value half of a
 *  filter summary (blueprint panels/filtering item). Empty when the row has no field yet. */
export function conditionText(row: FilterRow, field: FilterField | undefined): string {
  if (!row.field || !row.op) return "";
  if (isNullOp(row.op)) return opLabel(row.op);
  const parts = row.value.split(",").map((s) => s.trim()).filter(Boolean);
  const shown = parts.map((p) => (isRelativeDateToken(p) ? relativeLabel(p) : `"${valueLabel(field, p)}"`));
  return `${opLabel(row.op)} ${shown.join(row.op === "between" ? " – " : ", ")}`.trim();
}

// ── One condition: field · operator · value ───────────────────────────────────────────

export interface ConditionFieldsProps {
  row: FilterRow;
  fields: FilterField[];
  opsFor?: OpsFor;
  onChange: (row: FilterRow) => void;
  /** Error on the value control. */
  error?: string;
  disabled?: boolean;
  /** "row" = one grid line (expression row); "stacked" = one control per line (filter popover). */
  layout?: "row" | "stacked";
  /** Row layout: trailing column (the delete button). */
  trailing?: ReactNode;
  /** Operators that take no value (default isnull/notnull); the workflow engine adds "changed". */
  valueless?: (op: string) => boolean;
  /** Offer the server-resolved relative dates on date fields (default true). The workflow engine
   *  does not resolve them, so its caller turns them off and keeps a plain date input. */
  relativeDates?: boolean;
  /** Offer "another field of the record" as the value (workflow engine only; token `@field:<name>`). */
  compareFields?: boolean;
}

/** Value token meaning "the value of another field of the same record" (workflow conditions). */
export const FIELD_REF_PREFIX = "@field:";

export function ConditionFields({ row, fields, opsFor = opsForType, onChange, error, disabled, layout = "row", trailing, valueless = isNullOp, relativeDates = true, compareFields = false }: ConditionFieldsProps) {
  const visible = fields.filter((f) => isFilterableType(f.fieldType));
  const field = visible.find((f) => f.name === row.field);
  const ops = field ? opsFor(filterTypeOf(field)) : DEFAULT_OPS;
  const stacked = layout === "stacked" ? "stacked" : undefined;
  const fieldControl = (
    <Combobox
      label={t("slds.expression.field")}
      layout={stacked}
      options={visible.map((f) => ({ value: f.name, label: fieldLabel(f) }))}
      value={row.field || null}
      placeholder={t("slds.select.placeholder")}
      disabled={disabled}
      onChange={(name) => {
        const next = visible.find((f) => f.name === name);
        const nextOps = next ? opsFor(filterTypeOf(next)) : DEFAULT_OPS;
        onChange({ ...row, field: name ?? "", op: nextOps.includes("eq") ? "eq" : (nextOps[0] ?? ""), value: "" });
      }}
    />
  );
  const opControl = (
    <Combobox
      label={t("slds.expression.operator")}
      layout={stacked}
      options={ops.map((op) => ({ value: op, label: opLabel(op) }))}
      value={row.op || null}
      placeholder={t("slds.select.placeholder")}
      disabled={disabled || !row.field}
      onChange={(op) => onChange({ ...row, op: op ?? "", value: "" })}
    />
  );
  const valueControl = (
    <ValueControl
      field={field}
      op={row.op}
      value={row.value}
      error={error}
      inactive={!row.field || !row.op || valueless(row.op)}
      disabled={disabled}
      layout={stacked}
      relativeDates={relativeDates}
      onChange={(value) => onChange({ ...row, value })}
    />
  );
  // queue #286: pick another field instead of a constant. Same-kind fields only (number-ish with
  // number-ish, date with date, else same type) so the comparison is meaningful.
  const refToken = row.value.startsWith(FIELD_REF_PREFIX);
  const refOk = compareFields && !!field && !!row.op && !valueless(row.op) && !["in", "between", "contains"].includes(row.op);
  const kind = (ft: string) => (["number", "currency", "rating"].includes(ft) ? "number" : ["date", "datetime"].includes(ft) ? "date" : ft);
  const refOptions = field ? visible.filter((f) => f.name !== field.name && kind(filterTypeOf(f)) === kind(filterTypeOf(field))) : [];
  const valueSlot = refOk && refOptions.length ? (
    <div className="slds-grid slds-gutters_xx-small">
      <div className="slds-col">
        <Select
          label={t("slds.expression.compareWith")}
          layout={stacked}
          disabled={disabled}
          value={refToken ? row.value : ""}
          placeholder={t("slds.expression.constantValue")}
          options={refOptions.map((f) => ({ value: FIELD_REF_PREFIX + f.name, label: fieldLabel(f) }))}
          onChange={(e) => onChange({ ...row, value: e.target.value })}
        />
      </div>
      {!refToken && <div className="slds-col">{valueControl}</div>}
    </div>
  ) : valueControl;
  if (layout === "stacked") {
    return (
      <>
        {fieldControl}
        {opControl}
        {valueSlot}
      </>
    );
  }
  return (
    <div className="slds-grid slds-gutters_xx-small">
      <div className="slds-col">{fieldControl}</div>
      <div className="slds-col slds-grow-none">{opControl}</div>
      <div className="slds-col">{valueSlot}</div>
      {trailing && <div className="slds-col slds-grow-none">{trailing}</div>}
    </div>
  );
}

// Typed value input per field type + operator. Every widget emits the string FilterBuilder emitted:
// in/between comma-joined, booleans "true"/"false", relative dates as the backend token.
function ValueControl({
  field,
  op,
  value,
  onChange,
  error,
  inactive,
  disabled,
  layout,
  relativeDates = true,
}: {
  field: FilterField | undefined;
  op: string;
  value: string;
  onChange: (v: string) => void;
  error?: string;
  /** No field / operator yet, or a null operator: an empty disabled box (blueprint with-disabled-inputs). */
  inactive?: boolean;
  disabled?: boolean;
  layout?: "stacked";
  relativeDates?: boolean;
}) {
  const label = t("slds.expression.value");
  const ft = field?.fieldType ?? "text";
  const numeric = ft === "number" || ft === "currency" || ft === "rating";
  const isDate = ft === "date" || ft === "datetime";
  if (inactive) return <Input label={label} layout={layout} value="" disabled readOnly />;
  const common = { layout, disabled };

  if (op === "between") {
    const [lo = "", hi = ""] = value.split(",");
    const type = isDate ? "date" : numeric ? "number" : "text";
    return (
      <div className="slds-grid slds-gutters_xx-small">
        <div className="slds-col">
          <Input label={t("slds.expression.from")} {...common} type={type} value={lo} error={error} onChange={(e) => onChange(`${e.target.value},${hi}`)} />
        </div>
        <div className="slds-col">
          <Input label={t("slds.expression.to")} {...common} type={type} value={hi} onChange={(e) => onChange(`${lo},${e.target.value}`)} />
        </div>
      </div>
    );
  }
  const members = picklistValues(field);
  const memberOptions: ComboboxOption[] = members.map((v) => ({ value: v, label: valueLabel(field, v) }));
  if (op === "in") {
    if (members.length) {
      const chosen = value.split(",").map((s) => s.trim()).filter(Boolean);
      return <Combobox label={label} {...common} options={memberOptions} multiple value={chosen} error={error} onChange={(vs) => onChange(vs.join(","))} />;
    }
    return <Input label={label} {...common} value={value} placeholder={t("slds.expression.listPlaceholder")} error={error} onChange={(e) => onChange(e.target.value)} />;
  }
  // #449: a person / a related record is chosen by name, never typed as an id (eq/neq store one id).
  if ((ft === "user" || ft === "relation") && (op === "eq" || op === "neq")) {
    const frame = { label, layout, error };
    return ft === "user"
      ? <UserPicker frame={frame} value={value} onChange={onChange} />
      : <RecordPicker frame={frame} relatedEntity={(field?.options?.relatedEntity as string) ?? ""} labelFieldOpt={field?.options?.labelField as string | undefined} value={value} onChange={onChange} />;
  }
  if (ft === "picklist" || ft === "multi_select") {
    return <Combobox label={label} {...common} options={memberOptions} value={value || null} placeholder={t("slds.select.placeholder")} error={error} onChange={(v) => onChange(v ?? "")} />;
  }
  if (ft === "boolean") {
    const yesNo = ["true", "false"].map((v) => ({ value: v, label: valueLabel(field, v) }));
    return <Combobox label={label} {...common} options={yesNo} value={value || null} placeholder={t("slds.select.placeholder")} error={error} onChange={(v) => onChange(v ?? "")} />;
  }
  if (isDate && relativeDates && RELATIVE_OK_OPS.has(op)) {
    const token = isRelativeDateToken(value);
    return (
      <div className="slds-grid slds-gutters_xx-small">
        <div className="slds-col">
          <Select
            label={label}
            {...common}
            value={token ? value : ""}
            placeholder={t("slds.expression.specificDate")}
            options={RELATIVE_DATE_TOKENS.map((tk) => ({ value: tk, label: relativeLabel(tk) }))}
            error={token ? error : undefined}
            onChange={(e) => onChange(e.target.value)}
          />
        </div>
        {!token && (
          <div className="slds-col">
            <Input label={t("slds.expression.date")} {...common} type="date" value={value} error={error} onChange={(e) => onChange(e.target.value)} />
          </div>
        )}
      </div>
    );
  }
  return <Input label={label} {...common} type={isDate ? "date" : numeric ? "number" : "text"} value={value} error={error} onChange={(e) => onChange(e.target.value)} />;
}

// ── ExpressionBuilder ────────────────────────────────────────────────────────────────

export interface ExpressionBuilderProps {
  /** Candidate fields; non-filterable types (composite/rich_text/file/image/formula) are dropped. */
  fields: FilterField[];
  /** Top-level condition rows (controlled) — the FilterBuilder contract. */
  value: FilterRow[];
  onChange: (rows: FilterRow[]) => void;
  /** Per-field-type operator list (default opsForType; approval passes approvalOpsForType). */
  opsFor?: OpsFor;
  /** Heading; default "Điều kiện", `null` hides it. */
  title?: string | null;
  /** Shows the "Áp dụng khi" select. Without it the rows combine with AND. */
  match?: FilterMatch;
  onMatchChange?: (match: FilterMatch) => void;
  /** Modes offered in the select (default all / any / custom). */
  matchOptions?: FilterMatch[];
  customLogic?: string;
  onCustomLogicChange?: (logic: string) => void;
  /** Condition groups (one level) — "Thêm nhóm" shows when onGroupsChange is given. */
  groups?: FilterGroup[];
  onGroupsChange?: (groups: FilterGroup[]) => void;
  /** Value errors keyed by row id, e.g. { "7": "Nhập giá trị" }. */
  errors?: Record<string, string>;
  disabled?: boolean;
  className?: string;
  /** See ConditionFieldsProps.valueless / relativeDates. */
  valueless?: (op: string) => boolean;
  relativeDates?: boolean;
  compareFields?: boolean;
}

export function ExpressionBuilder({
  fields,
  value,
  onChange,
  opsFor,
  title = t("slds.expression.title"),
  match = "all",
  onMatchChange,
  matchOptions = ["all", "any", "custom"],
  customLogic = "",
  onCustomLogicChange,
  groups = [],
  onGroupsChange,
  errors,
  disabled,
  className,
  valueless,
  relativeDates,
  compareFields,
}: ExpressionBuilderProps) {
  const id = useId();
  const count = value.length + groups.length;
  const conj = (m: FilterMatch) => t(m === "any" ? "slds.expression.or" : "slds.expression.and");
  // Legend text of top-level item i (0-based): numbers under custom logic, else the conjunction
  // from the second item on (the first row's legend is assistive text only).
  const legend = (i: number) => (match === "custom" ? String(i + 1) : i > 0 ? conj(match) : null);
  const issue = match === "custom" ? checkCustomLogic(customLogic, count) : null;
  const logicError = issue && (issue.code === "syntax" ? t("slds.expression.logic.syntax") : fill(t(`slds.expression.logic.${issue.code}` as I18nKey), { n: issue.n }));

  // DOM id of the <li> whose first combobox takes focus after the next render; "" = "Thêm điều kiện".
  const focusTarget = useRef<string | null>(null);
  const addRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const target = focusTarget.current;
    if (target === null) return;
    focusTarget.current = null;
    const el = target ? document.getElementById(target)?.querySelector<HTMLElement>('[role="combobox"]') : addRef.current?.querySelector("button");
    el?.focus();
  });
  const rowDom = (rowId: string) => `${id}-row-${rowId}`;
  const groupDom = (groupId: string) => `${id}-group-${groupId}`;
  const topItems = [...value.map((r) => rowDom(r.id)), ...groups.map((g) => groupDom(g.id))];
  // The item after `i`, else the one before, else "" (the add button).
  const neighbour = (items: string[], i: number) => items[i + 1] ?? items[i - 1] ?? "";

  const patchRow = (next: FilterRow) => onChange(value.map((r) => (r.id === next.id ? next : r)));
  const patchGroup = (g: FilterGroup) => onGroupsChange?.(groups.map((x) => (x.id === g.id ? g : x)));

  // `focusAfter`: the item that takes focus once this row is gone (set in the handler — never in render).
  const deleteButton = (label: string, focusAfter: string, onDelete: () => void) => (
    <div className="slds-form-element">
      <span className="slds-form-element__label">{"\u00a0"}</span>
      <div className="slds-form-element__control">
        <ButtonIcon
          icon={Trash2}
          label={label}
          variant="border-filled"
          disabled={disabled}
          onClick={() => {
            focusTarget.current = focusAfter;
            onDelete();
          }}
        />
      </div>
    </div>
  );

  const renderRow = (
    row: FilterRow,
    legendText: string | null,
    assistive: string,
    deleteLabel: string,
    onRowChange: (r: FilterRow) => void,
    focusAfter: string,
    onDelete: () => void,
    inGroup?: boolean,
  ) => (
    <li key={row.id} id={rowDom(row.id)} className={clsx("slds-expression__row", inGroup && "slds-expression__row_group")}>
      <fieldset>
        <legend className="slds-expression__legend">
          {legendText && <span>{legendText}</span>}
          <span className="slds-assistive-text">{assistive}</span>
        </legend>
        <ConditionFields
          row={row}
          fields={fields}
          opsFor={opsFor}
          onChange={onRowChange}
          error={errors?.[row.id]}
          disabled={disabled}
          valueless={valueless}
          relativeDates={relativeDates}
          compareFields={compareFields}
          trailing={deleteButton(deleteLabel, focusAfter, onDelete)}
        />
      </fieldset>
    </li>
  );

  const matchSelect = (current: FilterMatch, modes: FilterMatch[], onPick: (m: FilterMatch) => void) => (
    <div className="slds-expression__options">
      <Combobox
        label={t("slds.expression.matchLabel")}
        options={modes.map((m) => ({ value: m, label: t(`slds.expression.match.${m}` as I18nKey) }))}
        value={current}
        disabled={disabled}
        onChange={(m) => m && onPick(m as FilterMatch)}
      />
    </div>
  );

  return (
    <div className={clsx("slds-expression", className)}>
      {title !== null && <h2 className="slds-expression__title">{title}</h2>}
      {onMatchChange && matchSelect(match, matchOptions, onMatchChange)}
      {match === "custom" && (
        <div className="slds-expression__custom-logic">
          <Input
            id={`${id}-logic`}
            label={t("slds.expression.customLogic")}
            value={customLogic}
            placeholder={t("slds.expression.customLogicHelp")}
            error={logicError ?? undefined}
            disabled={disabled}
            onChange={(e) => onCustomLogicChange?.(e.target.value)}
          />
        </div>
      )}
      <ul>
        {value.map((row, i) =>
          renderRow(
            row,
            legend(i),
            fill(t("slds.expression.condition"), { n: i + 1 }),
            fill(t("slds.expression.deleteCondition"), { n: i + 1 }),
            patchRow,
            neighbour(topItems, i),
            () => onChange(value.filter((r) => r.id !== row.id)),
          ),
        )}
        {groups.map((g, gi) => {
          const n = value.length + gi + 1;
          return (
            <li key={g.id} id={groupDom(g.id)} className="slds-expression__group">
              <fieldset>
                <legend className="slds-expression__legend slds-expression__legend_group">
                  {legend(n - 1) && <span>{legend(n - 1)}</span>}
                  <span className="slds-assistive-text">{fill(t("slds.expression.group"), { g: gi + 1 })}</span>
                </legend>
                {matchSelect(g.match, ["all", "any"], (m) => patchGroup({ ...g, match: m as FilterGroup["match"] }))}
                <ul>
                  {g.rows.map((row, ri) =>
                    renderRow(
                      row,
                      ri > 0 ? conj(g.match) : null,
                      fill(t("slds.expression.groupCondition"), { n: ri + 1, g: gi + 1 }),
                      fill(t("slds.expression.deleteGroupCondition"), { n: ri + 1, g: gi + 1 }),
                      (r) => patchGroup({ ...g, rows: g.rows.map((x) => (x.id === r.id ? r : x)) }),
                      // The group's last row out takes the group with it: focus the next top-level item.
                      g.rows.length > 1 ? neighbour(g.rows.map((x) => rowDom(x.id)), ri) : neighbour(topItems, n - 1),
                      () => {
                        const rows = g.rows.filter((x) => x.id !== row.id);
                        // An emptied group disappears (the base blueprint has no "remove group" action).
                        onGroupsChange?.(rows.length ? groups.map((x) => (x.id === g.id ? { ...g, rows } : x)) : groups.filter((x) => x.id !== g.id));
                      },
                      true,
                    ),
                  )}
                </ul>
                <div className="slds-expression__buttons">
                  <Button
                    iconLeft={Plus}
                    disabled={disabled}
                    onClick={() => {
                      const added = newFilterRow("", "");
                      focusTarget.current = rowDom(added.id);
                      patchGroup({ ...g, rows: [...g.rows, added] });
                    }}
                  >
                    {t("slds.expression.addCondition")}
                  </Button>
                </div>
              </fieldset>
            </li>
          );
        })}
      </ul>
      <div className="slds-expression__buttons" ref={addRef}>
        <Button
          iconLeft={Plus}
          disabled={disabled}
          onClick={() => {
            const added = newFilterRow("", "");
            focusTarget.current = rowDom(added.id);
            onChange([...value, added]);
          }}
        >
          {t("slds.expression.addCondition")}
        </Button>
        {onGroupsChange && (
          <Button
            iconLeft={Plus}
            disabled={disabled}
            onClick={() => {
              const added = newFilterGroup();
              focusTarget.current = groupDom(added.id);
              onGroupsChange([...groups, added]);
            }}
          >
            {t("slds.expression.addGroup")}
          </Button>
        )}
      </div>
    </div>
  );
}
