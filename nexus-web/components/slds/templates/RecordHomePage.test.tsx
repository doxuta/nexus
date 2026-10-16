// RecordHomePage — record page template (page-headers/record-home + path + tabs + right column).
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Building2 } from "lucide-react";
import { fakeRowLayout } from "@/test/fakeRowLayout";
import { RecordHomePage } from "./RecordHomePage";

const icon = { glyph: Building2, name: "account" };
const tabs = [
  { value: "details", label: "Chi tiết", content: <p>Nội dung chi tiết</p> },
  { value: "history", label: "Lịch sử", content: <p>Nội dung lịch sử</p> },
];
const stages = [
  { value: "lead", label: "Tiềm năng" },
  { value: "active", label: "Đang giao dịch" },
];

describe("RecordHomePage", () => {
  it("puts header, highlights, tabs and the right column in their regions", () => {
    const { container } = render(
      <RecordHomePage
        icon={icon}
        objectLabel="Khách hàng"
        title="Công ty TNHH Minh Phát"
        details={[{ label: "Ngành", value: "Bán lẻ" }]}
        tabs={tabs}
        aside={<div data-testid="aside">Liên hệ</div>}
      />,
    );
    const header = container.querySelector(".slds-page-header")!;
    expect(header).toHaveClass("slds-page-header_record-home");
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Khách hàngCông ty TNHH Minh Phát");
    expect(screen.getByText("Bán lẻ").closest(".slds-page-header__detail-block")).not.toBeNull();

    // Two-thirds / one-third grid that stacks below the SLDS "large" breakpoint (64em = 1024px).
    const main = screen.getByRole("tablist").closest(".slds-col")!;
    expect(main).toHaveClass("slds-size_1-of-1", "slds-large-size_2-of-3");
    expect(main.closest(".slds-tabs_card")).toBeNull();
    expect(main.querySelector(".slds-tabs_card .slds-tabs_default")).not.toBeNull();
    const side = screen.getByTestId("aside").closest(".slds-col")!;
    expect(side).toHaveClass("slds-size_1-of-1", "slds-large-size_1-of-3");
    expect(main.parentElement).toBe(side.parentElement);
    expect(main.parentElement).toHaveClass("slds-grid", "slds-wrap");
  });

  it("hides the path when there are no stages", () => {
    const { container, rerender } = render(<RecordHomePage icon={icon} objectLabel="Khách hàng" title="Minh Phát" tabs={tabs} />);
    expect(container.querySelector(".slds-path")).toBeNull();
    rerender(<RecordHomePage icon={icon} objectLabel="Khách hàng" title="Minh Phát" tabs={tabs} path={{ stages: [], current: "" }} />);
    expect(container.querySelector(".slds-path")).toBeNull();
  });

  it("shows the path between the header and the tabs when stages are given", () => {
    const { container } = render(
      <RecordHomePage icon={icon} objectLabel="Khách hàng" title="Minh Phát" tabs={tabs} path={{ stages, current: "active" }} />,
    );
    const path = container.querySelector(".slds-path")!;
    expect(path).not.toBeNull();
    expect(screen.getByRole("listbox")).toBeInTheDocument();
    const header = container.querySelector(".slds-page-header")!;
    const tablist = screen.getByRole("tablist");
    expect(header.compareDocumentPosition(path) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(path.compareDocumentPosition(tablist) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("tabs switch with the arrow keys", async () => {
    render(<RecordHomePage icon={icon} objectLabel="Khách hàng" title="Minh Phát" tabs={tabs} />);
    const [details, history] = screen.getAllByRole("tab");
    expect(details).toHaveAttribute("aria-selected", "true");
    details.focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(history).toHaveFocus();
    expect(history).toHaveAttribute("aria-selected", "true");
    expect(screen.getByText("Nội dung lịch sử").closest("[role=tabpanel]")).toHaveClass("slds-show");
  });

  // A phone is narrower than six tabs: the ones that do not fit go into the blueprint's "Thêm" tab
  // (tabs/base overflow), so the last one is still one tap away instead of clipped off the card.
  it("tabs that do not fit the card's width go into 'Thêm'", async () => {
    fakeRowLayout(350);
    try {
      const six = ["Chi tiết", "Ghi chú", "Liên quan", "Tệp", "Lịch sử", "Duyệt"].map((label, i) => ({
        value: `t${i}`,
        label,
        content: <p>Nội dung {label}</p>,
      }));
      render(<RecordHomePage icon={icon} objectLabel="Khách hàng" title="Minh Phát" tabs={six} />);
      expect(screen.getAllByRole("tab").map((tab) => tab.textContent)).toEqual(["Chi tiết", "Ghi chú", "Thêm"]);
      await userEvent.click(screen.getByRole("tab", { name: "Thêm" }));
      await userEvent.click(screen.getByRole("menuitem", { name: "Duyệt" }));
      expect(screen.getByRole("tab", { name: "Duyệt" })).toHaveAttribute("aria-selected", "true");
    } finally {
      vi.restoreAllMocks();
      vi.unstubAllGlobals();
    }
  });

  it("header actions: buttons first, the rest in the overflow menu", async () => {
    const onEdit = vi.fn();
    const onPrint = vi.fn();
    render(
      <RecordHomePage
        icon={icon}
        objectLabel="Khách hàng"
        title="Minh Phát"
        tabs={tabs}
        actions={[
          { label: "Sửa", onSelect: onEdit },
          { label: "Nhân bản", onSelect: () => {} },
          { label: "Xoá", onSelect: () => {} },
          { label: "In", onSelect: onPrint },
        ]}
      />,
    );
    const edit = screen.getByRole("button", { name: "Sửa" });
    expect(edit.closest(".slds-page-header__col-actions")).not.toBeNull();
    expect(edit.closest("ul")).toHaveClass("slds-button-group-list");
    await userEvent.click(edit);
    expect(onEdit).toHaveBeenCalledOnce();
    expect(screen.queryByRole("button", { name: "In" })).toBeNull();
    screen.getByRole("button", { name: "Thêm thao tác" }).focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByRole("menuitem", { name: "In" })).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(onPrint).toHaveBeenCalledOnce();
  });
});
