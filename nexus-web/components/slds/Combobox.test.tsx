// Combobox — blueprint `combobox` (base select-only, autocomplete, grouped options), SLDS 2.264.1.
// Keyboard per the combobox docs: Down opens and highlights, Up/Down loop, a letter jumps to the first
// match (select-only), Enter chooses and collapses, Esc collapses; focus never leaves the field.
import { describe, it, expect, vi } from "vitest";
import { useState } from "react";
import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Building2 } from "lucide-react";
import { Combobox, ComboboxField, fold, type ComboboxOption } from "@/components/slds/Combobox";

const OPTIONS: ComboboxOption[] = [
  { value: "acc", label: "Khách hàng" },
  { value: "rep", label: "Báo cáo" },
  { value: "con", label: "Liên hệ" },
  { value: "fil", label: "Tệp", disabled: true },
  { value: "lead", label: "Khách tiềm năng" },
];

function Controlled(props: { multiple?: boolean; variant?: "select" | "autocomplete"; options?: ComboboxOption[] }) {
  const [single, setSingle] = useState<string | null>(null);
  const [many, setMany] = useState<string[]>([]);
  return props.multiple ? (
    <Combobox label="Liên quan tới" options={props.options ?? OPTIONS} variant={props.variant} multiple value={many} onChange={setMany} />
  ) : (
    <Combobox label="Liên quan tới" options={props.options ?? OPTIONS} variant={props.variant} value={single} onChange={setSingle} />
  );
}

const combobox = () => screen.getByRole("combobox");

