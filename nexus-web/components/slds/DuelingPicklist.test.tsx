// DuelingPicklist — blueprint `dueling-picklist` (SLDS 2.264.1): two listboxes + move/reorder buttons,
// keyboard model from the blueprint docs (second multi-select model + Space drag-and-drop mode).
import { describe, it, expect } from "vitest";
import { useState } from "react";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { DuelingPicklist, moveWithin, type DuelingOption } from "@/components/slds/DuelingPicklist";

const OPTS: DuelingOption[] = [
  { value: "vi", label: "Tiếng Việt" },
  { value: "en", label: "Tiếng Anh" },
  { value: "ja", label: "Tiếng Nhật" },
  { value: "ko", label: "Tiếng Hàn" },
  { value: "fr", label: "Tiếng Pháp" },
];

function Harness(props: { initial?: string[]; reorder?: boolean; options?: DuelingOption[] }) {
  const [v, setV] = useState<string[]>(props.initial ?? ["ko", "fr"]);
  return (
    <>
      <DuelingPicklist label="Ngôn ngữ hỗ trợ" options={props.options ?? OPTS} value={v} onChange={setV} reorder={props.reorder} />
      <output data-testid="value">{v.join(",")}</output>
    </>
  );
}

const lists = () => screen.getAllByRole("listbox");
const texts = (list: HTMLElement) => within(list).queryAllByRole("option").map((o) => o.textContent);
const value = () => screen.getByTestId("value").textContent;

describe("moveWithin", () => {
  it("moves the selected block one step, stops at the edge and at locked neighbours", () => {
    expect(moveWithin(["a", "b", "c", "d"], new Set(["b", "c"]), -1)).toEqual(["b", "c", "a", "d"]);
    expect(moveWithin(["a", "b", "c", "d"], new Set(["a", "b"]), -1)).toEqual(["a", "b", "c", "d"]);
    expect(moveWithin(["a", "b", "c", "d"], new Set(["c"]), 1)).toEqual(["a", "b", "d", "c"]);
    expect(moveWithin(["a", "b", "c"], new Set(["c"]), -1, new Set(["b"]))).toEqual(["a", "b", "c"]);
  });
});

