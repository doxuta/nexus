// Lookup — blueprint `lookups` (SLDS 2.264.1): async record search on the autocomplete combobox.
import { describe, it, expect, vi } from "vitest";
import { useState } from "react";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Building2 } from "lucide-react";
import type { ComboboxOption } from "@/components/slds/Combobox";
import { Lookup } from "@/components/slds/Lookup";

const icon = { glyph: Building2, name: "account", label: "Khách hàng" };
const ACCOUNTS: ComboboxOption[] = [
  { value: "1", label: "Công ty Acme", meta: "Khách hàng • Hà Nội", icon },
  { value: "2", label: "Acme Logistics", meta: "Khách hàng • Đà Nẵng", icon },
];
const searchAll = (term: string) => Promise.resolve(ACCOUNTS.filter((a) => a.label.toLowerCase().includes(term.toLowerCase())));

function Single(props: { search?: (t: string) => Promise<ComboboxOption[]>; onChange?: (o: ComboboxOption | null) => void }) {
  const [v, setV] = useState<ComboboxOption | null>(null);
  return (
    <Lookup
      label="Khách hàng"
      delay={0}
      search={props.search ?? searchAll}
      value={v}
      onChange={(o) => {
        setV(o);
        props.onChange?.(o);
      }}
    />
  );
}

describe("Lookup", () => {
  it("searches as you type and lists entity results with icon + meta", async () => {
    const search = vi.fn(searchAll);
    render(<Single search={search} />);
    await userEvent.type(screen.getByRole("combobox"), "acme");
    const opts = await screen.findAllByRole("option");
    expect(search).toHaveBeenLastCalledWith("acme");
    expect(opts).toHaveLength(2);
    expect(opts[0]).toHaveClass("slds-listbox__option_entity", "slds-listbox__option_has-meta");
    expect(opts[0].querySelector(".slds-icon-standard-account")).not.toBeNull();
    expect(opts[0].querySelector("mark")).toHaveTextContent("Acme");
    expect(within(opts[0]).getByText("Khách hàng • Hà Nội")).toHaveClass("slds-listbox__option-meta");
  });

  it("shows the field spinner and aria-busy while a search is in flight", async () => {
    let resolve: (r: ComboboxOption[]) => void = () => {};
    render(<Single search={() => new Promise((r) => (resolve = r))} />);
    await userEvent.type(screen.getByRole("combobox"), "a");
    const field = screen.getByRole("combobox").parentElement!;
    expect(field).toHaveClass("slds-input-has-icon_group-right");
    expect(within(field).getByRole("status")).toHaveClass("slds-spinner", "slds-input__spinner");
    await act(async () => resolve(ACCOUNTS));
    expect(await screen.findAllByRole("option")).toHaveLength(2);
    expect(screen.getByRole("listbox")).toHaveAttribute("aria-busy", "false");
  });

  it("ignores a stale response that lands after a newer one", async () => {
    const pending: ((r: ComboboxOption[]) => void)[] = [];
    const search = vi.fn(() => new Promise<ComboboxOption[]>((r) => pending.push(r)));
    render(<Single search={search} />);
    await userEvent.type(screen.getByRole("combobox"), "a");
    await waitFor(() => expect(search).toHaveBeenCalledTimes(1));
    await userEvent.type(screen.getByRole("combobox"), "c");
    await waitFor(() => expect(search).toHaveBeenCalledTimes(2));
    await act(async () => pending[1]([ACCOUNTS[1]]));
    await act(async () => pending[0](ACCOUNTS));
    const opts = await screen.findAllByRole("option");
    expect(opts.map((o) => o.querySelector(".slds-listbox__option-text")!.textContent)).toEqual(["Acme Logistics"]);
  });

  it("single choice: read-only value with the object icon and a remove button (blueprint 'Option Selected')", async () => {
    const onChange = vi.fn();
    render(<Single onChange={onChange} />);
    await userEvent.type(screen.getByRole("combobox"), "acme");
    await screen.findAllByRole("option");
    await userEvent.keyboard("{ArrowDown}{Enter}");
    expect(onChange).toHaveBeenLastCalledWith(ACCOUNTS[0]);
    const value = screen.getByRole("combobox");
    expect(value.tagName).toBe("DIV");
    expect(value).toHaveTextContent("Công ty Acme");
    const field = value.parentElement!;
    expect(field).toHaveClass("slds-input-has-icon_left-right");
    const entity = field.querySelector(".slds-combobox__input-entity-icon")!;
    expect(entity).toHaveClass("slds-icon_container", "slds-icon-standard-account");
    expect(entity).toHaveAttribute("title", "Khách hàng");
    await userEvent.click(screen.getByRole("button", { name: "Bỏ mục đã chọn" }));
    expect(onChange).toHaveBeenLastCalledWith(null);
    expect(screen.getByRole("combobox").tagName).toBe("INPUT");
  });

  it("multi: picks become pills with the object icon and leave the result list", async () => {
    function Multi() {
      const [v, setV] = useState<ComboboxOption[]>([]);
      return <Lookup label="Khách hàng" delay={0} search={searchAll} multiple value={v} onChange={setV} />;
    }
    render(<Multi />);
    await userEvent.type(screen.getByRole("combobox"), "acme");
    await screen.findAllByRole("option");
    await userEvent.keyboard("{ArrowDown}{Enter}");
    const group = screen.getByRole("list", { name: "Các mục đã chọn:" });
    expect(within(group).getByText("Công ty Acme")).toHaveClass("slds-pill__label");
    expect(group.querySelector(".slds-pill__icon_container")).toHaveClass("slds-icon-standard-account");
    await userEvent.type(screen.getByRole("combobox"), "acme");
    await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    expect(screen.getByRole("option")).toHaveTextContent("Acme Logistics");
  });

  it("a failed search says so instead of looking empty", async () => {
    render(<Single search={() => Promise.reject(new Error("503"))} />);
    await userEvent.type(screen.getByRole("combobox"), "acme");
    expect(await screen.findByText("Không tải được kết quả tìm kiếm")).toBeInTheDocument();
  });
});
