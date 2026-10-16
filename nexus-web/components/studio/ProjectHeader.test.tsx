// specs/073 kh1a T144/T151 (FR-013, FR-022): the project strip — name, one-line facts, three zones, "…" menu.
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const matches = vi.fn<(s: string) => boolean>();
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches }) }));

vi.mock("./PublishReview", () => ({
  PublishReview: ({ anchor, onClose }: { anchor: React.ReactElement; onClose: () => void }) => (
    <>
      {anchor}
      <div role="dialog" aria-label="Xem lại và xuất bản">
        <button type="button" onClick={onClose}>Đóng</button>
      </div>
    </>
  ),
}));

import { openPublishReview } from "@/lib/draftEvents";
import { ProjectHeader } from "./ProjectHeader";

const identity = { name: "Phòng khám Nha khoa Sài Gòn Tâm Đức chi nhánh quận Bảy", industry: "Nha khoa", stageLabel: "Đang dựng", originLabel: "Từ gói mẫu" };
beforeEach(() => matches.mockReset().mockReturnValue(true));

describe("ProjectHeader", () => {
  it("the name is the h1, wraps instead of being cut, and carries the full text in title", () => {
    render(<ProjectHeader slug="acme" identity={identity} zone="dung" />);
    const h1 = screen.getByRole("heading", { level: 1 });
    const title = within(h1).getByText(identity.name);
    expect(title).not.toHaveClass("slds-truncate");
    expect(title).toHaveAttribute("title", identity.name);
  });

  it("shows industry, stage and origin on one dotted line; only that line is truncated", () => {
    render(<ProjectHeader slug="acme" identity={identity} zone="dung" />);
    const meta = screen.getByText("Nha khoa · Đang dựng · Từ gói mẫu");
    expect(meta).toHaveClass("slds-truncate");
  });

  it("the Nexus workshop shows its own line and the workshop name", () => {
    render(<ProjectHeader slug="northwind" identity={{ name: "Xưởng Nexus" }} zone="dung" home />);
    expect(screen.getByText("Xưởng Nexus · khách không thấy")).toBeInTheDocument();
  });

  it("three zones in a labelled nav, the current one has aria-current", () => {
    render(<ProjectHeader slug="acme" identity={identity} zone="van-hanh" />);
    const nav = screen.getByRole("navigation", { name: "Khu của dự án" });
    const links = within(nav).getAllByRole("link");
    expect(links.map((a) => a.textContent)).toEqual(["Dựng", "Vận hành", "Bảo trì"]);
    expect(within(nav).getByRole("link", { name: "Vận hành" })).toHaveAttribute("aria-current", "page");
    expect(within(nav).getByRole("link", { name: "Dựng" })).not.toHaveAttribute("aria-current");
    expect(within(nav).getByRole("link", { name: "Dựng" })).toHaveAttribute("href", "/studio/du-an/acme/dung/du-lieu");
    expect(within(nav).getByRole("link", { name: "Vận hành" })).toHaveAttribute("href", "/studio/du-an/acme/van-hanh/nguoi-dung");
    expect(within(nav).getByRole("link", { name: "Bảo trì" })).toHaveAttribute("href", "/studio/du-an/acme/bao-tri/kham");
  });

  it("lays out as one row from 1280px, two rows 768-1279, stacked below (FR-013)", () => {
    const { getByTestId } = render(<ProjectHeader slug="acme" identity={identity} zone="dung" openApp={() => {}} />);
    const cls = getByTestId("project-header").className;
    expect(cls).toContain("xl:grid-cols-[minmax(0,34rem)_auto_minmax(0,1fr)]");
    expect(cls).toContain("md:grid-cols-[minmax(0,1fr)_auto]");
    expect(cls).toMatch(/^slds-theme_default slds-border_bottom grid grid-cols-1/);
    const nav = screen.getByRole("navigation", { name: "Khu của dự án" }).parentElement!;
    expect(nav.className).toContain("xl:row-start-1");
    expect(nav.className).toContain("md:row-start-2");
  });

  it("is a joined page header on the default theme", () => {
    const { container } = render(<ProjectHeader slug="acme" identity={identity} zone="dung" />);
    expect(container.querySelector(".slds-page-header.slds-page-header_joined")).not.toBeNull();
    expect(container.firstElementChild).toHaveClass("slds-theme_default");
  });

  it("a status line is appended when the caller has a real one", () => {
    render(<ProjectHeader slug="acme" identity={identity} zone="dung" status="Có 2 lỗi chạy luật" />);
    expect(screen.getByText(/Có 2 lỗi chạy luật/)).toBeInTheDocument();
  });

  it("the … menu opens the app only when the token belongs to the project", () => {
    const openApp = vi.fn();
    const { unmount } = render(<ProjectHeader slug="acme" identity={identity} zone="dung" openApp={openApp} />);
    fireEvent.pointerDown(screen.getByRole("button", { name: "Thêm thao tác" }), { button: 0, ctrlKey: false });
    fireEvent.click(screen.getByRole("button", { name: "Thêm thao tác" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Mở app của khách" }));
    expect(openApp).toHaveBeenCalled();
    unmount();

    matches.mockReturnValue(false);
    render(<ProjectHeader slug="acme" identity={identity} zone="dung" openApp={openApp} />);
    expect(screen.queryByRole("button", { name: "Thêm thao tác" })).toBeNull();
    expect(matches).toHaveBeenCalledWith("acme");
  });

  it("the workshop's menu item says app of the workshop", () => {
    render(<ProjectHeader slug="northwind" identity={{ name: "Xưởng Nexus" }} zone="dung" home openApp={() => {}} />);
    fireEvent.pointerDown(screen.getByRole("button", { name: "Thêm thao tác" }), { button: 0, ctrlKey: false });
    fireEvent.click(screen.getByRole("button", { name: "Thêm thao tác" }));
    expect(screen.getByRole("menuitem", { name: "Mở app của xưởng Nexus" })).toBeInTheDocument();
  });

  // specs/073 T551 (nhap1b): the status line weighs the delivery state; a project not yet delivered has no draft label and no Publish button.
  describe("delivery status line", () => {
    const head = (publish: React.ComponentProps<typeof ProjectHeader>["publish"]) =>
      render(<ProjectHeader slug="acme" identity={identity} zone="dung" publish={publish} />);

    // specs/063 S7-9: nothing pending but a published version's deletes did not finish: the review still opens, with Thử lại in it.
    it("an unfinished delete with nothing pending says so and offers the review", () => {
      head({ delivered: true, pendingCount: 0, versionNo: 4, schemaRetry: true });
      expect(screen.getByText("Xoá chưa xong", { selector: "span" })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Xoá chưa xong · Thử lại" })).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Xuất bản" })).toBeNull();
    });

    it("pending drafts still win over an unfinished delete (the button stays 'Xuất bản')", () => {
      head({ delivered: true, pendingCount: 2, versionNo: 4, schemaRetry: true });
      expect(screen.getByText("2 thay đổi chưa xuất bản")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Xuất bản" })).toBeInTheDocument();
    });

    it("pending drafts outweigh the live version and offer Xuất bản", () => {
      head({ delivered: true, pendingCount: 3, versionNo: 4 });
      expect(screen.getByText("3 thay đổi chưa xuất bản")).toBeInTheDocument();
      expect(screen.queryByText(/Khách đang dùng/)).toBeNull();
      expect(screen.getByRole("button", { name: "Xuất bản" })).toBeInTheDocument();
    });

    it("delivered and clean: the live version, no Publish button", () => {
      head({ delivered: true, pendingCount: 0, versionNo: 4 });
      expect(screen.getByText("Khách đang dùng phiên bản 4")).toBeInTheDocument();
      expect(screen.queryByRole("button", { name: "Xuất bản" })).toBeNull();
    });

    it("not delivered: says so, with no draft label and no Publish button", () => {
      head({ delivered: false, pendingCount: 0, versionNo: null });
      expect(screen.getByText("Chưa giao · khách chưa thấy gì")).toBeInTheDocument();
      expect(screen.queryByText(/chưa xuất bản/)).toBeNull();
      expect(screen.queryByRole("button", { name: "Xuất bản" })).toBeNull();
    });

    it("counts only when the token belongs to this project", () => {
      matches.mockReturnValue(false);
      head({ delivered: true, pendingCount: 3, versionNo: 4 });
      expect(screen.queryByText(/thay đổi chưa xuất bản/)).toBeNull();
      expect(screen.queryByRole("button", { name: "Xuất bản" })).toBeNull();
    });

    it("the assistant's 'Mở bản nháp' (openPublishReview) opens the same review; with nothing pending there is no opener", () => {
      head({ delivered: true, pendingCount: 0, versionNo: 4 });
      expect(openPublishReview()).toBe(false);
      cleanup();
      head({ delivered: true, pendingCount: 2, versionNo: 4 });
      expect(screen.queryByRole("dialog")).toBeNull();
      act(() => { expect(openPublishReview()).toBe(true); });
      expect(screen.getByRole("dialog", { name: "Xem lại và xuất bản" })).toBeInTheDocument();
    });

    it("the button opens the review; closing it leaves the button in place", () => {
      head({ delivered: true, pendingCount: 1, versionNo: 4 });
      fireEvent.click(screen.getByRole("button", { name: "Xuất bản" }));
      expect(screen.getByRole("dialog", { name: "Xem lại và xuất bản" })).toBeInTheDocument();
      fireEvent.click(screen.getByRole("button", { name: "Đóng" }));
      expect(screen.queryByRole("dialog")).toBeNull();
      expect(screen.getByRole("button", { name: "Xuất bản" })).toBeInTheDocument();
    });
  });
});
