"use client";

// SLDS lookup (blueprint `lookups`, SLDS 2.264.1 — "a lookup is a combobox"): record search on the
// autocomplete combobox. Results are entity options (object tile + name + meta line); one choice
// becomes the read-only value with the object icon and a remove button (blueprint "Option Selected"),
// several become pills with the object icon (combobox/autocomplete "Lookup - Selected multiple options").
// `search(term)` runs debounced; a response that lands after a newer one is dropped, and a failed
// search says so in the list instead of looking like "no results".
// Omitted: the blueprint's leading "search for term" row (it opens a full results page Nexus doesn't have).
// ponytail: searches only once something is typed — no recent-records list on focus; add an onOpen hook
// to ComboboxField and call search("") when a caller has recents to show.
import { useEffect, useRef, useState } from "react";
import { t } from "@/lib/i18n";
import { ComboboxField, type ComboboxFieldProps, type ComboboxOption } from "./Combobox";

type LookupBaseProps = Omit<
  ComboboxFieldProps,
  "mode" | "options" | "selected" | "multiple" | "onSelect" | "onDeselect" | "inputValue" | "onInputChange" | "lockSelection" | "onCommit" | "loading"
> & {
  /** Record search; options carry `icon` (object tile) and usually `meta`. */
  search: (term: string) => Promise<ComboboxOption[]>;
  /** Debounce in ms before `search` runs (default 250). */
  delay?: number;
};

export type LookupProps = LookupBaseProps &
  (
    | { multiple?: false; value?: ComboboxOption | null; defaultValue?: ComboboxOption | null; onChange?: (value: ComboboxOption | null) => void }
    | { multiple: true; value?: ComboboxOption[]; defaultValue?: ComboboxOption[]; onChange?: (value: ComboboxOption[]) => void }
  );

const toList = (v: ComboboxOption | ComboboxOption[] | null | undefined) => (v == null ? [] : Array.isArray(v) ? v : [v]);

export function Lookup(props: LookupProps) {
  const { search, delay = 250, multiple, value, defaultValue, onChange, noResultsText, ...field } = props;
  const [inner, setInner] = useState<ComboboxOption[]>(() => toList(defaultValue));
  const [term, setTerm] = useState("");
  const [results, setResults] = useState<ComboboxOption[]>([]);
  const [loading, setLoading] = useState(false);
  const [failed, setFailed] = useState(false);
  const seq = useRef(0);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const selected = value !== undefined ? toList(value) : inner;
  const commit = (next: ComboboxOption[]) => {
    setInner(next);
    (onChange as ((v: ComboboxOption[] | ComboboxOption | null) => void) | undefined)?.(multiple ? next : (next[0] ?? null));
  };

  const onInputChange = (text: string) => {
    setTerm(text);
    clearTimeout(timer.current);
    const id = ++seq.current; // any older request is stale from here on
    if (!text.trim()) {
      setResults([]);
      setLoading(false);
      return;
    }
    setLoading(true);
    timer.current = setTimeout(() => {
      search(text.trim()).then(
        (found) => {
          if (id !== seq.current) return;
          setResults(found);
          setFailed(false);
          setLoading(false);
        },
        () => {
          if (id !== seq.current) return;
          setResults([]);
          setFailed(true);
          setLoading(false);
        },
      );
    }, delay);
  };

  const chosen = new Set(selected.map((o) => o.value));
  return (
    <ComboboxField
      {...field}
      mode="autocomplete"
      multiple={multiple}
      // Multi: a record already in the pills is not offered again.
      options={multiple ? results.filter((o) => !chosen.has(o.value)) : results}
      selected={selected}
      loading={loading}
      noResultsText={failed ? t("slds.lookup.searchFailed") : noResultsText}
      onSelect={(o) => commit(multiple ? [...selected, o] : [o])}
      onDeselect={(o) => commit(selected.filter((s) => s.value !== o.value))}
      inputValue={term}
      onInputChange={onInputChange}
    />
  );
}
