"use client";

// SLDS combobox (blueprint `combobox` base / autocomplete / grouped options, SLDS 2.264.1):
//   div.slds-form-element > label + div.slds-form-element__control >
//     div.slds-combobox_container[.slds-has-selection] > div.slds-combobox.slds-dropdown-trigger.slds-dropdown-trigger_click[.slds-is-open] >
//       div.slds-combobox__form-element.slds-input-has-icon.slds-input-has-icon_right[role=none] >
//         select-only:  div.slds-input_faux.slds-combobox__input[role=combobox][tabindex=0] > span.slds-truncate
//         autocomplete: input.slds-input.slds-combobox__input[role=combobox][aria-autocomplete=list]
//         + span.slds-icon_container.slds-input__icon.slds-input__icon_right (or remove button / spinner group)
//       + div.slds-dropdown.slds-dropdown_fluid[role=listbox] > ul.slds-listbox.slds-listbox_vertical[role=presentation|group]
//           > li.slds-listbox__item[role=presentation] > div.slds-media.slds-listbox__option[role=option]
//     + div.slds-listbox_selection-group (multi-select pills)
// Behaviour is the ARIA 1.2 combobox by hand — cmdk can emit neither this markup nor a select-only
// combobox. Focus never leaves the field: the highlighted option is aria-activedescendant +
// aria-selected + slds-has-focus, a chosen one aria-checked + slds-is-selected (the blueprint's split).
// Radix Popover only places the listbox (position:fixed, so it escapes overflow clipping) and dismisses
// it on Esc / outside click. Like Menu it is NOT portaled (it stays in a Modal's stacking context);
// `static` cancels SLDS's position:absolute and `slds-dropdown_left` its translateX(-50%) so the popper
// can place it (the datetime-picker blueprint pairs _fluid with _left too).
// Deviation: the listbox is tabindex=-1, not the blueprint's 0 — a tabbable popup would take the Tab
// that is meant to leave the field (the combobox pattern keeps DOM focus on the field).
import * as PopoverPrimitive from "@radix-ui/react-popover";
import clsx from "clsx";
import { Check, ChevronDown, Search, X, type LucideIcon } from "lucide-react";
import { useEffect, useRef, useState, type FocusEvent, type KeyboardEvent, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { ButtonIcon } from "./ButtonIcon";
import { FormElement, describedBy, useFieldIds, type FieldProps } from "./FormElement";
import { Icon } from "./Icon";
import { Pill, PillContainer } from "./Pill";
import { Spinner } from "./Spinner";

export interface ComboboxOption {
  value: string;
  label: string;
  /** Second line of an entity option (`slds-listbox__option-meta`), e.g. "Khách hàng • Hà Nội".
   *  Autocomplete typing matches it too (a person's email under their name). */
  meta?: string;
  /** Object tile — switches the row to the entity layout (lookups, record search). */
  icon?: { glyph: LucideIcon; name: string; label: string };
  disabled?: boolean;
  /** Heading the option is listed under (blueprint "Grouped options"). */
  group?: string;
}

const foldChar = (ch: string) => (ch === "đ" || ch === "Đ" ? "d" : ch.normalize("NFD")[0].toLowerCase()[0]);
/** Case- and mark-insensitive form, one char per code point so match indices map back onto the
 *  label: "Hà Nội" → "ha noi" — Vietnamese users type without diacritics. */
export const fold = (s: string) => Array.from(s.normalize("NFC"), foldChar).join("");
export const matches = (label: string, term: string) => fold(label).includes(fold(term.trim()));

/** The label with the typed text wrapped in <mark> (blueprint typeahead state). */
export function highlight(text: string, term: string): ReactNode {
  const q = fold(term.trim());
  const chars = Array.from(text.normalize("NFC"));
  const at = q ? chars.map(foldChar).join("").indexOf(q) : -1;
  if (at < 0) return text;
  return (
    <span>
      {chars.slice(0, at).join("")}
      <mark>{chars.slice(at, at + q.length).join("")}</mark>
      {chars.slice(at + q.length).join("")}
    </span>
  );
}

export interface ComboboxFieldProps extends FieldProps {
  id?: string;
  /** "select" = read-only listbox trigger (blueprint base); "autocomplete" = text input. */
  mode: "select" | "autocomplete";
  /** Options to list, already filtered by the owner. */
  options: ComboboxOption[];
  /** Chosen options as objects, so an async result that is no longer listed keeps its label/icon. */
  selected: ComboboxOption[];
  multiple?: boolean;
  onSelect: (option: ComboboxOption) => void;
  onDeselect: (option: ComboboxOption) => void;
  /** Autocomplete text (controlled by the owner). */
  inputValue?: string;
  onInputChange?: (text: string) => void;
  /** Autocomplete single-select: once chosen, the input becomes the read-only value + remove button
   *  (blueprint "Selected an option"). TimePicker turns it off and keeps the text editable. */
  lockSelection?: boolean;
  /** Enter with nothing highlighted, and blur: the typed text goes to the owner (TimePicker parses it). */
  onCommit?: (text: string) => void;
  placeholder?: string;
  /** Select-only: spinner row after the options ("Loading more options"); autocomplete: spinner in
   *  the field ("Typeahead - Loading"). Both set aria-busy on the listbox. */
  loading?: boolean;
  /** Row shown when the typed text matches nothing; `false` hides it. */
  noResultsText?: string | false;
  disabled?: boolean;
  readOnly?: boolean;
  required?: boolean;
  /** false = aria-required only: a compound fieldset's legend carries the mark (DateTimePicker). */
  requiredMark?: boolean;
  /** Error line drawn by a compound fieldset: the field is invalid and described by that line, but
   *  draws no message (nor slds-has-error) of its own — the fieldset's slds-has-error colours it. */
  errorId?: string;
  /** Right glyph + utility name; default down (select) / search (autocomplete). */
  icon?: { glyph: LucideIcon; name: string };
  /** Extra class on div.slds-combobox_container, e.g. `slds-size_small` (picklist). */
  containerClassName?: string;
  /** Extra class on div.slds-combobox, e.g. `slds-timepicker`. */
  comboboxClassName?: string;
  className?: string;
}

/** The field every picker renders through (Combobox, Picklist, Lookup, TimePicker). */
export function ComboboxField({
  id,
  mode,
  options,
  selected,
  multiple,
  onSelect,
  onDeselect,
  inputValue = "",
  onInputChange,
  lockSelection = true,
  onCommit,
  placeholder,
  loading,
  noResultsText,
  disabled,
  readOnly,
  required,
  requiredMark = true,
  errorId,
  icon,
  containerClassName,
  comboboxClassName,
  className,
  label,
  labelHidden,
  error,
  help,
  fieldLevelHelp,
  layout,
  oneColumn,
  editing,
}: ComboboxFieldProps) {
  const { controlId, labelId, helpId } = useFieldIds(id);
  const listboxId = `${controlId}-listbox`;
  const optionId = (i: number) => `${listboxId}-${i}`;
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState<string | null>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const fauxRef = useRef<HTMLDivElement>(null);
  const focusNext = useRef<"input" | "faux" | null>(null);

  const editable = !disabled && !readOnly;
  const locked = mode === "autocomplete" && !multiple && lockSelection && selected.length > 0;
  const faux = mode === "select" || locked;
  const chosen = new Set(selected.map((o) => o.value));
  // Text that is just the current value (TimePicker shows "08:00" in the box) is not a search term.
  const term = !multiple && selected[0]?.label === inputValue ? "" : inputValue;

  // Render order = keyboard order: options are gathered under their group's first appearance.
  const firstSeen = new Map<string, number>();
  options.forEach((o, i) => firstSeen.has(o.group ?? "") || firstSeen.set(o.group ?? "", i));
  const ordered = [...options].sort((a, b) => firstSeen.get(a.group ?? "")! - firstSeen.get(b.group ?? "")!);
  const enabled = ordered.filter((o) => !o.disabled);
  const activeIndex = ordered.findIndex((o) => o.value === active && !o.disabled);
  const empty = mode === "autocomplete" && !loading && ordered.length === 0 && inputValue.trim() !== "" && noResultsText !== false;
  // Autocomplete shows its spinner in the field, so a load with nothing listed yet opens no empty box.
  const showList = open && editable && (ordered.length > 0 || (Boolean(loading) && mode === "select") || empty);

  // Keep the highlighted option in view (slds-dropdown_length-5 scrolls; TimePicker lists 96 times).
  // Scroll the listbox only — scrollIntoView would also scroll the page while Radix still has the
  // popper parked off-screen on the first frame, and the page jump then flips the listbox upwards.
  // `list` is state set by the listbox's ref: Radix Presence mounts the content one commit after
  // `open`, so an effect keyed on `showList` alone would run before the listbox exists.
  const [list, setList] = useState<HTMLDivElement | null>(null);
  useEffect(() => {
    const option = list && activeIndex >= 0 ? document.getElementById(`${listboxId}-${activeIndex}`) : null;
    if (!list || !option) return;
    const o = option.getBoundingClientRect();
    const l = list.getBoundingClientRect();
    if (o.top < l.top) list.scrollBy(0, o.top - l.top);
    else if (o.bottom > l.bottom) list.scrollBy(0, o.bottom - l.bottom);
  }, [list, activeIndex, listboxId]);

  // The single-select autocomplete swaps input ↔ read-only value; hand focus to whichever is rendered.
  useEffect(() => {
    const target = focusNext.current;
    focusNext.current = null;
    if (target === "input") inputRef.current?.focus();
    else if (target === "faux") fauxRef.current?.focus();
  });

  const close = () => {
    setOpen(false);
    setActive(null);
  };
  // Focus that stays inside the combobox (e.g. into the tabindex=-1 listbox on a scrollbar drag) is
  // not "leaving" it — same rule as onInteractOutside below.
  const closeOnLeave = (e: FocusEvent) => {
    if (!rootRef.current?.contains(e.relatedTarget as Node | null)) close();
  };
  const openList = (pick: "chosen" | "last" | "none" = "chosen") => {
    if (!editable) return;
    setOpen(true);
    const target = pick === "none" ? undefined : pick === "last" ? enabled[enabled.length - 1] : (enabled.find((o) => chosen.has(o.value)) ?? enabled[0]);
    setActive(target?.value ?? null);
  };
  const move = (step: 1 | -1) => {
    if (!enabled.length) return;
    const i = enabled.findIndex((o) => o.value === active);
    const next = i < 0 ? (step === 1 ? 0 : enabled.length - 1) : (i + step + enabled.length) % enabled.length;
    setActive(enabled[next].value);
  };
  // Docs: a letter highlights the first option starting with it; pressing it again moves to the next.
  const typeahead = (key: string) => {
    const from = enabled.findIndex((o) => o.value === active);
    const hit = [...enabled.slice(from + 1), ...enabled.slice(0, from + 1)].find((o) => fold(o.label).startsWith(fold(key)));
    if (!hit) return;
    setOpen(true);
    setActive(hit.value);
  };
  const choose = (o: ComboboxOption) => {
    if (o.disabled) return;
    if (multiple) {
      if (chosen.has(o.value)) onDeselect(o);
      else onSelect(o);
      setActive(o.value);
      if (mode === "autocomplete" && inputValue) onInputChange?.("");
      return;
    }
    onSelect(o);
    close();
    if (mode === "autocomplete" && lockSelection) {
      onInputChange?.("");
      focusNext.current = "faux";
    }
  };
  const clearSelection = () => {
    if (selected[0]) onDeselect(selected[0]);
    focusNext.current = "input";
  };

  const onKeyDown = (e: KeyboardEvent<HTMLElement>) => {
    if (!editable) return;
    const current = ordered[activeIndex];
    switch (e.key) {
      case "ArrowDown":
      case "ArrowUp":
        e.preventDefault();
        if (!showList) openList(e.key === "ArrowUp" ? "last" : "chosen");
        else move(e.key === "ArrowDown" ? 1 : -1);
        return;
      case "Home":
      case "End":
        if (!faux || !showList) return; // in a text input they move the caret
        e.preventDefault();
        setActive((e.key === "Home" ? enabled[0] : enabled[enabled.length - 1])?.value ?? null);
        return;
      case "Enter":
        if (showList && current) {
          e.preventDefault();
          choose(current);
        } else if (faux) {
          e.preventDefault();
          openList();
        } else if (onCommit) {
          e.preventDefault();
          onCommit(inputValue);
          close();
        }
        return;
      case " ":
        if (!faux) return;
        e.preventDefault();
        if (showList && current) choose(current);
        else openList();
        return;
      case "Tab":
        if (open) close();
        return;
      case "Backspace":
      case "Delete":
        if (!locked) return;
        e.preventDefault();
        clearSelection();
        return;
      default:
        if (faux && !locked && e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) typeahead(e.key);
    }
  };

  const message = error ?? help;
  const aria = {
    "aria-haspopup": "listbox" as const,
    "aria-activedescendant": showList && activeIndex >= 0 ? optionId(activeIndex) : undefined,
    "aria-describedby": describedBy(message != null && message !== "" && helpId, errorId),
    "aria-required": required || undefined,
    "aria-invalid": error || errorId ? true : undefined,
  };

  let fauxText: ReactNode = placeholder ?? t("slds.combobox.placeholder");
  if (selected.length === 1) fauxText = selected[0].label;
  else if (selected.length > 1) fauxText = t("slds.combobox.selectedCount").replace("{n}", String(selected.length));
  const lockedIcon = locked ? selected[0].icon : undefined;
  const glyph = icon ?? (mode === "select" ? { glyph: ChevronDown, name: "down" } : { glyph: Search, name: "search" });
  const removeButton = locked && editable && (
    <ButtonIcon icon={X} label={t("slds.combobox.removeSelected")} className="slds-input__icon slds-input__icon_right" onClick={clearSelection} />
  );
  const spinnerInField = mode === "autocomplete" && loading;

  const field = faux ? (
    <div
      ref={fauxRef}
      role="combobox"
      tabIndex={disabled ? -1 : 0}
      id={`${controlId}-selected-value`}
      aria-labelledby={labelId}
      aria-controls={listboxId}
      aria-expanded={showList}
      aria-disabled={disabled || undefined}
      aria-readonly={readOnly || undefined}
      {...aria}
      className={clsx(
        "slds-input_faux slds-combobox__input",
        showList && "slds-has-focus",
        selected.length > 0 && "slds-combobox__input-value",
        disabled && "slds-is-disabled",
      )}
      onClick={() => (showList ? close() : openList())}
      onBlur={closeOnLeave}
      onKeyDown={onKeyDown}
    >
      <span className="slds-truncate" id={`${controlId}-value`}>
        {fauxText}
      </span>
    </div>
  ) : (
    <input
      ref={inputRef}
      id={controlId}
      type="text"
      role="combobox"
      autoComplete="off"
      aria-autocomplete="list"
      aria-controls={listboxId}
      aria-expanded={showList}
      aria-readonly={readOnly || undefined}
      {...aria}
      // Multi keeps slds-has-selection on the container for its pills; -value there is the locked
      // single value's inset (blueprint "Selected an option"), wrong for text still being typed.
      className={clsx("slds-input slds-combobox__input", showList && "slds-has-focus", inputValue !== "" && !multiple && "slds-combobox__input-value")}
      placeholder={placeholder ?? t("slds.combobox.search")}
      value={inputValue}
      disabled={disabled}
      readOnly={readOnly}
      onChange={(e) => {
        onInputChange?.(e.target.value);
        if (editable) setOpen(true);
        setActive(null);
      }}
      // Docs: re-opening with a value shows it highlighted; with none, nothing is highlighted yet.
      onClick={() => !showList && openList(selected.length ? "chosen" : "none")}
      // Docs: leaving the combobox hides the options. `open` must drop too, not just the listbox:
      // while nothing is listed yet (search in flight) Radix has no content to dismiss, and results
      // landing later would pop the list open under a field the user has already left.
      onBlur={(e) => {
        onCommit?.(inputValue);
        closeOnLeave(e);
      }}
      onKeyDown={onKeyDown}
    />
  );

  const renderOption = (o: ComboboxOption, i: number) => {
    const isActive = i === activeIndex;
    const isChosen = chosen.has(o.value);
    return (
      <li key={o.value} role="presentation" className="slds-listbox__item">
        <div
          id={optionId(i)}
          role="option"
          aria-selected={isActive || undefined}
          aria-checked={isChosen || undefined}
          aria-disabled={o.disabled || undefined}
          className={clsx(
            "slds-media slds-listbox__option",
            o.icon
              ? ["slds-listbox__option_entity", o.meta ? "slds-listbox__option_has-meta" : "slds-media_center"]
              : "slds-listbox__option_plain slds-media_small",
            isChosen && "slds-is-selected",
            isActive && "slds-has-focus",
          )}
          onClick={() => choose(o)}
        >
          <span className="slds-media__figure slds-listbox__option-icon">
            {o.icon ? (
              <Icon icon={o.icon.glyph} category="standard" name={o.icon.name} size="small" />
            ) : (
              isChosen && (
                <span className="slds-icon_container slds-icon-utility-check slds-current-color">
                  <Check className="slds-icon slds-icon_x-small nx-lucide" aria-hidden="true" />
                </span>
              )
            )}
          </span>
          <span className="slds-media__body">
            {o.icon ? (
              <>
                <span className="slds-listbox__option-text slds-listbox__option-text_entity">{highlight(o.label, term)}</span>
                {o.meta && <span className="slds-listbox__option-meta slds-listbox__option-meta_entity">{o.meta}</span>}
              </>
            ) : o.meta ? (
              // Flat option with a second line (queue #301: two tables both called "Khách hàng").
              <>
                <span className="slds-listbox__option-text" title={o.label}>{highlight(o.label, term)}</span>
                <span className="slds-listbox__option-meta">{o.meta}</span>
              </>
            ) : (
              <span title={o.label}>{highlight(o.label, term)}</span>
            )}
          </span>
        </div>
      </li>
    );
  };

  const tail = (
    <>
      {loading && mode === "select" && (
        <li role="option" aria-selected={false} className="slds-listbox__item">
          <div className="slds-align_absolute-center slds-p-top_medium">
            <Spinner size="x-small" inline />
          </div>
        </li>
      )}
      {/* No blueprint for "no results": the plain-option row, as presentation (not choosable). */}
      {empty && (
        <li role="presentation" className="slds-listbox__item">
          <div className="slds-media slds-listbox__option slds-listbox__option_plain slds-media_small" role="presentation">
            <span className="slds-media__body">
              <span>{noResultsText || t("slds.combobox.noResults")}</span>
            </span>
          </div>
        </li>
      )}
    </>
  );

  const groups: { label?: string; items: { o: ComboboxOption; i: number }[] }[] = [];
  ordered.forEach((o, i) => {
    const last = groups[groups.length - 1];
    if (last && last.label === o.group) last.items.push({ o, i });
    else groups.push({ label: o.group, items: [{ o, i }] });
  });
  const grouped = ordered.some((o) => o.group);

  return (
    <FormElement
      label={label}
      labelHidden={labelHidden}
      htmlFor={controlId}
      labelId={labelId}
      helpId={helpId}
      required={required && requiredMark}
      error={error}
      help={help}
      fieldLevelHelp={fieldLevelHelp}
      layout={layout}
      oneColumn={oneColumn}
      editing={editing}
      className={className}
    >
      <PopoverPrimitive.Root open={showList} onOpenChange={(next) => !next && close()}>
        <div className={clsx("slds-combobox_container", (locked || (mode === "autocomplete" && multiple && selected.length > 0)) && "slds-has-selection", containerClassName)}>
          <div ref={rootRef} className={clsx("slds-combobox slds-dropdown-trigger slds-dropdown-trigger_click", showList && "slds-is-open", comboboxClassName)}>
            <PopoverPrimitive.Anchor asChild>
              <div
                role="none"
                className={clsx(
                  "slds-combobox__form-element slds-input-has-icon",
                  spinnerInField ? "slds-input-has-icon_group-right" : lockedIcon ? "slds-input-has-icon_left-right" : "slds-input-has-icon_right",
                )}
              >
                {lockedIcon && (
                  <Icon icon={lockedIcon.glyph} category="standard" name={lockedIcon.name} size="small" label={lockedIcon.label} className="slds-combobox__input-entity-icon" />
                )}
                {field}
                {spinnerInField ? (
                  <div className="slds-input__icon-group slds-input__icon-group_right">
                    <Spinner size="x-small" variant="brand" className="slds-input__spinner" />
                    {removeButton}
                  </div>
                ) : (
                  removeButton || <Icon icon={glyph.glyph} name={glyph.name} size="x-small" className="slds-input__icon slds-input__icon_right" />
                )}
              </div>
            </PopoverPrimitive.Anchor>
            <PopoverPrimitive.Content
              asChild
              side="bottom"
              align="start"
              sideOffset={0}
              onOpenAutoFocus={(e) => e.preventDefault()}
              onCloseAutoFocus={(e) => e.preventDefault()}
              onInteractOutside={(e) => {
                if (rootRef.current?.contains(e.target as Node)) e.preventDefault();
              }}
            >
              <div
                ref={setList}
                id={listboxId}
                role="listbox"
                aria-label={typeof label === "string" ? label : undefined}
                aria-multiselectable={multiple || undefined}
                aria-busy={Boolean(loading)}
                tabIndex={-1}
                className={clsx(
                  "slds-dropdown slds-dropdown_fluid slds-dropdown_left static",
                  ordered.some((o) => o.icon) ? "slds-dropdown_length-with-icon-7" : "slds-dropdown_length-5",
                )}
                style={{ width: "var(--radix-popover-trigger-width)" }}
                // Keep focus in the field while an option is clicked.
                onMouseDown={(e) => e.preventDefault()}
              >
                {grouped ? (
                  <>
                    {groups.map((g) =>
                      g.label ? (
                        <ul key={g.label} className="slds-listbox slds-listbox_vertical" role="group" aria-label={g.label}>
                          <li role="presentation" className="slds-listbox__item">
                            <div className="slds-media slds-listbox__option slds-listbox__option_plain slds-media_small" role="presentation">
                              <h3 className="slds-listbox__option-header" role="presentation">
                                {g.label}
                              </h3>
                            </div>
                          </li>
                          {g.items.map(({ o, i }) => renderOption(o, i))}
                        </ul>
                      ) : (
                        <ul key="" className="slds-listbox slds-listbox_vertical" role="presentation">
                          {g.items.map(({ o, i }) => renderOption(o, i))}
                        </ul>
                      ),
                    )}
                    {(loading || empty) && (
                      <ul className="slds-listbox slds-listbox_vertical" role="presentation">
                        {tail}
                      </ul>
                    )}
                  </>
                ) : (
                  <ul className="slds-listbox slds-listbox_vertical" role="presentation">
                    {ordered.map(renderOption)}
                    {tail}
                  </ul>
                )}
              </div>
            </PopoverPrimitive.Content>
          </div>
        </div>
      </PopoverPrimitive.Root>
      {multiple && (
        // Always expanded: the collapsed group hides every row after the first, with no "+N more" yet.
        // Kept mounted (slds-hide) when empty so removing the last pill can still hand focus to the field.
        <PillContainer
          variant="selection"
          className={clsx("slds-is-expanded", selected.length === 0 && "slds-hide")}
          emptyFocusRef={mode === "autocomplete" ? inputRef : fauxRef}
        >
          {selected.map((o) => (
            <Pill key={o.value} label={o.label} icon={o.icon} onRemove={editable ? () => onDeselect(o) : undefined} />
          ))}
        </PillContainer>
      )}
    </FormElement>
  );
}

type ComboboxBaseProps = Omit<
  ComboboxFieldProps,
  "mode" | "selected" | "multiple" | "onSelect" | "onDeselect" | "inputValue" | "onInputChange" | "lockSelection" | "onCommit"
> & {
  /** "select" (default) = read-only listbox; "autocomplete" = type to filter the options. */
  variant?: "select" | "autocomplete";
};

export type ComboboxProps = ComboboxBaseProps &
  (
    | { multiple?: false; value?: string | null; defaultValue?: string | null; onChange?: (value: string | null) => void }
    | { multiple: true; value?: string[]; defaultValue?: string[]; onChange?: (value: string[]) => void }
  );

const toList = (v: string | string[] | null | undefined) => (v == null ? [] : Array.isArray(v) ? v : [v]);

/** Local-options combobox; values are option `value`s. Controlled (`value`) or not (`defaultValue`). */
export function Combobox(props: ComboboxProps) {
  const { variant = "select", options, multiple, value, defaultValue, onChange, ...field } = props;
  const [inner, setInner] = useState<string[]>(() => toList(defaultValue));
  const [text, setText] = useState("");
  const values = value !== undefined ? toList(value) : inner;
  const commit = (next: string[]) => {
    setInner(next);
    (onChange as ((v: string[] | string | null) => void) | undefined)?.(multiple ? next : (next[0] ?? null));
  };
  return (
    <ComboboxField
      {...field}
      mode={variant}
      multiple={multiple}
      options={variant === "autocomplete" && text.trim() ? options.filter((o) => matches(o.label, text) || (o.meta != null && matches(o.meta, text))) : options}
      selected={values.flatMap((v) => options.filter((o) => o.value === v))}
      onSelect={(o) => commit(multiple ? [...values, o.value] : [o.value])}
      onDeselect={(o) => commit(values.filter((v) => v !== o.value))}
      inputValue={text}
      onInputChange={setText}
    />
  );
}
