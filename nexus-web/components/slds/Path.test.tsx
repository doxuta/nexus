// Path — blueprint path/base (default, later-stage, different-stage-selected, with-coaching,
// different-coaching, lost, won), SLDS 2.264.1. Roles and keys from the path docs: listbox/option
// without coaching, tablist/tab with coaching; Left/Right move focus and selection, clamped at the ends.
import { describe, it, expect, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Path, type PathStage, type PathStep } from "./Path";

const stages: PathStage[] = [
  { value: "new", label: "Mới" },
  { value: "qualify", label: "Đánh giá" },
  { value: "proposal", label: "Đề xuất" },
  { value: "negotiate", label: "Đàm phán" },
  { value: "closed", label: "Đã đóng" },
];

const items = () => screen.getAllByRole("option").map((o) => o.parentElement!);

describe("Path", () => {
  it("renders the blueprint track: complete / current / incomplete stages and the action area", () => {
    const { container } = render(<Path stages={stages} current="proposal" onAction={() => {}} />);
    const root = container.firstElementChild!;
    expect(root.className).toBe("slds-path");
    expect(root.firstElementChild).toHaveClass("slds-grid", "slds-path__track");

    const nav = screen.getByRole("listbox");
    expect(nav.tagName).toBe("UL");
    expect(nav).toHaveClass("slds-path__nav");
    expect(nav).toHaveAttribute("aria-orientation", "horizontal");
    expect(nav.closest(".slds-path__scroller_inner")!.parentElement).toHaveClass("slds-path__scroller");
    const stageName = document.getElementById(nav.getAttribute("aria-labelledby")!)!;
    expect(stageName).toHaveClass("slds-path__stage-name");
    expect(stageName).toHaveTextContent("Giai đoạn: Đề xuất");

    const li = items();
    expect(li.map((l) => l.className)).toEqual([
      "slds-path__item slds-is-complete",
      "slds-path__item slds-is-complete",
      "slds-path__item slds-is-current slds-is-active",
      "slds-path__item slds-is-incomplete",
      "slds-path__item slds-is-incomplete",
    ]);
    li.forEach((l) => expect(l).toHaveAttribute("role", "presentation"));

    const options = screen.getAllByRole("option");
    expect(options[2]).toHaveClass("slds-path__link");
    expect(options[2]).toHaveAttribute("aria-selected", "true");
    expect(options[2]).toHaveAttribute("tabindex", "0");
    expect(options[0]).toHaveAttribute("aria-selected", "false");
    expect(options[0]).toHaveAttribute("tabindex", "-1");
    expect(options[0].querySelector(".slds-path__stage .slds-assistive-text")).toHaveTextContent("Giai đoạn đã hoàn tất");
    expect(options[2].querySelector(".slds-path__stage .slds-assistive-text")).toHaveTextContent("Giai đoạn hiện tại:");
    expect(options[3].querySelector(".slds-assistive-text")).toBeNull();
    expect(options[2].querySelector(".slds-path__stage svg")).toHaveClass("slds-icon", "slds-icon_x-small");
    expect(options[2].querySelector(".slds-path__title")).toHaveTextContent("Đề xuất");

    const action = screen.getByRole("button", { name: "Đánh dấu hoàn tất giai đoạn" });
    expect(action).toHaveClass("slds-button", "slds-button_brand", "slds-path__mark-complete");
    expect(action.parentElement).toHaveClass("slds-grid", "slds-path__action");
    expect(action.querySelector("svg")).toHaveClass("slds-button__icon", "slds-button__icon_left");
    // No coaching: no trigger, no content area.
    expect(root.querySelector(".slds-path__trigger")).toBeNull();
    expect(root.querySelector(".slds-path__content")).toBeNull();
  });

  it("Left/Right move focus and selection; a non-current stage offers 'Đánh dấu là giai đoạn hiện tại'", async () => {
    const onAction = vi.fn();
    render(<Path stages={stages} current="qualify" onAction={onAction} />);
    const options = screen.getAllByRole("option");
    options[1].focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(options[2]).toHaveFocus();
    expect(options[2]).toHaveAttribute("aria-selected", "true");
    expect(options[2]).toHaveAttribute("tabindex", "0");
    expect(options[1]).toHaveAttribute("tabindex", "-1");
    expect(items()[2].className).toBe("slds-path__item slds-is-incomplete slds-is-active");
    expect(items()[1].className).toBe("slds-path__item slds-is-current");

    const action = screen.getByRole("button", { name: "Đánh dấu là giai đoạn hiện tại" });
    expect(action.querySelector("svg")).toBeNull();
    await userEvent.click(action);
    expect(onAction).toHaveBeenCalledWith("setCurrent", stages[2]);

    await userEvent.click(screen.getAllByRole("option")[0]);
    expect(screen.getAllByRole("option")[0]).toHaveAttribute("aria-selected", "true");
    options[0].focus();
    await userEvent.keyboard("{ArrowLeft}"); // stays on the first stage
    expect(options[0]).toHaveFocus();
    expect(options[0]).toHaveAttribute("aria-selected", "true");
  });

  it("ArrowLeft moves back a stage; the last stage keeps ArrowRight; arrows do not scroll the page", async () => {
    render(<Path stages={stages} current="proposal" />);
    const options = screen.getAllByRole("option");
    // Listbox options are not disclosure triggers: aria-expanded belongs to coaching tabs only.
    options.forEach((o) => expect(o).not.toHaveAttribute("aria-expanded"));

    options[2].focus();
    await userEvent.keyboard("{ArrowLeft}");
    expect(options[1]).toHaveFocus();
    expect(options[1]).toHaveAttribute("aria-selected", "true");
    expect(options[1]).toHaveAttribute("tabindex", "0");
    expect(options[2]).toHaveAttribute("aria-selected", "false");

    // fireEvent returns false when the handler called preventDefault.
    expect(fireEvent.keyDown(options[1], { key: "ArrowRight" })).toBe(false);
    expect(options[2]).toHaveFocus();

    await userEvent.keyboard("{ArrowRight}{ArrowRight}");
    expect(options[4]).toHaveFocus();
    // A handler that indexes past the end throws; the DOM reports it as a window "error" event
    // and leaves the state as it was, so the focus checks alone would not notice.
    const errors: string[] = [];
    const onError = (e: ErrorEvent) => errors.push(e.message);
    window.addEventListener("error", onError);
    await userEvent.keyboard("{ArrowRight}"); // stays on the last stage
    window.removeEventListener("error", onError);
    expect(errors).toEqual([]);
    expect(options[4]).toHaveFocus();
    expect(options[4]).toHaveAttribute("aria-selected", "true");
    expect(options[4]).toHaveAttribute("tabindex", "0");
    expect(options[3]).toHaveAttribute("aria-selected", "false");
  });

  it("'Đánh dấu hoàn tất giai đoạn' reports the current stage", async () => {
    const onAction = vi.fn();
    render(<Path stages={stages} current="new" onAction={onAction} />);
    await userEvent.click(screen.getByRole("button", { name: "Đánh dấu hoàn tất giai đoạn" }));
    expect(onAction).toHaveBeenCalledWith("complete", stages[0]);
  });

  it("closed lost: earlier stages incomplete, lost stage current; closed won: earlier stages complete", () => {
    const lost = [...stages.slice(0, 4), { value: "lost", label: "Đã đóng - Thất bại" }];
    const { unmount } = render(<Path stages={lost} current="lost" closed="lost" onAction={() => {}} />);
    expect(items().map((l) => l.className)).toEqual([
      "slds-path__item slds-is-incomplete",
      "slds-path__item slds-is-incomplete",
      "slds-path__item slds-is-incomplete",
      "slds-path__item slds-is-incomplete",
      "slds-path__item slds-is-current slds-is-lost slds-is-active",
    ]);
    expect(screen.getByRole("button", { name: "Đổi trạng thái đóng" })).toBeInTheDocument();
    unmount();

    const won = [...stages.slice(0, 4), { value: "won", label: "Đã đóng - Thành công" }];
    render(<Path stages={won} current="won" closed="won" />);
    expect(items().map((l) => l.className)).toEqual([
      "slds-path__item slds-is-complete",
      "slds-path__item slds-is-complete",
      "slds-path__item slds-is-complete",
      "slds-path__item slds-is-complete",
      "slds-path__item slds-is-current slds-is-won slds-is-active",
    ]);
    // Read-only path (no onAction): no action button.
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("with coaching: tablist roles, a toggle that opens the selected stage's key fields and guidance", async () => {
    const onEdit = vi.fn();
    const { container } = render(
      <Path
        stages={stages}
        current="qualify"
        onAction={() => {}}
        onEditKeyFields={onEdit}
        coaching={(s) =>
          s.value === "qualify"
            ? { keyFields: [{ label: "Ngân sách dự kiến", value: "250.000.000 ₫" }], guidance: <p>Hỏi rõ người quyết định.</p> }
            : { keyFields: [{ label: "Đối thủ", value: "Chưa rõ" }], guidance: <p>So sánh với đối thủ chính.</p> }
        }
      />,
    );
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-path", "slds-path_has-coaching");
    expect(root).not.toHaveClass("slds-is-expanded");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(screen.getByRole("tablist")).toHaveClass("slds-path__nav");
    const tabs = screen.getAllByRole("tab");
    tabs.forEach((tab) => expect(tab).toHaveAttribute("aria-expanded", "false"));

    const toggle = screen.getByRole("button", { name: "Hiện chi tiết" });
    expect(toggle).toHaveClass("slds-button", "slds-button_icon", "slds-button_icon-border-filled", "slds-path__trigger");
    expect(toggle).toHaveAttribute("title", "Bật/tắt hướng dẫn giai đoạn");
    expect(toggle.parentElement).toHaveClass("slds-path__scroller-container");
    const content = document.getElementById(toggle.getAttribute("aria-controls")!)!;
    expect(content).toHaveClass("slds-path__content", "slds-is-collapsed");
    const more = screen.getByRole("button", { name: "Xem thêm" });
    expect(more).toHaveClass("slds-button_neutral", "slds-path__trigger-coaching-content");
    expect(more).toHaveAttribute("aria-controls", content.id);

    await userEvent.click(toggle);
    expect(root).toHaveClass("slds-is-expanded");
    expect(toggle).toHaveClass("slds-path__trigger_open");
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(content).not.toHaveClass("slds-is-collapsed");
    tabs.forEach((tab) => expect(tab).toHaveAttribute("aria-expanded", "true"));
    expect(screen.getByRole("button", { name: "Thu gọn" })).toHaveAttribute("aria-expanded", "true");

    const keys = content.querySelector(".slds-path__coach.slds-grid > .slds-path__keys")!;
    expect(within(keys as HTMLElement).getByRole("heading", { name: "Trường chính của giai đoạn" })).toBeInTheDocument();
    const field = within(keys as HTMLElement).getByRole("listitem");
    expect(field).toHaveClass("slds-form__item");
    expect(field.querySelector(".slds-form-element_readonly .slds-form-element__label")).toHaveTextContent("Ngân sách dự kiến");
    expect(field.querySelector(".slds-form-element__static")).toHaveTextContent("250.000.000 ₫");
    const guidance = content.querySelector(".slds-path__guidance")!;
    expect(guidance.querySelector("h2.slds-path__coach-title")).toHaveTextContent("Hướng dẫn để thành công");
    expect(guidance.querySelector(".slds-text-longform.slds-path__guidance-content")).toHaveTextContent("Hỏi rõ người quyết định.");

    // Coaching follows the selected stage, not the current one.
    tabs[1].focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(content).toHaveTextContent("So sánh với đối thủ chính.");

    await userEvent.click(within(keys as HTMLElement).getByRole("button", { name: "Sửa" }));
    expect(onEdit).toHaveBeenCalledWith(stages[2]);

    await userEvent.click(screen.getByRole("button", { name: "Thu gọn" }));
    expect(content).toHaveClass("slds-is-collapsed");
  });
});

// specs/073 T050: mode="steps" — the Studio build-steps bar. Every step is a link, Tab reaches all of them.
describe("Path mode=steps", () => {
  const steps: PathStep[] = [
    { key: "data", label: "Dữ liệu", sublabel: "3 bảng", complete: true, href: "/studio/p/1/data" },
    { key: "screens", label: "Màn hình", complete: false, href: "/studio/p/1/screens" },
    { key: "rules", label: "Quy tắc", complete: null, href: "/studio/p/1/rules" },
  ];
  const render3 = (current = "screens") =>
    render(<Path mode="steps" label="Các bước dựng" steps={steps} current={current} />);

  it("is a labelled nav of links with exactly one aria-current=step and the nx-path-steps hook", () => {
    const { container } = render3();
    const nav = screen.getByRole("navigation", { name: "Các bước dựng" });
    expect(container.querySelector(".slds-path.nx-path-steps")).not.toBeNull();
    expect(nav.querySelector("ul.slds-path__nav")).not.toBeNull();
    const links = within(nav).getAllByRole("link");
    expect(links.map((a) => a.getAttribute("href"))).toEqual(steps.map((s) => s.href));
    expect(links.filter((a) => a.getAttribute("aria-current") === "step")).toHaveLength(1);
    expect(screen.getByRole("link", { name: /Màn hình/ })).toHaveAttribute("aria-current", "step");
    expect(nav.querySelector(".slds-path__track")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("complete=true/false carry hidden text, null carries none; no tick when null", () => {
    render3();
    expect(screen.getByRole("link", { name: /Dữ liệu/ })).toHaveTextContent("đã đủ");
    expect(screen.getByRole("link", { name: /Màn hình/ })).toHaveTextContent("chưa đủ");
    const rules = screen.getByRole("link", { name: /Quy tắc/ });
    expect(rules).not.toHaveTextContent("đủ");
    expect(rules.closest("li")!.className).not.toMatch(/is-complete/);
    expect(rules.querySelector("svg")).toBeNull();
    expect(screen.getByRole("link", { name: /Dữ liệu/ }).closest("li")).toHaveClass("slds-is-complete");
    expect(screen.getByRole("link", { name: /Dữ liệu/ })).toHaveTextContent("3 bảng");
  });

  it("every step is reachable with Tab (no roving tabindex); arrows are an extra", async () => {
    render3();
    const links = screen.getAllByRole("link");
    for (const a of links) expect(a).not.toHaveAttribute("tabindex", "-1");
    await userEvent.tab();
    expect(links[0]).toHaveFocus();
    await userEvent.tab();
    expect(links[1]).toHaveFocus();
    await userEvent.keyboard("{ArrowRight}");
    expect(links[2]).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}{ArrowLeft}");
    expect(links[0]).toHaveFocus();
  });

  it("reveals the open step by scrolling its own strip, never the window", () => {
    const wsy = vi.spyOn(window, "scrollTo").mockImplementation(() => {});
    const rect = (left: number, right: number) => ({ left, right, top: 0, bottom: 0, width: right - left, height: 0, x: left, y: 0, toJSON() {} }) as DOMRect;
    const spy = vi.spyOn(Element.prototype, "getBoundingClientRect").mockImplementation(function (this: Element) {
      return this.classList.contains("slds-path__scroller") ? rect(0, 100) : rect(150, 250);
    });
    const { container } = render3("rules");
    expect(container.querySelector<HTMLElement>(".slds-path__scroller")!.scrollLeft).toBe(150);
    expect(wsy).not.toHaveBeenCalled();
    spy.mockRestore();
    wsy.mockRestore();
  });

  it("modifier+arrow is left to the browser (Alt+Left is Back)", async () => {
    render3("data");
    screen.getByRole("link", { name: /Dữ liệu/ }).focus();
    await userEvent.keyboard("{Alt>}{ArrowRight}{/Alt}");
    expect(screen.getByRole("link", { name: /Dữ liệu/ })).toHaveFocus();
  });
});
