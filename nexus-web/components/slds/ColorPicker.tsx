"use client";

// SLDS color picker (blueprint `color-picker` base, SLDS 2.264.1):
//   div.slds-color-picker
//     > div.slds-form-element.slds-color-picker__summary[.slds-has-error]
//         > label.slds-color-picker__summary-label + div.slds-form-element__control
//           > button.slds-color-picker__summary-button.slds-button_icon-more (span.slds-swatch + down glyph)
//           + div.slds-color-picker__summary-input > input.slds-input  [+ p.slds-form-error]
//     + section.slds-popover.slds-color-picker__selector[role=dialog]
//         > div.slds-popover__body > div.slds-media > div.slds-media__body > tabs:
//             "Mặc định" = ul.slds-color-picker__swatches[role=listbox] > li > a[role=option] > span.slds-swatch
//             "Tuỳ chỉnh" = div.slds-color-picker__custom (range + indicator, hue slider + preview, Hex/R/G/B)
//         + footer.slds-popover__footer > div.slds-color-picker__selector-footer (Huỷ · Xong)
// Behaviour: @radix-ui/react-popover (Esc, click-outside, focus in and back to the trigger). `modal`
// makes it the keyboard focus trap the popover docs ask for: the summary hex input sits between the
// trigger and the popover in the DOM, so an untrapped Tab would leave (and close) it. On open Radix
// focuses the first focusable element (the active tab). The popover edits a draft; only "Xong" (or
// typing a valid hex in the summary input) commits. Value is "#rrggbb" lowercase or "" (none — the
// swatch then shows `placeholderColor`).
// Deviation: the popover is positioned by Radix (fixed) instead of flowing under the summary; SLDS's
// margin-top still spaces it. The swatch colours are data (the value / the blueprint's preset list),
// the only place a colour is written inline.
import * as PopoverPrimitive from "@radix-ui/react-popover";
import clsx from "clsx";
import { ChevronDown } from "lucide-react";
import { useId, useRef, useState, type KeyboardEvent, type PointerEvent } from "react";
import { t } from "@/lib/i18n";
import { fill } from "./DataTable";
import { Tabs } from "./Tabs";

/** The blueprint's preset list (color-picker/predefined-only). */
export const DEFAULT_SWATCHES = [
  "#e3abec", "#c2dbf7", "#9fd6ff", "#9de7da", "#9df0c0", "#fff099", "#fed49a",
  "#d073e0", "#86baf3", "#5ebbff", "#44d8be", "#3be282", "#ffe654", "#ffb758",
  "#bd35bd", "#5779c1", "#5679c0", "#00aea9", "#3cba4c", "#f5bc25", "#f99221",
  "#580d8c", "#001970", "#0a2399", "#0b7477", "#0b6b50", "#b67e11", "#b85d0d",
];

type Hsv = { h: number; s: number; v: number }; // h 0–360, s/v 0–100
type Rgb = [number, number, number];

const HEX_RE = /^#[0-9a-f]{6}$/;
const clamp = (n: number, lo: number, hi: number) => Math.min(hi, Math.max(lo, n));

/** "#ABC" / "abc123" / " #AbC123 " → "#aabbcc" / "#abc123"; null when it is not a colour. */
export function normalizeHex(text: string): string | null {
  let h = text.trim().toLowerCase();
  if (!h.startsWith("#")) h = `#${h}`;
  if (/^#[0-9a-f]{3}$/.test(h)) h = `#${[...h.slice(1)].map((c) => c + c).join("")}`;
  return HEX_RE.test(h) ? h : null;
}

const hexToRgb = (hex: string): Rgb => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16)) as Rgb;
const rgbToHex = (rgb: Rgb) => `#${rgb.map((n) => Math.round(n).toString(16).padStart(2, "0")).join("")}`;

function rgbToHsv([r, g, b]: Rgb, keepHue = 0): Hsv {
  const [rr, gg, bb] = [r / 255, g / 255, b / 255];
  const max = Math.max(rr, gg, bb);
  const d = max - Math.min(rr, gg, bb);
  let h = keepHue; // grey has no hue: keep the slider where it was
  if (d) {
    if (max === rr) h = 60 * (((gg - bb) / d) % 6);
    else if (max === gg) h = 60 * ((bb - rr) / d + 2);
    else h = 60 * ((rr - gg) / d + 4);
    if (h < 0) h += 360;
  }
  return { h: Math.round(h), s: max ? Math.round((d / max) * 100) : 0, v: Math.round(max * 100) };
}

