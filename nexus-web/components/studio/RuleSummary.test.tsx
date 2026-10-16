import { readFileSync } from "node:fs";
import type { ComponentProps } from "react";
import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RuleSummary } from "./RuleSummary";

const sentence = { when: "Phiếu được tạo", cond: "luôn luôn", then: ["Gửi dữ liệu sang hệ thống khác", "Gửi email"] };
const renderRow = (p: Partial<ComponentProps<typeof RuleSummary>> = {}) =>
  render(<ul><RuleSummary name="Báo phiếu mới" sentence={sentence} trigger="after_create" {...p} /></ul>);

describe("RuleSummary", () => {
  it("reads Khi / Nếu / Thì with every action on its own line", () => {
    renderRow();
    for (const w of ["Khi", "Nếu", "Thì"]) expect(screen.getByText(w)).toBeTruthy();
    expect(screen.getByText("Phiếu được tạo")).toBeTruthy();
    expect(screen.getByText("Gửi dữ liệu sang hệ thống khác")).toBeTruthy();
    expect(screen.getByText("Gửi email")).toBeTruthy();
    expect(screen.queryByText("Đang tắt")).toBeNull();
  });
  it("off rule says 'Đang tắt' as coloured text", () => {
    renderRow({ active: false });
    expect(screen.getByText("Đang tắt").className).toContain("nx-status_weak");
  });
  it("before-save rule says 'Chạy khi lưu' as StatusText, not a Badge", () => {
    renderRow({ trigger: "before_save" });
    expect(screen.getByText("Chạy khi lưu").className).toContain("nx-status");
    expect(readFileSync("components/studio/RuleSummary.tsx", "utf8")).not.toMatch(/import[^;]*\b(Badge|Pill)\b/);
  });
  it("draft state shows its label; a name link only when href is given", () => {
    renderRow({ draftState: "edited", href: "/x" });
    expect(screen.getByRole("link", { name: "Báo phiếu mới" }).getAttribute("href")).toBe("/x");
    expect(document.querySelector(".nx-status_warn")).toBeTruthy();
  });

  describe("toggle + failed-run mark (lát f2-rule-toggle-error)", () => {
    it("switch is named per rule, reflects state, calls onToggle with the inverse", () => {
      const calls: boolean[] = [];
      render(<ul>
        <RuleSummary name="A" sentence={sentence} active onToggle={(n) => calls.push(n)} />
        <RuleSummary name="B" sentence={sentence} active={false} onToggle={() => {}} />
      </ul>);
      const a = screen.getByRole("switch", { name: "Bật luật A" }) as HTMLInputElement;
      const b = screen.getByRole("switch", { name: "Bật luật B" }) as HTMLInputElement;
      expect(a.checked).toBe(true);
      expect(b.checked).toBe(false);
      fireEvent.click(a);
      expect(calls).toEqual([false]);
      // the faux track holds both words (CSS shows the one matching state)
      expect(document.querySelectorAll(".slds-checkbox_on")[0].textContent).toBe("Đang bật");
      expect(document.querySelectorAll(".slds-checkbox_off")[1].textContent).toBe("Đang tắt");
    });
    it("Tab reaches the switch and Space toggles it", async () => {
      const calls: boolean[] = [];
      const user = userEvent.setup();
      renderRow({ active: false, onToggle: (n) => calls.push(n) });
      await user.tab();
      expect((document.activeElement as HTMLElement).getAttribute("role")).toBe("switch");
      await user.keyboard(" ");
      expect(calls).toEqual([true]);
    });
    it("outcome message sits in an aria-live polite region of the row", () => {
      renderRow({ onToggle: () => {}, message: { tone: "error", text: "Không đổi được" } });
      const live = screen.getByText("Không đổi được").closest("[aria-live]");
      expect(live?.getAttribute("aria-live")).toBe("polite");
    });
    it("live region exists even with no message (so the first result is announced)", () => {
      renderRow({ onToggle: () => {} });
      expect(document.querySelector('[aria-live="polite"]')).toBeTruthy();
    });
    it("failed mark is words 'Lần chạy gần nhất lỗi', a button to the failing run; absent otherwise", () => {
      const opened: number[] = [];
      renderRow({ failed: true, onOpenFailedRun: () => opened.push(1) });
      fireEvent.click(screen.getByRole("button", { name: "Lần chạy gần nhất lỗi" }));
      expect(opened).toEqual([1]);
    });
    it("no mark when not failed, and never says 'chưa chạy'", () => {
      renderRow({ onToggle: () => {} });
      expect(screen.queryByText("Lần chạy gần nhất lỗi")).toBeNull();
      expect(document.body.textContent).not.toMatch(/chưa chạy/i);
    });
    it("without onToggle the old 'Đang tắt' status text stays (no duplicate)", () => {
      renderRow({ active: false });
      expect(screen.getAllByText("Đang tắt")).toHaveLength(1);
    });
    it("busy switch is disabled", () => {
      renderRow({ onToggle: () => {}, toggling: true });
      expect((screen.getByRole("switch") as HTMLInputElement).disabled).toBe(true);
    });
  });
});