describe("DuelingPicklist", () => {
  it("markup: group labelled by the legend label; two multiselectable listboxes described by the drag instructions", () => {
    render(<Harness />);
    const group = screen.getByRole("group", { name: "Ngôn ngữ hỗ trợ" });
    expect(group).toHaveClass("slds-form-element");
    expect(group.querySelector(".slds-form-element__label")).toHaveClass("slds-form-element__legend");
    const dueling = group.querySelector(".slds-form-element__control > .slds-dueling-list")!;
    expect(Array.from(dueling.children).filter((c) => c.classList.contains("slds-dueling-list__column"))).toHaveLength(4);
    const [source, target] = lists();
    expect(source).toHaveAccessibleName("Có sẵn");
    expect(target).toHaveAccessibleName("Đã chọn");
    for (const l of [source, target]) {
      expect(l).toHaveClass("slds-listbox", "slds-listbox_vertical");
      expect(l).toHaveAttribute("aria-multiselectable", "true");
      expect(l.parentElement).toHaveClass("slds-dueling-list__options");
      expect(document.getElementById(l.getAttribute("aria-describedby")!)).toHaveTextContent("Nhấn phím cách");
    }
    expect(texts(source)).toEqual(["Tiếng Việt", "Tiếng Anh", "Tiếng Nhật"]);
    expect(texts(target)).toEqual(["Tiếng Hàn", "Tiếng Pháp"]);
    const first = within(source).getAllByRole("option")[0];
    expect(first).toHaveClass("slds-listbox__option", "slds-listbox__option_plain", "slds-media", "slds-media_small", "slds-media_inline");
    expect(first).toHaveAttribute("aria-selected", "false");
    expect(first).toHaveAttribute("tabindex", "0");
    expect(within(source).getAllByRole("option")[1]).toHaveAttribute("tabindex", "-1");
    expect(screen.getByRole("button", { name: "Chuyển mục đang chọn sang Đã chọn" })).toHaveClass("slds-button_icon-container");
    expect(screen.getByRole("button", { name: "Chuyển mục đang chọn lên" })).toBeInTheDocument();
  });

  it("click selects, the move button carries the selection across (unselected, appended) and keeps focus", async () => {
    render(<Harness />);
    const [source] = lists();
    await userEvent.click(within(source).getByRole("option", { name: "Tiếng Anh" }));
    expect(within(source).getByRole("option", { name: "Tiếng Anh" })).toHaveAttribute("aria-selected", "true");
    expect(within(source).getByRole("option", { name: "Tiếng Anh" })).toHaveClass("slds-is-selected");
    const move = screen.getByRole("button", { name: "Chuyển mục đang chọn sang Đã chọn" });
    await userEvent.click(move);
    expect(value()).toBe("ko,fr,en");
    expect(within(lists()[1]).getByRole("option", { name: "Tiếng Anh" })).toHaveAttribute("aria-selected", "false");
    expect(move).toHaveFocus();
    expect(screen.getByText(/Tiếng Anh: đã chuyển sang Đã chọn/)).toHaveAttribute("aria-live", "assertive");
  });

  it("keyboard: arrows move focus + selection, Shift extends, Ctrl+Right moves across with focus following", async () => {
    render(<Harness />);
    const [source] = lists();
    within(source).getAllByRole("option")[0].focus();
    await userEvent.keyboard("{ArrowDown}");
    const en = within(source).getByRole("option", { name: "Tiếng Anh" });
    expect(en).toHaveFocus();
    expect(en).toHaveAttribute("aria-selected", "true");
    expect(within(source).getByRole("option", { name: "Tiếng Việt" })).toHaveAttribute("aria-selected", "false");
    await userEvent.keyboard("{Shift>}{ArrowDown}{/Shift}");
    expect(within(source).getAllByRole("option", { selected: true }).map((o) => o.textContent)).toEqual(["Tiếng Anh", "Tiếng Nhật"]);
    await userEvent.keyboard("{Control>}{ArrowRight}{/Control}");
    expect(value()).toBe("ko,fr,en,ja");
    const target = lists()[1];
    expect(within(target).getByRole("option", { name: "Tiếng Nhật" })).toHaveFocus();
    expect(within(target).getAllByRole("option", { selected: true }).map((o) => o.textContent)).toEqual(["Tiếng Anh", "Tiếng Nhật"]);
  });

  it("keyboard: Ctrl+Space toggles, Ctrl+A selects all, Ctrl+Left sends back to the source", async () => {
    render(<Harness />);
    const target = lists()[1];
    within(target).getAllByRole("option")[0].focus();
    await userEvent.keyboard("{Control>}a{/Control}");
    expect(within(target).getAllByRole("option", { selected: true })).toHaveLength(2);
    await userEvent.keyboard("{Control>} {/Control}");
    expect(within(target).getAllByRole("option", { selected: true }).map((o) => o.textContent)).toEqual(["Tiếng Pháp"]);
    await userEvent.keyboard("{Control>}{ArrowLeft}{/Control}");
    expect(value()).toBe("ko");
    expect(texts(lists()[0])).toEqual(["Tiếng Việt", "Tiếng Anh", "Tiếng Nhật", "Tiếng Pháp"]);
  });

  it("Space grabs in the selected list (slds-is-grabbed), arrows reorder, Space drops — announced in the live region", async () => {
    render(<Harness initial={["ko", "fr", "en"]} />);
    const target = lists()[1];
    within(target).getByRole("option", { name: "Tiếng Anh" }).focus();
    await userEvent.keyboard(" ");
    expect(within(target).getByRole("option", { name: "Tiếng Anh" })).toHaveClass("slds-is-grabbed");
    expect(screen.getByText(/Tiếng Anh: vị trí hiện tại 3\/3/)).toBeInTheDocument();
    await userEvent.keyboard("{ArrowUp}{ArrowUp}");
    expect(value()).toBe("en,ko,fr");
    expect(within(lists()[1]).getByRole("option", { name: "Tiếng Anh" })).toHaveFocus();
    await userEvent.keyboard(" ");
    expect(within(lists()[1]).getByRole("option", { name: "Tiếng Anh" })).not.toHaveClass("slds-is-grabbed");
    expect(screen.getByText(/Tiếng Anh: vị trí cuối 1\/3/)).toBeInTheDocument();
  });

  it("drag mode never outlives the grabbed item: Ctrl+Left out of the list, or a blur to the page, ends it", async () => {
    render(<Harness initial={["ko", "fr", "en"]} />);
    within(lists()[1]).getByRole("option", { name: "Tiếng Anh" }).focus();
    await userEvent.keyboard(" ");
    expect(within(lists()[1]).getByRole("option", { name: "Tiếng Anh" })).toHaveClass("slds-is-grabbed");
    await userEvent.keyboard("{Control>}{ArrowLeft}{/Control}");
    expect(value()).toBe("ko,fr");
    for (const l of lists()) expect(l).toHaveAccessibleDescription(/Nhấn phím cách/);
    // Back in the selected list, arrows only move focus — no silent reorder.
    within(lists()[1]).getByRole("option", { name: "Tiếng Pháp" }).focus();
    await userEvent.keyboard("{ArrowUp}");
    expect(value()).toBe("ko,fr");
    expect(within(lists()[1]).getByRole("option", { name: "Tiếng Hàn" })).toHaveFocus();
    expect(document.querySelector(".slds-is-grabbed")).toBeNull();
    // Grab again, then click the page background (blur with no relatedTarget): drag mode ends.
    await userEvent.keyboard(" ");
    expect(within(lists()[1]).getByRole("option", { name: "Tiếng Hàn" })).toHaveClass("slds-is-grabbed");
    await userEvent.click(document.body);
    await waitFor(() => expect(document.querySelector(".slds-is-grabbed")).toBeNull());
    expect(lists()[1]).toHaveAccessibleDescription(/Nhấn phím cách/);
  });

  it("up/down buttons reorder the selection; no reorder column when reorder=false", async () => {
    const { unmount } = render(<Harness initial={["ko", "fr", "en"]} />);
    await userEvent.click(within(lists()[1]).getByRole("option", { name: "Tiếng Anh" }));
    await userEvent.click(screen.getByRole("button", { name: "Chuyển mục đang chọn lên" }));
    expect(value()).toBe("ko,en,fr");
    unmount();
    render(<Harness reorder={false} />);
    expect(screen.queryByRole("button", { name: "Chuyển mục đang chọn lên" })).toBeNull();
  });

  it("locked item: aria-disabled, lock icon text, never leaves the selected list", async () => {
    render(<Harness options={OPTS.map((o) => (o.value === "ko" ? { ...o, locked: true } : o))} />);
    const ko = within(lists()[1]).getByRole("option", { name: /Tiếng Hàn/ });
    expect(ko).toHaveAttribute("aria-disabled", "true");
    expect(ko.querySelector(".slds-media__figure_reverse .slds-assistive-text")).toHaveTextContent(": không thể gỡ khỏi Đã chọn");
    await userEvent.click(ko);
    await userEvent.click(screen.getByRole("button", { name: "Chuyển mục đang chọn sang Có sẵn" }));
    expect(value()).toBe("ko,fr");
  });

  it("disabled: options boxes slds-is-disabled, listboxes aria-disabled, nothing tabbable, buttons disabled", () => {
    render(<DuelingPicklist label="Ngôn ngữ" options={OPTS} value={["fr"]} onChange={() => {}} disabled />);
    for (const l of lists()) {
      expect(l).toHaveAttribute("aria-disabled", "true");
      expect(l.parentElement).toHaveClass("slds-is-disabled");
      for (const o of within(l).getAllByRole("option")) expect(o).toHaveAttribute("tabindex", "-1");
    }
    for (const b of screen.getAllByRole("button")) expect(b).toBeDisabled();
  });

  it("view mode: label + static comma list; required mark in the group label", () => {
    const { unmount } = render(<DuelingPicklist label="Ngôn ngữ" options={OPTS} value={["vi", "en"]} onChange={() => {}} readOnly />);
    expect(document.querySelector(".slds-dueling-list .slds-form-element__static")).toHaveTextContent("Tiếng Việt, Tiếng Anh");
    unmount();
    render(<DuelingPicklist label="Ngôn ngữ" options={OPTS} value={[]} onChange={() => {}} required error="Chọn ít nhất một ngôn ngữ" />);
    const group = screen.getByRole("group");
    expect(group.querySelector(".slds-form-element__legend abbr.slds-required")).not.toBeNull();
    expect(group).toHaveClass("slds-has-error");
    expect(group.querySelector(".slds-form-element__help")).toHaveTextContent("Chọn ít nhất một ngôn ngữ");
    // Screen readers hear the error on both lists, after the drag instructions.
    for (const l of lists()) {
      expect(l).toHaveAttribute("aria-invalid", "true");
      expect(l).toHaveAccessibleDescription(/Nhấn phím cách.*Chọn ít nhất một ngôn ngữ/);
    }
  });
});
