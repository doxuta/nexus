// Card + RelatedListCard — blueprint cards (default, related-list-table, loading, empty,
// visibly-hidden-card-header), SLDS 2.264.1.
import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { Building2, Contact } from "lucide-react";
import { Card, RelatedListCard } from "./Card";

describe("Card", () => {
  it("renders the blueprint header, body and 'Xem tất cả' footer", () => {
    const { container } = render(
      <Card
        title="Khách hàng"
        href="/crm_accounts"
        count={3}
        icon={{ glyph: Building2, name: "account" }}
        actions={<button className="slds-button slds-button_neutral">Mới</button>}
        viewAllHref="/crm_accounts"
      >
        Nội dung thẻ
      </Card>,
    );
    const article = container.firstElementChild!;
    expect(article.tagName).toBe("ARTICLE");
    expect(article).toHaveClass("slds-card");

    const headerRow = article.firstElementChild!;
    expect(headerRow).toHaveClass("slds-card__header", "slds-grid");
    const header = headerRow.firstElementChild!;
    expect(header.tagName).toBe("HEADER");
    expect(header).toHaveClass("slds-media", "slds-media_center", "slds-has-flexi-truncate");
    expect(header.querySelector(".slds-media__figure .slds-icon_container")).toHaveClass("slds-icon-standard-account");

    const heading = screen.getByRole("heading", { level: 2 });
    expect(heading).toHaveClass("slds-card__header-title");
    const link = within(heading).getByRole("link");
    expect(link).toHaveClass("slds-card__header-link", "slds-truncate");
    expect(link).toHaveAttribute("title", "Khách hàng (3)");
    expect(link).toHaveAttribute("href", "/crm_accounts");

    expect(screen.getByRole("button", { name: "Mới" }).parentElement).toHaveClass("slds-no-flex");
    expect(screen.getByText("Nội dung thẻ")).toHaveClass("slds-card__body", "slds-card__body_inner");

    const footer = article.querySelector("footer")!;
    expect(footer).toHaveClass("slds-card__footer");
    const viewAll = within(footer).getByRole("link");
    expect(viewAll).toHaveClass("slds-card__footer-action");
    expect(viewAll).toHaveTextContent("Xem tất cả");
    expect(viewAll.querySelector(".slds-assistive-text")).toHaveTextContent("Khách hàng");
  });

  it("drops body padding, hides the header visually and omits the footer on request", () => {
    const { container } = render(
      <Card title="Ẩn tiêu đề" hideHeader padded={false}>
        Thân
      </Card>,
    );
    expect(container.querySelector(".slds-card__header")).toHaveClass("slds-assistive-text");
    const body = screen.getByText("Thân");
    expect(body).toHaveClass("slds-card__body");
    expect(body).not.toHaveClass("slds-card__body_inner");
    expect(container.querySelector("footer")).toBeNull();
    // No href: the title is text, not a dead link.
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("shows the loading spinner instead of the body and footer", () => {
    render(
      <Card title="Công việc" loading viewAllHref="/tasks">
        Không được hiện
      </Card>,
    );
    const spinner = screen.getByRole("status");
    expect(spinner).toHaveClass("slds-spinner", "slds-spinner_small");
    expect(spinner).toHaveTextContent("Đang tải");
    expect(spinner.parentElement).toHaveClass("slds-card__body", "slds-card__body_inner");
    expect(screen.queryByText("Không được hiện")).toBeNull();
    expect(screen.queryByText("Xem tất cả")).toBeNull();
  });
});

const columns = [
  { key: "name", label: "Họ tên" },
  { key: "email", label: "Email" },
  { key: "title", label: "Chức danh" },
];
const rows = [
  { id: "c1", name: "Nguyễn Văn An", email: "an@minhphat.vn", title: "Giám đốc" },
  { id: "c2", name: "Trần Thị Bình", email: "binh@minhphat.vn", title: "Kế toán" },
];

describe("RelatedListCard", () => {
  it("renders the related-list-table card: counted title, blueprint table, row links", () => {
    render(
      <RelatedListCard
        title="Liên hệ"
        icon={{ glyph: Contact, name: "contact" }}
        columns={columns}
        rows={rows}
        rowKey={(r) => r.id}
        rowHref={(r) => `/crm_contacts/${r.id}`}
        viewAllHref="/crm_contacts"
      />,
    );
    expect(screen.getByRole("heading", { level: 2 }).querySelector("[title]")).toHaveAttribute("title", "Liên hệ (2)");

    const table = screen.getByRole("table", { name: "Liên hệ" });
    expect(table).toHaveClass(
      "slds-table",
      "slds-table_cell-buffer",
      "slds-no-row-hover",
      "slds-table_bordered",
      "slds-table_fixed-layout",
    );
    // Card body without padding (blueprint: data table flush to the card edges).
    expect(table.parentElement).toHaveClass("slds-card__body");
    expect(table.parentElement).not.toHaveClass("slds-card__body_inner");

    const headers = screen.getAllByRole("columnheader");
    expect(headers.map((h) => h.textContent)).toEqual(["Họ tên", "Email", "Chức danh"]);
    expect(headers[0].firstElementChild).toHaveClass("slds-truncate");
    expect(headers[0].firstElementChild).toHaveAttribute("title", "Họ tên");
    expect(headers[0].parentElement).toHaveClass("slds-line-height_reset");

    const bodyRows = within(table).getAllByRole("row").slice(1);
    expect(bodyRows).toHaveLength(2);
    expect(bodyRows[0]).toHaveClass("slds-hint-parent");
    const rowHeader = within(bodyRows[0]).getByRole("rowheader");
    const link = within(rowHeader).getByRole("link", { name: "Nguyễn Văn An" });
    expect(link).toHaveAttribute("href", "/crm_contacts/c1");
    // Reachable by Tab: no grid keyboard model, so no tabindex=-1 on the record link.
    expect(link).not.toHaveAttribute("tabindex");
    expect(within(bodyRows[0]).getByText("an@minhphat.vn")).toHaveAttribute("title", "an@minhphat.vn");
  });

  it("takes an explicit total and custom cell renderers", () => {
    render(
      <RelatedListCard
        title="Liên hệ"
        count={12}
        columns={[{ key: "name", label: "Họ tên", render: (r: (typeof rows)[number]) => <b>{r.name}</b> }]}
        rows={rows}
        rowKey={(r) => r.id}
      />,
    );
    expect(screen.getByTitle("Liên hệ (12)")).toBeInTheDocument();
    expect(screen.getByText("Trần Thị Bình").tagName).toBe("B");
  });

  it("collapses to the empty card state with no rows", () => {
    const { container } = render(
      <RelatedListCard title="Liên hệ" columns={columns} rows={[] as typeof rows} rowKey={(r) => r.id} viewAllHref="/x" />,
    );
    expect(screen.queryByRole("table")).toBeNull();
    expect(container.querySelector("footer")).toBeNull();
    expect(screen.getByTitle("Liên hệ (0)")).toBeInTheDocument();
  });
});
