// Pill + PillContainer — blueprint `pills` (base, listbox-of-pill-options), SLDS 2.264.1.
// Keyboard rules from the pills docs: one focusable pill per set, Left/Right move and wrap, and on
// removal focus goes to the next pill (previous when the last one was removed, the listbox when empty).
import { describe, it, expect, vi } from "vitest";
import { useRef, useState } from "react";
import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Building2 } from "lucide-react";
import { Pill, PillContainer } from "@/components/slds/Pill";

const cls = (el: Element | null) => el?.className;

describe("Pill", () => {
  it("linked pill: span.slds-pill_link > a.slds-pill__action > span.slds-pill__label, remove button", async () => {
    const onRemove = vi.fn();
    const { container } = render(<Pill label="Công ty Acme" href="/account/1" onRemove={onRemove} />);
    const pill = container.firstElementChild!;
    expect(cls(pill)).toBe("slds-pill slds-pill_link");
    const a = screen.getByRole("link", { name: "Công ty Acme" });
    expect(cls(a)).toBe("slds-pill__action");
    expect(a).toHaveAttribute("href", "/account/1");
    expect(a).toHaveAttribute("title", "Công ty Acme");
    expect(cls(a.firstElementChild)).toBe("slds-pill__label");
    // The assistive text names the pill (the blueprint's "Remove" placeholder alone repeats N times);
    // the hover title stays the short verb.
    const rm = screen.getByRole("button", { name: "Loại bỏ Công ty Acme" });
    expect(rm).toHaveClass("slds-button", "slds-button_icon", "slds-pill__remove");
    expect(rm).toHaveAttribute("title", "Loại bỏ");
    expect(rm.parentElement).toBe(pill);
    await userEvent.click(rm);
    expect(onRemove).toHaveBeenCalledTimes(1);
  });

  it("unlinked pill (listbox option markup): label span with title, remove button inside span.slds-pill__remove", () => {
    const { container } = render(<Pill label="Hà Nội" onRemove={() => {}} />);
    const pill = container.firstElementChild!;
    expect(cls(pill)).toBe("slds-pill");
    const label = pill.querySelector(".slds-pill__label")!;
    expect(label.tagName).toBe("SPAN");
    expect(label).toHaveAttribute("title", "Hà Nội");
    const rm = screen.getByRole("button", { name: "Loại bỏ Hà Nội" });
    expect(rm).toHaveAttribute("title", "Loại bỏ");
    const wrap = rm.parentElement!;
    expect(cls(wrap)).toBe("slds-icon_container slds-pill__remove");
    expect(wrap).toHaveAttribute("title", "Loại bỏ");
  });

  it("no onRemove → no remove button; bare modifier", () => {
    const { container } = render(<Pill label="Chỉ đọc" bare />);
    expect(screen.queryByRole("button")).toBeNull();
    expect(cls(container.firstElementChild)).toBe("slds-pill slds-pill_bare");
  });

  it("icon: wrapped in slds-pill__icon_container on linked pills, merged into it on unlinked pills", () => {
    const icon = { glyph: Building2, name: "account", label: "Khách hàng" };
    const { container, rerender } = render(<Pill label="Acme" href="#" icon={icon} />);
    const holder = container.querySelector(".slds-pill__icon_container")!;
    expect(holder.tagName).toBe("SPAN");
    expect(holder.firstElementChild).toHaveClass("slds-icon_container", "slds-icon-standard-account");
    rerender(<Pill label="Acme" icon={icon} />);
    const merged = container.querySelector(".slds-pill__icon_container")!;
    expect(merged).toHaveClass("slds-icon_container", "slds-icon-standard-account");
    expect(merged).toHaveAttribute("title", "Khách hàng");
  });

  it("avatar: circle avatar inside the icon container (linked) / x-small avatar as the container (unlinked)", () => {
    const { container, rerender } = render(<Pill label="Tài" href="#" avatar={{ name: "Đỗ Xuân Tài" }} />);
    expect(container.querySelector(".slds-pill__icon_container > .slds-avatar.slds-avatar_circle")).not.toBeNull();
    rerender(<Pill label="Tài" avatar={{ name: "Đỗ Xuân Tài" }} />);
    const av = container.querySelector(".slds-pill__icon_container")!;
    expect(av).toHaveClass("slds-avatar", "slds-avatar_x-small");
  });

  it("error: slds-has-error and an error glyph replaces the icon", () => {
    const { container } = render(<Pill label="Sai định dạng" href="#" error onRemove={() => {}} />);
    expect(container.firstElementChild).toHaveClass("slds-pill", "slds-pill_link", "slds-has-error");
    const box = container.querySelector(".slds-pill__icon_container .slds-icon_container")!;
    expect(box.querySelector("svg")).toHaveClass("slds-icon", "slds-icon-text-error");
    expect(box.querySelector(".slds-assistive-text")).toHaveTextContent("Lỗi");
    // The error is announced on the focusable parts, not only inside the (unfocusable) icon.
    expect(screen.getByRole("link", { name: "Sai định dạng" })).toHaveAccessibleDescription("Lỗi");
    expect(screen.getByRole("button", { name: "Loại bỏ Sai định dạng" })).toHaveAccessibleDescription("Lỗi");
  });

  it("Delete / Backspace on a focused pill removes it", async () => {
    const onRemove = vi.fn();
    render(<Pill label="A" onRemove={onRemove} />);
    screen.getByRole("button", { name: "Loại bỏ A" }).focus();
    await userEvent.keyboard("{Delete}");
    await userEvent.keyboard("{Backspace}");
    expect(onRemove).toHaveBeenCalledTimes(2);
  });
});