function hsvToRgb({ h, s, v }: Hsv): Rgb {
  const f = (n: number) => {
    const k = (n + h / 60) % 6;
    return (v / 100) * (1 - (s / 100) * Math.max(0, Math.min(k, 4 - k, 1))) * 255;
  };
  return [f(5), f(3), f(1)];
}

const hsvToHex = (hsv: Hsv) => rgbToHex(hsvToRgb(hsv));

/** The popover's working colour: the exact hex (a swatch or typed hex must round-trip unchanged)
 *  plus the hue, which a grey cannot carry but the hue slider must keep. */
type Draft = { hex: string; h: number };
const draftOf = (hex: string, keepHue = 0): Draft => ({ hex, h: rgbToHsv(hexToRgb(hex), keepHue).h });

export interface ColorPickerProps {
  label: string;
  /** "#rrggbb" or "" (no colour chosen). */
  value: string;
  onChange: (hex: string) => void;
  /** Swatch colour while `value` is "" (e.g. the stock brand colour). */
  placeholderColor?: string;
  swatches?: string[];
  /** Summary error from the owner (the server rejected the colour…). */
  error?: string;
  id?: string;
  className?: string;
}

export function ColorPicker({
  label,
  value,
  onChange,
  placeholderColor = "#0176d3",
  swatches = DEFAULT_SWATCHES,
  error,
  id,
  className,
}: ColorPickerProps) {
  const auto = useId();
  const summaryId = id ?? `${auto}-summary`;
  const summaryErrorId = `${summaryId}-error`;
  const shown = value || placeholderColor;

  // Summary input: shows `value`, except while the user is typing something not yet a colour.
  const [typed, setTyped] = useState<string | null>(null);
  const [typedBad, setTypedBad] = useState(false);
  const summaryError = error ?? (typedBad ? t("slds.colorPicker.invalid") : undefined);

  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<Draft>(() => draftOf(normalizeHex(shown) ?? placeholderColor));
  const [tab, setTab] = useState("default");

  const openChange = (next: boolean) => {
    if (next) {
      setDraft(draftOf(normalizeHex(shown) ?? placeholderColor));
      setTab("default");
    }
    setOpen(next);
  };
  const commit = (hex: string) => {
    setTyped(null);
    setTypedBad(false);
    onChange(hex);
  };

  return (
    <div className={clsx("slds-color-picker", className)}>
      <PopoverPrimitive.Root open={open} onOpenChange={openChange} modal>
        <div className={clsx("slds-form-element slds-color-picker__summary", summaryError && "slds-has-error")}>
          <label className="slds-form-element__label slds-color-picker__summary-label" htmlFor={summaryId}>
            {label}
          </label>
          <div className="slds-form-element__control">
            <PopoverPrimitive.Trigger asChild>
              <button type="button" className="slds-button slds-color-picker__summary-button slds-button_icon slds-button_icon-more" title={t("slds.colorPicker.title")}>
                <span className="slds-swatch" style={{ background: shown }}>
                  <span className="slds-assistive-text">{shown}</span>
                </span>
                <ChevronDown aria-hidden="true" className="slds-button__icon slds-button__icon_small slds-m-left_xx-small nx-lucide" />
                <span className="slds-assistive-text">{fill(t("slds.colorPicker.current"), { color: shown })}</span>
              </button>
            </PopoverPrimitive.Trigger>
            <div className="slds-color-picker__summary-input">
              <input
                id={summaryId}
                type="text"
                className="slds-input"
                value={typed ?? value}
                placeholder={placeholderColor}
                aria-invalid={summaryError ? true : undefined}
                aria-describedby={summaryError ? summaryErrorId : undefined}
                onChange={(e) => {
                  const text = e.target.value;
                  const hex = normalizeHex(text);
                  setTypedBad(false);
                  if (text.trim() === "") commit("");
                  else if (hex && text.trim().length >= 7) commit(hex);
                  else setTyped(text);
                }}
                onBlur={() => {
                  if (typed === null) return;
                  const hex = normalizeHex(typed);
                  if (hex) commit(hex);
                  else setTypedBad(true);
                }}
              />
            </div>
            {summaryError && (
              <p className="slds-form-error" id={summaryErrorId}>
                {summaryError}
              </p>
            )}
          </div>
        </div>
        <PopoverPrimitive.Content asChild side="bottom" align="start">
          <section className="slds-popover slds-color-picker__selector" role="dialog" aria-label={t("slds.colorPicker.title")}>
            <div className="slds-popover__body">
              <div className="slds-media">
                <div className="slds-media__body">
                  <Tabs
                    value={tab}
                    onValueChange={setTab}
                    items={[
                      {
                        value: "default",
                        label: t("slds.colorPicker.default"),
                        content: <Swatches swatches={swatches} selected={draft.hex} onSelect={(hex) => setDraft(draftOf(hex, draft.h))} />,
                      },
                      {
                        value: "custom",
                        label: t("slds.colorPicker.custom"),
                        content: <Custom draft={draft} onDraft={setDraft} />,
                      },
                    ]}
                  />
                </div>
              </div>
            </div>
            <footer className="slds-popover__footer">
              <div className="slds-color-picker__selector-footer">
                <PopoverPrimitive.Close asChild>
                  <button type="button" className="slds-button slds-button_neutral">
                    {t("action.cancel")}
                  </button>
                </PopoverPrimitive.Close>
                <button
                  type="button"
                  className="slds-button slds-button_brand"
                  onClick={() => {
                    commit(draft.hex);
                    setOpen(false);
                  }}
                >
                  {t("slds.colorPicker.done")}
                </button>
              </div>
            </footer>
          </section>
        </PopoverPrimitive.Content>
      </PopoverPrimitive.Root>
    </div>
  );
}