describe("Combobox — select-only (blueprint combobox/base)", () => {
  it("renders the closed blueprint markup", () => {
    const { container } = render(<Controlled />);
    const el = combobox();
    expect(el.tagName).toBe("DIV");
    expect(el).toHaveClass("slds-input_faux", "slds-combobox__input");
    expect(el).not.toHaveClass("slds-combobox__input-value");
    expect(el).toHaveAttribute("tabindex", "0");
    expect(el).toHaveAttribute("aria-haspopup", "listbox");
    expect(el).toHaveAttribute("aria-expanded", "false");
    expect(el.firstElementChild).toHaveClass("slds-truncate");
    expect(el).toHaveTextContent("Chọn một mục…");
    const label = container.querySelector("label.slds-form-element__label")!;
    expect(el).toHaveAttribute("aria-labelledby", label.id);
    const formEl = el.parentElement!;
    expect(formEl).toHaveClass("slds-combobox__form-element", "slds-input-has-icon", "slds-input-has-icon_right");
    expect(formEl).toHaveAttribute("role", "none");
    expect(formEl.querySelector(".slds-icon_container")).toHaveClass("slds-icon-utility-down", "slds-input__icon", "slds-input__icon_right");
    expect(formEl.parentElement).toHaveClass("slds-combobox", "slds-dropdown-trigger", "slds-dropdown-trigger_click");
    expect(formEl.parentElement!.parentElement).toHaveClass("slds-combobox_container");
    expect(screen.queryByRole("listbox")).toBeNull();
  });

  it("opens with Down: listbox markup, first option highlighted, ids wired", async () => {
    render(<Controlled />);
    combobox().focus();
    await userEvent.keyboard("{ArrowDown}");
    const el = combobox();
    const listbox = screen.getByRole("listbox");
    expect(el).toHaveAttribute("aria-expanded", "true");
    expect(el).toHaveAttribute("aria-controls", listbox.id);
    expect(el).toHaveClass("slds-has-focus");
    expect(el.closest(".slds-combobox")).toHaveClass("slds-is-open");
    // Radix would name it role=dialog — the blueprint's role must win.
    expect(listbox).toHaveClass("slds-dropdown", "slds-dropdown_fluid", "slds-dropdown_length-5");
    const ul = listbox.firstElementChild!;
    expect(ul.tagName).toBe("UL");
    expect(ul).toHaveClass("slds-listbox", "slds-listbox_vertical");
    expect(ul).toHaveAttribute("role", "presentation");
    const opts = screen.getAllByRole("option");
    expect(opts).toHaveLength(5);
    expect(opts[0].parentElement).toHaveClass("slds-listbox__item");
    expect(opts[0].parentElement).toHaveAttribute("role", "presentation");
    expect(opts[0]).toHaveClass("slds-media", "slds-listbox__option", "slds-listbox__option_plain", "slds-media_small", "slds-has-focus");
    expect(opts[0]).toHaveAttribute("aria-selected", "true");
    expect(el).toHaveAttribute("aria-activedescendant", opts[0].id);
    expect(opts[0].querySelector(".slds-media__figure.slds-listbox__option-icon")).not.toBeNull();
    expect(opts[0].querySelector(".slds-media__body span[title='Khách hàng']")).not.toBeNull();
    expect(opts[3]).toHaveAttribute("aria-disabled", "true");
    expect(document.activeElement).toBe(el);
  });

  it("Up/Down loop and skip disabled options; Enter chooses and collapses", async () => {
    const onChange = vi.fn();
    render(<Combobox label="Liên quan tới" options={OPTIONS} onChange={onChange} />);
    combobox().focus();
    await userEvent.keyboard("{ArrowDown}{ArrowUp}");
    expect(combobox()).toHaveAttribute("aria-activedescendant", screen.getAllByRole("option")[4].id);
    await userEvent.keyboard("{ArrowUp}");
    // "Tệp" (index 3) is disabled and skipped.
    expect(combobox()).toHaveAttribute("aria-activedescendant", screen.getAllByRole("option")[2].id);
    await userEvent.keyboard("{Enter}");
    expect(onChange).toHaveBeenLastCalledWith("con");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(combobox()).toHaveTextContent("Liên hệ");
    expect(combobox()).toHaveClass("slds-combobox__input-value");
    expect(document.activeElement).toBe(combobox());
  });

  it("re-opening highlights the chosen option, marked with the check", async () => {
    render(<Combobox label="Liên quan tới" options={OPTIONS} defaultValue="rep" />);
    combobox().focus();
    await userEvent.keyboard("{Enter}");
    const chosen = screen.getByRole("option", { name: "Báo cáo" });
    expect(chosen).toHaveClass("slds-is-selected", "slds-has-focus");
    expect(chosen).toHaveAttribute("aria-checked", "true");
    expect(chosen.querySelector(".slds-icon_container.slds-icon-utility-check.slds-current-color svg")).toHaveClass("slds-icon", "slds-icon_x-small");
  });

  it("a letter jumps to the first match, ignoring case and Vietnamese marks", async () => {
    render(<Controlled />);
    combobox().focus();
    await userEvent.keyboard("l");
    expect(combobox()).toHaveAttribute("aria-activedescendant", screen.getByRole("option", { name: "Liên hệ" }).id);
    await userEvent.keyboard("b");
    expect(combobox()).toHaveAttribute("aria-activedescendant", screen.getByRole("option", { name: "Báo cáo" }).id);
  });

  it("Esc closes without choosing; a click toggles; clicking an option chooses it", async () => {
    const onChange = vi.fn();
    render(<Combobox label="Liên quan tới" options={OPTIONS} onChange={onChange} />);
    await userEvent.click(combobox());
    expect(screen.getByRole("listbox")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(onChange).not.toHaveBeenCalled();
    await userEvent.click(combobox());
    await userEvent.click(screen.getByRole("option", { name: "Báo cáo" }));
    expect(onChange).toHaveBeenCalledWith("rep");
    expect(screen.queryByRole("listbox")).toBeNull();
  });

  it("multi-select: options toggle and stay open; the field counts; pills sit in the selection group", async () => {
    render(<Controlled multiple />);
    combobox().focus();
    await userEvent.keyboard("{ArrowDown}{Enter}{ArrowDown}{Enter}");
    expect(screen.getByRole("listbox")).toHaveAttribute("aria-multiselectable", "true");
    const opts = screen.getAllByRole("option");
    expect(opts[0]).toHaveAttribute("aria-checked", "true");
    expect(opts[1]).toHaveAttribute("aria-checked", "true");
    expect(combobox()).toHaveTextContent("Đã chọn 2 mục");
    await userEvent.keyboard("{Escape}");
    const group = screen.getByRole("list", { name: "Các mục đã chọn:" }).parentElement!;
    expect(group).toHaveClass("slds-listbox_selection-group");
    expect(within(group).getAllByRole("listitem")).toHaveLength(2);
    await userEvent.click(within(group).getByRole("button", { name: "Loại bỏ Khách hàng" }));
    expect(combobox()).toHaveTextContent("Báo cáo");
  });

  it("grouped options: one ul[role=group] per heading, header is presentation", async () => {
    const grouped: ComboboxOption[] = [
      { value: "a", label: "Acme", group: "Nhóm Một" },
      { value: "b", label: "Beta", group: "Nhóm Hai" },
      { value: "c", label: "Cánh Diều", group: "Nhóm Một" },
    ];
    render(<Combobox label="Tài khoản" options={grouped} />);
    combobox().focus();
    await userEvent.keyboard("{ArrowDown}");
    const groups = screen.getAllByRole("group");
    expect(groups.map((g) => g.getAttribute("aria-label"))).toEqual(["Nhóm Một", "Nhóm Hai"]);
    for (const g of groups) expect(g).toHaveClass("slds-listbox", "slds-listbox_vertical");
    const header = groups[0].querySelector("h3.slds-listbox__option-header")!;
    expect(header).toHaveTextContent("Nhóm Một");
    expect(header).toHaveAttribute("role", "presentation");
    expect(within(groups[0]).getAllByRole("option").map((o) => o.textContent)).toEqual(["Acme", "Cánh Diều"]);
    // Keyboard order follows the rendered order.
    await userEvent.keyboard("{ArrowDown}");
    expect(combobox()).toHaveAttribute("aria-activedescendant", screen.getByRole("option", { name: "Cánh Diều" }).id);
  });

  it("loading: spinner row at the end and aria-busy", async () => {
    render(<Combobox label="Liên quan tới" options={OPTIONS.slice(0, 2)} loading />);
    combobox().focus();
    await userEvent.keyboard("{ArrowDown}");
    const listbox = screen.getByRole("listbox");
    expect(listbox).toHaveAttribute("aria-busy", "true");
    const spinner = within(listbox).getByRole("status");
    expect(spinner).toHaveClass("slds-spinner", "slds-spinner_x-small", "slds-spinner_inline");
    expect(spinner.parentElement).toHaveClass("slds-align_absolute-center", "slds-p-top_medium");
  });

  it("disabled: no tab stop, does not open", async () => {
    render(<Combobox label="Liên quan tới" options={OPTIONS} disabled />);
    expect(combobox()).toHaveAttribute("tabindex", "-1");
    expect(combobox()).toHaveAttribute("aria-disabled", "true");
    expect(combobox()).toHaveClass("slds-is-disabled");
    await userEvent.click(combobox());
    expect(screen.queryByRole("listbox")).toBeNull();
  });

  it("error: slds-has-error on the form element, message wired by aria-describedby", () => {
    const { container } = render(<Combobox label="Liên quan tới" options={OPTIONS} required error="Hãy chọn một mục." />);
    expect(container.firstElementChild).toHaveClass("slds-form-element", "slds-has-error");
    const help = screen.getByText("Hãy chọn một mục.");
    expect(combobox()).toHaveAttribute("aria-describedby", help.id);
    expect(combobox()).toHaveAttribute("aria-invalid", "true");
    expect(combobox()).toHaveAttribute("aria-required", "true");
  });
});

describe("Combobox — autocomplete (blueprint combobox/autocomplete)", () => {
  it("renders an input combobox with the search icon", () => {
    render(<Controlled variant="autocomplete" />);
    const input = combobox();
    expect(input.tagName).toBe("INPUT");
    expect(input).toHaveClass("slds-input", "slds-combobox__input");
    expect(input).toHaveAttribute("aria-autocomplete", "list");
    expect(input).toHaveAttribute("autocomplete", "off");
    expect(input).toHaveAttribute("placeholder", "Tìm kiếm…");
    expect(input.parentElement!.querySelector(".slds-icon_container")).toHaveClass("slds-icon-utility-search");
  });

  it("typing filters (diacritic-insensitive) and marks the match; nothing highlighted until an arrow", async () => {
    render(<Controlled variant="autocomplete" />);
    await userEvent.type(combobox(), "khach");
    const opts = screen.getAllByRole("option");
    expect(opts.map((o) => o.textContent)).toEqual(["Khách hàng", "Khách tiềm năng"]);
    expect(opts[0].querySelector("mark")).toHaveTextContent("Khách");
    expect(combobox()).not.toHaveAttribute("aria-activedescendant");
    expect(combobox()).toHaveClass("slds-combobox__input-value");
    await userEvent.keyboard("{ArrowDown}{ArrowDown}{Enter}");
    // Single choice: the input becomes the read-only value with a remove button (blueprint "Selected an option").
    const value = combobox();
    expect(value.tagName).toBe("DIV");
    expect(value).toHaveTextContent("Khách tiềm năng");
    expect(value.closest(".slds-combobox_container")).toHaveClass("slds-has-selection");
    expect(document.activeElement).toBe(value);
    await userEvent.click(screen.getByRole("button", { name: "Bỏ mục đã chọn" }));
    expect(combobox().tagName).toBe("INPUT");
    expect(combobox()).toHaveValue("");
    expect(document.activeElement).toBe(combobox());
  });

  it("typing also matches an option's meta line (e.g. a person's email under their name)", async () => {
    const people: ComboboxOption[] = [
      { value: "a1", label: "Quản trị Nexus", meta: "admin@example.com" },
      { value: "u2", label: "Trần Sales Rep", meta: "test@example.com" },
    ];
    render(<Combobox label="Người tạo" variant="autocomplete" options={people} />);
    await userEvent.type(combobox(), "test@example");
    const opts = screen.getAllByRole("option");
    expect(opts).toHaveLength(1);
    expect(opts[0]).toHaveTextContent("Trần Sales Rep");
  });

  it("no match: an empty-state row", async () => {
    render(<Controlled variant="autocomplete" />);
    await userEvent.type(combobox(), "zzz");
    expect(screen.queryAllByRole("option")).toHaveLength(0);
    expect(within(screen.getByRole("listbox")).getByText("Không có kết quả phù hợp")).toBeInTheDocument();
  });

  it("multi: input stays, text clears after each pick, pills below", async () => {
    render(<Controlled variant="autocomplete" multiple />);
    await userEvent.type(combobox(), "bao");
    await userEvent.keyboard("{ArrowDown}{Enter}");
    expect(combobox()).toHaveValue("");
    expect(combobox().tagName).toBe("INPUT");
    const group = screen.getByRole("list", { name: "Các mục đã chọn:" });
    expect(within(group).getByText("Báo cáo")).toBeInTheDocument();
    // Typing again is a plain search box, not the locked-value inset of slds-has-selection.
    await userEvent.type(combobox(), "kh");
    expect(combobox().closest(".slds-combobox_container")).toHaveClass("slds-has-selection");
    expect(combobox()).not.toHaveClass("slds-combobox__input-value");
  });

  it("entity options: object icon + text/meta classes, length-with-icon-7", async () => {
    const accounts: ComboboxOption[] = [
      { value: "1", label: "Công ty Acme", meta: "Khách hàng • Hà Nội", icon: { glyph: Building2, name: "account", label: "Khách hàng" } },
      { value: "2", label: "Cánh Diều", icon: { glyph: Building2, name: "account", label: "Khách hàng" } },
    ];
    render(<Combobox label="Khách hàng" variant="autocomplete" options={accounts} />);
    await userEvent.click(combobox());
    expect(screen.getByRole("listbox")).toHaveClass("slds-dropdown_length-with-icon-7");
    const [withMeta, plain] = screen.getAllByRole("option");
    expect(withMeta).toHaveClass("slds-listbox__option_entity", "slds-listbox__option_has-meta");
    expect(plain).toHaveClass("slds-listbox__option_entity", "slds-media_center");
    expect(withMeta.querySelector(".slds-listbox__option-icon .slds-icon_container")).toHaveClass("slds-icon-standard-account");
    expect(withMeta.querySelector(".slds-listbox__option-text.slds-listbox__option-text_entity")).toHaveTextContent("Công ty Acme");
    expect(withMeta.querySelector(".slds-listbox__option-meta.slds-listbox__option-meta_entity")).toHaveTextContent("Khách hàng • Hà Nội");
  });

  it("Tab closes the listbox and moves on", async () => {
    render(
      <>
        <Controlled variant="autocomplete" />
        <button type="button">Sau</button>
      </>,
    );
    await userEvent.click(combobox());
    expect(screen.getByRole("listbox")).toBeInTheDocument();
    await userEvent.tab();
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Sau" }));
  });
});

describe("Combobox — leaving the field and emptying the selection", () => {
  function Late({ mode, options }: { mode: "select" | "autocomplete"; options: ComboboxOption[] }) {
    const [text, setText] = useState("");
    return (
      <>
        <ComboboxField
          label="Khách hàng"
          mode={mode}
          options={options}
          selected={[]}
          onSelect={() => {}}
          onDeselect={() => {}}
          inputValue={text}
          onInputChange={setText}
          noResultsText={false}
        />
        <input aria-label="Khác" />
      </>
    );
  }

  it.each(["autocomplete", "select"] as const)("%s: options that arrive after focus left do not pop the listbox open", async (mode) => {
    const { rerender } = render(<Late mode={mode} options={[]} />);
    if (mode === "autocomplete") await userEvent.type(combobox(), "ac");
    else await userEvent.click(combobox());
    expect(screen.queryByRole("listbox")).toBeNull();
    await userEvent.click(screen.getByRole("textbox", { name: "Khác" }));
    rerender(<Late mode={mode} options={OPTIONS} />);
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(combobox()).toHaveAttribute("aria-expanded", "false");
  });

  it.each(["autocomplete", "select"] as const)("%s: focus moving into its own listbox (scrollbar drag) keeps it open", async (mode) => {
    render(<Late mode={mode} options={OPTIONS} />);
    await userEvent.click(combobox());
    const listbox = screen.getByRole("listbox");
    act(() => listbox.focus());
    expect(screen.getByRole("listbox")).toBeInTheDocument();
    expect(combobox()).toHaveAttribute("aria-expanded", "true");
  });

  it("select multi: Delete on the last pill hands focus to the field", async () => {
    render(<Combobox label="Liên quan tới" options={OPTIONS} multiple defaultValue={["acc"]} />);
    screen.getByRole("button", { name: "Loại bỏ Khách hàng" }).focus();
    await userEvent.keyboard("{ArrowRight}{Delete}");
    expect(screen.queryByRole("button", { name: "Loại bỏ Khách hàng" })).toBeNull();
    expect(document.activeElement).toBe(combobox());
  });

  it("autocomplete multi: clicking remove on the last pill hands focus to the input", async () => {
    render(<Combobox label="Liên quan tới" variant="autocomplete" options={OPTIONS} multiple defaultValue={["acc"]} />);
    await userEvent.click(screen.getByRole("button", { name: "Loại bỏ Khách hàng" }));
    expect(screen.queryByRole("button", { name: "Loại bỏ Khách hàng" })).toBeNull();
    expect(document.activeElement).toBe(combobox());
  });
});

describe("fold", () => {
  it("drops case and Vietnamese marks one char per char", () => {
    expect(fold("Hà Nội Đông")).toBe("ha noi dong");
    expect(fold("Ặỹữờ").length).toBe(4);
  });
});

describe("Combobox — flat option meta (#301)", () => {
  it("draws a secondary line for a flat option with meta, and still matches typing on it", async () => {
    const user = userEvent.setup();
    const opts: ComboboxOption[] = [
      { value: "a", label: "Khách hàng", meta: "crm_customers · CRM" },
      { value: "b", label: "Khách hàng", meta: "dental_customers · Nha khoa" },
      { value: "c", label: "Cơ hội" },
    ];
    render(<Controlled variant="autocomplete" options={opts} />);
    await user.click(combobox());
    const metas = document.querySelectorAll(".slds-listbox__option-meta");
    expect(metas).toHaveLength(2);
    expect(metas[0]).toHaveTextContent("crm_customers · CRM");
    await user.keyboard("nha");
    expect(screen.getAllByRole("option")).toHaveLength(1);
  });

  it("an option without meta keeps the old plain markup", async () => {
    const user = userEvent.setup();
    render(<Controlled options={[{ value: "c", label: "Cơ hội" }]} />);
    await user.click(combobox());
    expect(document.querySelector(".slds-listbox__option-meta")).toBeNull();
    expect(screen.getByRole("option")).toHaveClass("slds-listbox__option_plain", "slds-media_small");
  });
});