function Harness({ initial, withInput }: { initial: string[]; withInput?: boolean }) {
  const [items, setItems] = useState(initial);
  const input = useRef<HTMLInputElement>(null);
  return (
    <>
      {withInput && <input ref={input} aria-label="Tìm" />}
      <PillContainer variant="listbox" label="Đã chọn" emptyFocusRef={withInput ? input : undefined}>
        {items.map((x) => (
          <Pill key={x} label={x} onRemove={() => setItems((s) => s.filter((y) => y !== x))} />
        ))}
      </PillContainer>
    </>
  );
}

const removeButtons = () => screen.queryAllByRole("button", { name: /^Loại bỏ / });
// Asserts through the accessible name: a screen reader must hear which pill it landed on.
const expectFocusOn = (x: string) => expect(document.activeElement).toHaveAccessibleName(`Loại bỏ ${x}`);

describe("PillContainer", () => {
  it("plain container: div.slds-pill_container holding the pills directly", () => {
    const { container } = render(
      <PillContainer>
        <Pill label="A" href="#" />
        <Pill label="B" href="#" />
      </PillContainer>,
    );
    const box = container.firstElementChild!;
    expect(cls(box)).toBe("slds-pill_container");
    expect(Array.from(box.children).map((c) => c.className)).toEqual(["slds-pill slds-pill_link", "slds-pill slds-pill_link"]);
  });

  it("listbox: div.slds-pill_container > ul.slds-listbox.slds-listbox_horizontal[aria-label] > li.slds-listbox-item", () => {
    render(<Harness initial={["A", "B"]} />);
    const ul = screen.getByRole("list", { name: "Đã chọn" });
    expect(cls(ul)).toBe("slds-listbox slds-listbox_horizontal");
    expect(cls(ul.parentElement)).toBe("slds-pill_container");
    expect(Array.from(ul.children).map((c) => c.className)).toEqual(["slds-listbox-item", "slds-listbox-item"]);
  });

  it("default aria-label comes from the dictionary", () => {
    render(
      <PillContainer variant="listbox">
        <Pill label="A" />
      </PillContainer>,
    );
    expect(screen.getByRole("list", { name: "Các mục đã chọn:" })).toBeInTheDocument();
  });

  it("only the first pill is in the tab order initially", () => {
    render(<Harness initial={["A", "B", "C"]} />);
    expect(removeButtons().map((b) => b.tabIndex)).toEqual([0, -1, -1]);
  });

  it("Right / Left move focus and wrap around; the tab stop follows focus", async () => {
    render(<Harness initial={["A", "B", "C"]} />);
    removeButtons()[0].focus();
    await userEvent.keyboard("{ArrowRight}");
    expectFocusOn("B");
    expect(removeButtons().map((b) => b.tabIndex)).toEqual([-1, 0, -1]);
    await userEvent.keyboard("{ArrowRight}{ArrowRight}");
    expectFocusOn("A");
    await userEvent.keyboard("{ArrowLeft}");
    expectFocusOn("C");
    await userEvent.keyboard("{ArrowLeft}");
    expectFocusOn("B");
  });

  it("focusing a pill some other way (mouse, script) makes it the tab stop", () => {
    render(<Harness initial={["A", "B", "C"]} />);
    act(() => removeButtons()[2].focus());
    expect(removeButtons().map((b) => b.tabIndex)).toEqual([-1, -1, 0]);
  });

  it("removing a middle pill focuses the next one; removing the last focuses the previous", async () => {
    render(<Harness initial={["A", "B", "C"]} />);
    removeButtons()[1].focus();
    await userEvent.keyboard("{Delete}");
    expect(removeButtons()).toHaveLength(2);
    expectFocusOn("C");
    await userEvent.keyboard("{Backspace}");
    expect(removeButtons()).toHaveLength(1);
    expectFocusOn("A");
    expect(removeButtons()[0].tabIndex).toBe(0);
  });

  it("clicking a remove button moves focus the same way", async () => {
    render(<Harness initial={["A", "B"]} />);
    await userEvent.click(removeButtons()[0]);
    expectFocusOn("B");
  });

  it("removing the only pill focuses the associated input, else the listbox", async () => {
    const { unmount } = render(<Harness initial={["A"]} withInput />);
    removeButtons()[0].focus();
    await userEvent.keyboard("{Delete}");
    expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Tìm" }));
    unmount();
    render(<Harness initial={["A"]} />);
    removeButtons()[0].focus();
    await userEvent.keyboard("{Delete}");
    expect(document.activeElement).toBe(screen.getByRole("list", { name: "Đã chọn" }));
  });

  // Focus only moves when the removal took it away; a slow or cancelled removal must not pull it back.
  function List({ items, onRemove }: { items: string[]; onRemove: () => void }) {
    return (
      <>
        <input aria-label="Khác" />
        <PillContainer variant="listbox" label="Đã chọn">
          {items.map((x) => (
            <Pill key={x} label={x} onRemove={onRemove} />
          ))}
        </PillContainer>
      </>
    );
  }

  it("async removal that lands after the user moved to another field leaves focus there", async () => {
    const { rerender } = render(<List items={["A", "B", "C"]} onRemove={() => {}} />);
    screen.getByRole("button", { name: "Loại bỏ B" }).focus();
    await userEvent.keyboard("{Delete}");
    const other = screen.getByRole("textbox", { name: "Khác" });
    act(() => other.focus());
    rerender(<List items={["A", "C"]} onRemove={() => {}} />);
    expect(document.activeElement).toBe(other);
  });

  it("async removal that lands after the user arrowed to another pill leaves focus on that pill", async () => {
    const { rerender } = render(<List items={["A", "B", "C"]} onRemove={() => {}} />);
    screen.getByRole("button", { name: "Loại bỏ B" }).focus();
    await userEvent.keyboard("{Delete}{ArrowLeft}");
    expectFocusOn("A");
    rerender(<List items={["A", "C"]} onRemove={() => {}} />);
    expectFocusOn("A");
  });

  it("a cancelled removal does not steal focus when the list later shrinks for another reason", async () => {
    const { rerender } = render(<List items={["A", "B", "C"]} onRemove={() => {}} />);
    screen.getByRole("button", { name: "Loại bỏ C" }).focus();
    await userEvent.keyboard("{Delete}");
    const other = screen.getByRole("textbox", { name: "Khác" });
    act(() => other.focus());
    rerender(<List items={["B", "C"]} onRemove={() => {}} />);
    expect(document.activeElement).toBe(other);
    rerender(<List items={[]} onRemove={() => {}} />);
    expect(document.activeElement).toBe(other);
  });
});
