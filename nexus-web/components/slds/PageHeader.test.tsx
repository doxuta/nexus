// PageHeader — blueprint page-headers (base, object-home, record-home, related-list), SLDS 2.264.1.
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { Briefcase } from "lucide-react";
import { PageHeader } from "./PageHeader";

const icon = { glyph: Briefcase, name: "opportunity" };

describe("PageHeader", () => {
  it("base: icon, title and name-meta inside the media body", () => {
    const { container } = render(
      <PageHeader icon={{ ...icon, label: "Cơ hội" }} title="Minh Phát - 80.000 thiết bị" meta="Nguyễn Văn An • Khách hàng VIP" />,
    );
    const root = container.firstElementChild!;
    expect(root.className).toBe("slds-page-header");
    const row = root.firstElementChild!;
    expect(row).toHaveClass("slds-page-header__row");
    expect(row.firstElementChild).toHaveClass("slds-page-header__col-title");

    const tile = root.querySelector(".slds-media__figure > .slds-icon_container")!;
    expect(tile).toHaveClass("slds-icon-standard-opportunity");
    expect(tile).toHaveAttribute("title", "Cơ hội");
    expect(tile.querySelector("svg")).toHaveClass("slds-icon", "slds-page-header__icon");
    expect(within(tile as HTMLElement).getByText("Cơ hội")).toHaveClass("slds-assistive-text");

    const h1 = screen.getByRole("heading", { level: 1 });
    expect(h1.parentElement).toHaveClass("slds-page-header__name-title");
    expect(h1.parentElement!.parentElement).toHaveClass("slds-page-header__name");
    const title = within(h1).getByText("Minh Phát - 80.000 thiết bị");
    expect(title).toHaveClass("slds-page-header__title", "slds-truncate");
    expect(title).toHaveAttribute("title", "Minh Phát - 80.000 thiết bị");

    const meta = screen.getByText("Nguyễn Văn An • Khách hàng VIP");
    expect(meta.tagName).toBe("P");
    expect(meta).toHaveClass("slds-page-header__name-meta");
    expect(meta.parentElement).toHaveClass("slds-media__body");
    expect(root.querySelectorAll(".slds-page-header__row")).toHaveLength(1);
  });

  it("object-home: label over title, list-view switcher slot, actions and a meta/controls row", () => {
    const { container } = render(
      <PageHeader
        variant="object-home"
        icon={icon}
        label="Cơ hội"
        title="Xem gần đây"
        titleMenu={<button>Đổi danh sách</button>}
        actions={[<button key="new">Mới</button>, <button key="more">Thêm thao tác</button>]}
        meta="10 mục • Cập nhật 13 phút trước"
        controls={[<button key="refresh">Làm mới</button>]}
      />,
    );
    const root = container.firstElementChild!;
    // Blueprint object-home carries no modifier class.
    expect(root.className).toBe("slds-page-header");
    const h1 = screen.getByRole("heading", { level: 1 });
    const spans = Array.from(h1.children);
    expect(spans.map((s) => s.textContent)).toEqual(["Cơ hội", "Xem gần đây"]);
    expect(spans[0].className).toBe("");
    expect(spans[1]).toHaveClass("slds-page-header__title");

    const switcher = screen.getByRole("button", { name: "Đổi danh sách" }).parentElement!;
    expect(switcher).toHaveClass("slds-page-header__name-switcher");
    expect(switcher.previousElementSibling).toHaveClass("slds-page-header__name-title");

    const rows = Array.from(root.children).filter((c) => c.classList.contains("slds-page-header__row"));
    expect(rows).toHaveLength(2);
    const actions = rows[0].querySelector(".slds-page-header__col-actions > .slds-page-header__controls")!;
    expect(Array.from(actions.children).map((c) => c.className)).toEqual([
      "slds-page-header__control",
      "slds-page-header__control",
    ]);
    expect(within(actions as HTMLElement).getByRole("button", { name: "Mới" })).toBeInTheDocument();

    const meta = within(rows[1] as HTMLElement).getByText("10 mục • Cập nhật 13 phút trước");
    expect(meta).toHaveClass("slds-page-header__meta-text");
    expect(meta.parentElement).toHaveClass("slds-page-header__col-meta");
    const controls = rows[1].querySelector(".slds-page-header__col-controls > .slds-page-header__controls")!;
    expect(controls.firstElementChild).toHaveClass("slds-page-header__control");
    expect(within(controls as HTMLElement).getByRole("button", { name: "Làm mới" })).toBeInTheDocument();
    // name-meta is the base variant's slot only.
    expect(root.querySelector(".slds-page-header__name-meta")).toBeNull();
  });

  it("record-home: modifier class and the detail row of highlight fields", () => {
    const { container } = render(
      <PageHeader
        variant="record-home"
        icon={icon}
        label="Cơ hội"
        title="Minh Phát - 1.200 thiết bị"
        actions={[<button key="edit">Sửa</button>]}
        details={[
          { label: "Giai đoạn", value: "Đàm phán" },
          { label: "Website", value: <a href="https://minhphat.vn">minhphat.vn</a> },
        ]}
      />,
    );
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-page-header", "slds-page-header_record-home");
    const detailRow = root.querySelector(".slds-page-header__row.slds-page-header__row_gutters > .slds-page-header__col-details > ul")!;
    expect(detailRow).toHaveClass("slds-page-header__detail-row");
    const blocks = Array.from(detailRow.children);
    expect(blocks).toHaveLength(2);
    expect(blocks[0]).toHaveClass("slds-page-header__detail-block");
    const [label, value] = Array.from(blocks[0].children);
    expect(label).toHaveClass("slds-text-title", "slds-truncate");
    expect(label).toHaveAttribute("title", "Giai đoạn");
    expect(value).toHaveClass("slds-truncate");
    expect(value).toHaveAttribute("title", "Đàm phán");
    expect(within(blocks[1] as HTMLElement).getByRole("link", { name: "minhphat.vn" })).toBeInTheDocument();
  });

  // SLDS ships no small-screen rules for page headers. Below its medium breakpoint (48em) the
  // title and the action group take a line each and the highlights go two per line — layout
  // utilities on the component itself, so no page depends on a stylesheet patch for it.
  it("below 48em: title and actions stack, highlights wrap two per line", () => {
    const { container } = render(
      <PageHeader
        variant="record-home"
        icon={icon}
        label="Cơ hội"
        title="Minh Phát - 1.200 thiết bị"
        actions={[<button key="edit">Sửa</button>]}
        details={[{ label: "Giai đoạn", value: "Đàm phán" }]}
      />,
    );
    const root = container.firstElementChild!;
    expect(root.querySelector(".slds-page-header__row")).toHaveClass("max-md:flex-wrap");
    expect(root.querySelector(".slds-page-header__col-title")).toHaveClass("max-md:basis-full");
    expect(root.querySelector(".slds-page-header__col-actions")).toHaveClass("max-md:basis-full");
    expect(root.querySelector(".slds-page-header__detail-row")).toHaveClass("max-md:flex-wrap");
    expect(root.querySelector(".slds-page-header__detail-block")).toHaveClass("max-md:basis-1/2", "max-md:max-w-1/2");
  });

  it("related-list: breadcrumbs above the title, modifier class", () => {
    const { container } = render(
      <PageHeader
        variant="related-list"
        title="Liên hệ"
        breadcrumbs={[
          { label: "Khách hàng", href: "/crm_accounts" },
          { label: "Minh Phát", href: "/crm_accounts/1" },
        ]}
        meta="10 mục • sắp theo tên"
      />,
    );
    const root = container.firstElementChild!;
    expect(root).toHaveClass("slds-page-header", "slds-page-header_related-list");
    const colTitle = root.querySelector(".slds-page-header__col-title")!;
    expect(colTitle.firstElementChild!.tagName).toBe("NAV");
    expect(colTitle.firstElementChild!.nextElementSibling).toHaveClass("slds-media");
    expect(screen.getByRole("link", { name: "Minh Phát" })).toHaveAttribute("href", "/crm_accounts/1");
    expect(root.querySelector(".slds-media__figure")).toBeNull();
    expect(screen.getByText("10 mục • sắp theo tên")).toHaveClass("slds-page-header__meta-text");
  });
  it("wrapTitle drops the ellipsis class and keeps the full name in title", () => {
    render(<PageHeader title="Phòng khám Nha khoa Sài Gòn Tâm Đức chi nhánh quận Bảy" wrapTitle />);
    const title = screen.getByText(/Phòng khám Nha khoa/);
    expect(title).not.toHaveClass("slds-truncate");
    expect(title).toHaveAttribute("title", "Phòng khám Nha khoa Sài Gòn Tâm Đức chi nhánh quận Bảy");
  });

  it("without wrapTitle the title keeps slds-truncate", () => {
    render(<PageHeader title="Khách hàng" />);
    expect(screen.getByText("Khách hàng")).toHaveClass("slds-truncate");
  });
});