// "Mặc định" tab: a listbox of swatches with one tab stop (the selected one, else the first);
// arrow keys move AND select, like the radio group SLDS describes for it.
function Swatches({ swatches, selected, onSelect }: { swatches: string[]; selected: string; onSelect: (hex: string) => void }) {
  const list = useRef<HTMLUListElement>(null);
  const current = swatches.indexOf(selected);
  const stop = current >= 0 ? current : 0;
  const move = (e: KeyboardEvent, i: number) => {
    const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[e.key];
    if (!step) return;
    e.preventDefault();
    const next = (i + step + swatches.length) % swatches.length;
    onSelect(swatches[next]);
    list.current?.querySelectorAll<HTMLElement>("[role=option]")[next]?.focus();
  };
  return (
    <ul ref={list} className="slds-color-picker__swatches" role="listbox" aria-label={t("slds.colorPicker.presets")}>
      {swatches.map((hex, i) => (
        <li key={hex} className="slds-color-picker__swatch" role="presentation">
          <a
            className="slds-color-picker__swatch-trigger"
            href="#"
            role="option"
            aria-selected={i === current}
            tabIndex={i === stop ? 0 : -1}
            onClick={(e) => {
              e.preventDefault();
              onSelect(hex);
            }}
            onKeyDown={(e) => move(e, i)}
          >
            <span className="slds-swatch" style={{ background: hex }}>
              <span className="slds-assistive-text">{hex}</span>
            </span>
          </a>
        </li>
      ))}
    </ul>
  );
}

