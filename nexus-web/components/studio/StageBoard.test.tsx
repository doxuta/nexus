// specs/073 so1a T452 (FR-091, architecture 3.2): the four-column stage board — rules, not cards; one calm line per project.
import { fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { projectRow, SAMPLE_ROWS } from "@/app/dev/kit/sections/projectRows";
import { StageBoard } from "./StageBoard";

const handlers = () => ({
  onOpen: vi.fn(),
  onOpenStep: vi.fn(),
  onTrial: vi.fn(),
  onIndustry: vi.fn(),
  onHealthCheck: vi.fn(),
  onSettings: vi.fn(),
});
const column = (name: string) => screen.getByRole("region", { name: new RegExp(`^${name}`) });

describe("StageBoard", () => {
  it("four columns in stage order, each headed by the stage name and its count of projects", () => {
    render(<StageBoard rows={SAMPLE_ROWS} query="" {...handlers()} />);
    const heads = screen.getAllByRole("heading", { level: 2 });
    expect(heads.map((h) => h.textContent)).toEqual(["Đang dựng 2", "Chạy thử 1", "Đang vận hành 1", "Cần bảo trì 1"]);
    expect(within(column("Đang dựng")).getAllByRole("button", { name: /^(Phòng khám Bạn Nhỏ|Gạch Việt)$/ })).toHaveLength(2);
  });

  it("column titles are 16px headings and project names are the larger 18px line (hierarchy by size)", () => {
    render(<StageBoard rows={SAMPLE_ROWS} query="" {...handlers()} />);
    const head = screen.getAllByRole("heading", { level: 2 })[0];
    expect(head).toHaveClass("slds-text-heading_small");
    const name = screen.getByRole("button", { name: "Gạch Việt" });
    expect(name.style.fontSize).toBe("var(--slds-g-font-scale-3)");
    expect(name.style.fontWeight).toBe("var(--slds-g-font-weight-7)");
  });

  it("no grey column and no card per project: one card, rules between columns", () => {
    const { container } = render(<StageBoard rows={SAMPLE_ROWS} query="" {...handlers()} />);
    expect(container.querySelectorAll(".slds-card")).toHaveLength(1);
    expect(container.querySelector(".slds-theme_shade,.slds-theme_default")).toBeNull();
  });

  it("the card shows how many customer owners can sign in; a field the server did not send shows nothing (specs/065 T093)", () => {
    const { rerender } = render(<StageBoard rows={[projectRow({ stage: "live", usableOwnerCount: 2 })]} query="" {...handlers()} />);
    expect(screen.getByText("2 chủ khách đăng nhập được")).toBeInTheDocument();
    rerender(<StageBoard rows={[projectRow({ stage: "live", usableOwnerCount: 0 })]} query="" {...handlers()} />);
    expect(screen.getByText("Chưa có chủ khách đăng nhập được")).toBeInTheDocument();
    rerender(<StageBoard rows={[projectRow({ stage: "live", usableOwnerCount: undefined })]} query="" {...handlers()} />);
    expect(screen.queryByText(/chủ khách/)).toBeNull();
  });

  it("an empty column says so", () => {
    render(<StageBoard rows={[SAMPLE_ROWS[0]]} query="" {...handlers()} />);
    expect(within(column("Chạy thử")).getByText("Chưa có dự án nào.")).toBeInTheDocument();
  });

  it("a search that matches nothing says what was searched for; matches name, industry and code", () => {
    const { rerender } = render(<StageBoard rows={SAMPLE_ROWS} query="zzz" {...handlers()} />);
    expect(screen.getAllByText("Không có dự án khớp “zzz”.")).toHaveLength(4);
    rerender(<StageBoard rows={SAMPLE_ROWS} query="nha khoa" {...handlers()} />);
    expect(screen.getByRole("button", { name: "Nha khoa Tâm Đức" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Gạch Việt" })).toBeNull();
    rerender(<StageBoard rows={SAMPLE_ROWS} query="MINH-ANH" {...handlers()} />);
    expect(screen.getByRole("button", { name: "Phòng khám Minh Anh" })).toBeInTheDocument();
    // the count in the head follows the filter
    expect(screen.getAllByRole("heading", { level: 2 })[2].textContent).toBe("Đang vận hành 1");
    expect(screen.getAllByRole("heading", { level: 2 })[0].textContent).toBe("Đang dựng 0");
  });

  it("a building project shows the meter and 'Kế tiếp' with the step as a link to that step; no problem text on the row", () => {
    const h = handlers();
    render(<StageBoard rows={[SAMPLE_ROWS[0]]} query="" {...h} />);
    const row = column("Đang dựng");
    expect(within(row).getByRole("img", { name: "1 trên 8 bước dựng đã đủ" })).toBeInTheDocument();
    expect(within(row).getByText(/^Kế tiếp:/)).toHaveTextContent("Kế tiếp: Người dùng & quyền · 0 vai · 0 nhân viên");
    fireEvent.click(within(row).getByRole("button", { name: "Người dùng & quyền" }));
    expect(h.onOpenStep).toHaveBeenCalledWith(SAMPLE_ROWS[0], "access");
    expect(h.onOpen).not.toHaveBeenCalled();
  });

  it("a live project shows one neutral line of use instead of the meter", () => {
    render(<StageBoard rows={[SAMPLE_ROWS[3]]} query="" {...handlers()} />);
    const row = column("Đang vận hành");
    expect(within(row).queryByRole("img")).toBeNull();
    expect(within(row).getByText(/^4 người dùng · hoạt động lần cuối /)).toBeInTheDocument();
    const never = projectRow({ stage: "live", lastActiveAt: null, counts: { ...projectRow().counts, staffUsers: 0 } });
    render(<StageBoard rows={[never]} query="" {...handlers()} />);
    expect(screen.getAllByText("0 người dùng · chưa có hoạt động")).toHaveLength(1);
  });

  it("the name opens the project; so does a click on the empty part of the row, but not a click inside the menu", () => {
    const h = handlers();
    render(<StageBoard rows={[SAMPLE_ROWS[1]]} query="" {...h} />);
    fireEvent.click(screen.getByRole("button", { name: "Gạch Việt" }));
    expect(h.onOpen).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByText("Vật liệu xây dựng"));
    expect(h.onOpen).toHaveBeenCalledTimes(2);
    expect(h.onOpen).toHaveBeenLastCalledWith(SAMPLE_ROWS[1]);
    fireEvent.click(screen.getByRole("button", { name: "Thao tác với dự án Gạch Việt" }));
    expect(h.onOpen).toHaveBeenCalledTimes(2);
  });

  it("the '…' menu works from the keyboard: Enter opens, arrows move, Enter runs 'Bắt đầu chạy thử' for a building project", async () => {
    const user = userEvent.setup();
    const h = handlers();
    render(<StageBoard rows={[SAMPLE_ROWS[1]]} query="" {...h} />);
    const trigger = screen.getByRole("button", { name: "Thao tác với dự án Gạch Việt" });
    trigger.focus();
    await user.keyboard("{Enter}");
    const items = await screen.findAllByRole("menuitem");
    expect(items.map((i) => i.textContent)).toEqual(["Bắt đầu chạy thử", "Sửa ngành", "Khám lại", "Cài đặt dự án"]);
    await user.keyboard("{Enter}");
    expect(h.onTrial).toHaveBeenCalledWith(SAMPLE_ROWS[1], true);
  });

  it("a project on trial offers 'Về Đang dựng'; a delivered one offers no trial switch at all", async () => {
    const user = userEvent.setup();
    const h = handlers();
    const { unmount } = render(<StageBoard rows={[SAMPLE_ROWS[2]]} query="" {...h} />);
    await user.click(screen.getByRole("button", { name: "Thao tác với dự án Nha khoa Tâm Đức" }));
    await user.click(await screen.findByRole("menuitem", { name: "Về Đang dựng" }));
    expect(h.onTrial).toHaveBeenCalledWith(SAMPLE_ROWS[2], false);
    unmount();
    render(<StageBoard rows={[SAMPLE_ROWS[3]]} query="" {...handlers()} />);
    await user.click(screen.getByRole("button", { name: "Thao tác với dự án Phòng khám Minh Anh" }));
    const labels = (await screen.findAllByRole("menuitem")).map((i) => i.textContent);
    expect(labels).toEqual(["Sửa ngành", "Khám lại", "Cài đặt dự án"]);
  });

  it("'Khám lại' and 'Cài đặt dự án' hand the row over", async () => {
    const user = userEvent.setup();
    const h = handlers();
    render(<StageBoard rows={[SAMPLE_ROWS[1]]} query="" {...h} />);
    await user.click(screen.getByRole("button", { name: "Thao tác với dự án Gạch Việt" }));
    await user.click(await screen.findByRole("menuitem", { name: "Khám lại" }));
    expect(h.onHealthCheck).toHaveBeenCalledWith(SAMPLE_ROWS[1]);
    await user.click(screen.getByRole("button", { name: "Thao tác với dự án Gạch Việt" }));
    await user.click(await screen.findByRole("menuitem", { name: "Cài đặt dự án" }));
    expect(h.onSettings).toHaveBeenCalledWith(SAMPLE_ROWS[1]);
  });

  it("'Sửa ngành' edits in the row itself: Lưu sends the text (blank = clear), Esc and Huỷ leave it alone", async () => {
    const user = userEvent.setup();
    const h = handlers();
    render(<StageBoard rows={[SAMPLE_ROWS[1]]} query="" {...h} />);
    const open = async () => {
      await user.click(screen.getByRole("button", { name: "Thao tác với dự án Gạch Việt" }));
      await user.click(await screen.findByRole("menuitem", { name: "Sửa ngành" }));
    };
    await open();
    const input = screen.getByRole("textbox", { name: "Ngành của Gạch Việt" });
    expect(input).toHaveValue("Vật liệu xây dựng");
    expect(input).toHaveFocus();
    await user.clear(input);
    await user.type(input, "  Nội thất  ");
    await user.click(screen.getByRole("button", { name: "Lưu" }));
    expect(h.onIndustry).toHaveBeenCalledWith(SAMPLE_ROWS[1], "Nội thất");
    expect(screen.queryByRole("textbox")).toBeNull();

    await open();
    await user.clear(screen.getByRole("textbox"));
    await user.keyboard("{Enter}");
    expect(h.onIndustry).toHaveBeenLastCalledWith(SAMPLE_ROWS[1], null);

    await open();
    await user.type(screen.getByRole("textbox"), "x{Escape}");
    expect(screen.queryByRole("textbox")).toBeNull();
    await open();
    await user.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(h.onIndustry).toHaveBeenCalledTimes(2);
  });

  it("a column head is the first stop of the Tab order and its focus shows the stage description (not hover-only)", async () => {
    const user = userEvent.setup();
    render(<StageBoard rows={SAMPLE_ROWS} query="" {...handlers()} />);
    await user.tab();
    const head = within(screen.getByRole("heading", { level: 2, name: /^Đang dựng/ })).getByRole("button", { name: "Đang dựng" });
    expect(head).toHaveFocus();
    expect(await screen.findByRole("tooltip")).toHaveTextContent("Dự án đang được dựng: bảng, màn hình, quyền, báo cáo.");
  });
});