// "Tuỳ chỉnh" tab: saturation (x) / brightness (y) range, hue slider + preview, Hex/R/G/B inputs.
function Custom({ draft, onDraft }: { draft: Draft; onDraft: (draft: Draft) => void }) {
  const id = useId();
  const { hex, h } = draft;
  const rgb = hexToRgb(hex);
  const { s, v } = rgbToHsv(rgb, h);
  const fromHsv = (next: Hsv) => onDraft({ hex: hsvToHex(next), h: next.h });
  // The field being typed in keeps its raw text until it parses; the others follow the draft.
  const [editing, setEditing] = useState<{ field: string; text: string } | null>(null);
  const shownText = (field: string, fallback: string) => (editing?.field === field ? editing.text : fallback);
  const hexBad = editing?.field === "hex" && normalizeHex(editing.text) === null;

  const fromPointer = (e: PointerEvent<HTMLDivElement>) => {
    const box = e.currentTarget.getBoundingClientRect();
    if (!box.width || !box.height) return;
    fromHsv({
      h,
      s: clamp(((e.clientX - box.left) / box.width) * 100, 0, 100),
      v: clamp(((box.bottom - e.clientY) / box.height) * 100, 0, 100),
    });
  };
  const onIndicatorKey = (e: KeyboardEvent) => {
    const step = e.shiftKey ? 10 : 1;
    const d = { ArrowRight: [step, 0], ArrowLeft: [-step, 0], ArrowUp: [0, step], ArrowDown: [0, -step] }[e.key];
    if (!d) return;
    e.preventDefault();
    fromHsv({ h, s: clamp(s + d[0], 0, 100), v: clamp(v + d[1], 0, 100) });
  };
  const channel = (i: 0 | 1 | 2, text: string) => {
    setEditing({ field: `c${i}`, text });
    const n = Number(text);
    if (text.trim() === "" || !Number.isInteger(n) || n < 0 || n > 255) return;
    const next = [...rgb] as Rgb;
    next[i] = n;
    onDraft(draftOf(rgbToHex(next), h));
  };
  const channels = [
    ["c0", "R", "slds.colorPicker.red"],
    ["c1", "G", "slds.colorPicker.green"],
    ["c2", "B", "slds.colorPicker.blue"],
  ] as const;

  return (
    <div className="slds-color-picker__custom">
      <p id={`${id}-help`} className="slds-assistive-text">
        {t("slds.colorPicker.rangeHelp")}
      </p>
      <div
        className="slds-color-picker__custom-range"
        style={{ background: `hsl(${h}, 100%, 50%)` }}
        onPointerDown={(e) => {
          e.currentTarget.setPointerCapture?.(e.pointerId);
          fromPointer(e);
        }}
        onPointerMove={(e) => {
          if (e.currentTarget.hasPointerCapture?.(e.pointerId)) fromPointer(e);
        }}
      >
        <button
          type="button"
          className="slds-color-picker__range-indicator"
          style={{ bottom: `${v}%`, left: `${s}%` }}
          aria-live="assertive"
          aria-atomic="true"
          aria-describedby={`${id}-help`}
          onKeyDown={onIndicatorKey}
        >
          <span className="slds-assistive-text">{`hex ${hex}`}</span>
        </button>
      </div>
      <div className="slds-color-picker__hue-and-preview">
        <label className="slds-assistive-text" htmlFor={`${id}-hue`}>
          {t("slds.colorPicker.hue")}
        </label>
        <input
          type="range"
          className="slds-color-picker__hue-slider"
          min={0}
          max={360}
          id={`${id}-hue`}
          value={h}
          onChange={(e) => fromHsv({ h: Number(e.target.value), s, v })}
        />
        <span className="slds-swatch" style={{ background: hex }}>
          <span className="slds-assistive-text" aria-hidden="true">
            {hex}
          </span>
        </span>
      </div>
      <div className="slds-color-picker__custom-inputs">
        <div className={clsx("slds-form-element slds-color-picker__input-custom-hex", hexBad && "slds-has-error")}>
          <label className="slds-form-element__label" htmlFor={`${id}-hex`}>
            {t("slds.colorPicker.hex")}
          </label>
          <div className="slds-form-element__control">
            <input
              id={`${id}-hex`}
              type="text"
              className="slds-input"
              value={shownText("hex", hex)}
              aria-invalid={hexBad ? true : undefined}
              aria-describedby={hexBad ? `${id}-error` : undefined}
              onChange={(e) => {
                setEditing({ field: "hex", text: e.target.value });
                const next = normalizeHex(e.target.value);
                if (next) onDraft(draftOf(next, h));
              }}
              onBlur={() => {
                if (!hexBad) setEditing(null);
              }}
            />
          </div>
        </div>
        {channels.map(([field, letter, name], i) => (
          <div key={field} className="slds-form-element">
            <label className="slds-form-element__label" htmlFor={`${id}-${field}`}>
              <abbr title={t(name)}>{letter}</abbr>
            </label>
            <div className="slds-form-element__control">
              <input
                id={`${id}-${field}`}
                type="text"
                inputMode="numeric"
                className="slds-input"
                value={shownText(field, String(rgb[i]))}
                onChange={(e) => channel(i as 0 | 1 | 2, e.target.value)}
                onBlur={() => setEditing(null)}
              />
            </div>
          </div>
        ))}
      </div>
      {hexBad && (
        <p className="slds-form-error" id={`${id}-error`}>
          {t("slds.colorPicker.invalid")}
        </p>
      )}
    </div>
  );
}
