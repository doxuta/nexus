// MarkdownLite — the anti-exfil link contract (AI-MODULES, "safe internal links").
// The component renders EXACTLY ONE link shape: [label](/internal/path) whitelisted by
// isSafeInternalHref, via next/link. EVERY other link-looking string must stay dead literal
// text — external http(s), protocol-relative //, javascript:, data:, mailto:, backslash
// tricks — proven here by asserting NO <a> element exists AND the raw text is still visible.
// This is a structural whitelist: these tests are the property's regression pin for both the
// AI Builder chat and the global ⌘K assistant (same component).
import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { MarkdownLite, isSafeInternalHref } from "./MarkdownLite";

describe("isSafeInternalHref — the structural whitelist", () => {
  it("accepts path-absolute internal routes (query + hash included)", () => {
    expect(isSafeInternalHref("/")).toBe(true);
    expect(isSafeInternalHref("/reports/rep-1")).toBe(true);
    expect(isSafeInternalHref("/home?app=phong_kham")).toBe(true);
    expect(isSafeInternalHref("/studio/model#fields")).toBe(true);
    // A colon AFTER the first "/" is path data, not a scheme.
    expect(isSafeInternalHref("/docs/a:b")).toBe(true);
  });

  it("rejects everything else — external, protocol-relative, schemes, backslash, junk", () => {
    expect(isSafeInternalHref("https://evil.com")).toBe(false);
    expect(isSafeInternalHref("http://evil.com")).toBe(false);
    expect(isSafeInternalHref("//evil.com")).toBe(false);
    expect(isSafeInternalHref("/\\evil.com")).toBe(false); // WHATWG folds \ → / ⇒ //evil.com
    expect(isSafeInternalHref("javascript:alert(1)")).toBe(false);
    expect(isSafeInternalHref("data:text/html,<script>1</script>")).toBe(false);
    expect(isSafeInternalHref("mailto:a@b.c")).toBe(false);
    expect(isSafeInternalHref("reports/rep-1")).toBe(false); // relative — not path-absolute
    expect(isSafeInternalHref(" /reports")).toBe(false); // leading space is not "/"
    expect(isSafeInternalHref("")).toBe(false);
    expect(isSafeInternalHref(`/${"a".repeat(200)}`)).toBe(false); // 201 chars — over the cap
    expect(isSafeInternalHref(`/${"a".repeat(199)}`)).toBe(true); // 200 exactly — at the cap
  });
});

describe("MarkdownLite — safe internal links render via next/link", () => {
  it("renders [label](/path) as a real SPA link with the exact href", () => {
    render(<MarkdownLite text="Xem chi tiết ở [Báo cáo doanh thu](/reports/rep-1) nhé." />);
    const a = screen.getByRole("link", { name: "Báo cáo doanh thu" });
    expect(a).toHaveAttribute("href", "/reports/rep-1");
    // The bracket/paren syntax is consumed — only the label shows.
    expect(screen.queryByText(/\[Báo cáo doanh thu\]/)).toBeNull();
  });

  it("keeps query strings and works inside bullets and alongside bold/code", () => {
    const { container } = render(
      <MarkdownLite text={"- Mở **app** tại [Trang chủ](/home?app=phong_kham) rồi chạy `seed`"} />,
    );
    expect(screen.getByRole("link", { name: "Trang chủ" })).toHaveAttribute("href", "/home?app=phong_kham");
    expect(container.querySelector("strong")).toHaveTextContent("app");
    expect(container.querySelector("code")).toHaveTextContent("seed");
    expect(container.querySelector("ul")).not.toBeNull();
  });

  it("renders multiple safe links in one line, each with its own target", () => {
    render(<MarkdownLite text="[Bảng điều khiển](/dashboards/db-1) và [Việc Hôm Nay](/studio/workqueues)" />);
    expect(screen.getByRole("link", { name: "Bảng điều khiển" })).toHaveAttribute("href", "/dashboards/db-1");
    expect(screen.getByRole("link", { name: "Việc Hôm Nay" })).toHaveAttribute("href", "/studio/workqueues");
  });
});

describe("MarkdownLite — every other link shape stays DEAD TEXT (anti-exfil pin)", () => {
  // Each hostile target must produce ZERO <a> elements and keep the literal text on screen.
  const HOSTILE: Array<[string, string]> = [
    ["external https", "[bấm vào đây](https://evil.example/x?y=1)"],
    ["external http", "[link](http://evil.example)"],
    ["protocol-relative", "[link](//evil.example/steal)"],
    ["backslash protocol-relative", "[link](/\\evil.example)"],
    ["javascript:", "[link](javascript:alert(1))"],
    ["data:", "[link](data:text/html;base64,PHNjcmlwdD4)"],
    ["mailto:", "[link](mailto:a@evil.example)"],
    ["relative path", "[link](reports/rep-1)"],
  ];

  for (const [name, md] of HOSTILE) {
    it(`${name} → no anchor, literal text preserved`, () => {
      const { container } = render(<MarkdownLite text={`Xem ${md} ngay.`} />);
      expect(container.querySelector("a")).toBeNull();
      // The raw markdown stays visible verbatim — nothing is hidden or half-rendered.
      expect(container.textContent).toContain(md);
    });
  }

  it("an over-long internal-looking target (>200 chars) also stays dead text", () => {
    const long = `/reports/${"a".repeat(220)}`;
    const { container } = render(<MarkdownLite text={`[link](${long})`} />);
    expect(container.querySelector("a")).toBeNull();
    expect(container.textContent).toContain(`[link](${long})`);
  });

  it("a table cell with a hostile link is equally dead (renderInline is the single seam)", () => {
    const { container } = render(
      <MarkdownLite text={"| Cột |\n| --- |\n| [x](https://evil.example) |"} />,
    );
    expect(container.querySelector("table")).not.toBeNull();
    expect(container.querySelector("a")).toBeNull();
    expect(container.textContent).toContain("[x](https://evil.example)");
  });

  it("a table cell with a SAFE link renders it (the one capability, everywhere inline text renders)", () => {
    const { container } = render(
      <MarkdownLite text={"| Cột |\n| --- |\n| [Mở](/reports/rep-2) |"} />,
    );
    expect(container.querySelector("table")).not.toBeNull();
    expect(screen.getByRole("link", { name: "Mở" })).toHaveAttribute("href", "/reports/rep-2");
  });
});

describe("MarkdownLite — pre-existing rendering unchanged (regression)", () => {
  it("bold, code, headings, dividers, bullets and plain brackets still render as before", () => {
    const { container } = render(
      <MarkdownLite text={"### Tiêu đề\n**đậm** và `code`\n\n---\n- một\n- hai\nvăn bản [ngoặc] thường (x)"} />,
    );
    expect(container.querySelector("strong")).toHaveTextContent("đậm");
    expect(container.querySelector("code")).toHaveTextContent("code");
    expect(container.querySelector("hr")).not.toBeNull();
    expect(container.querySelectorAll("li")).toHaveLength(2);
    expect(container.textContent).toContain("văn bản [ngoặc] thường (x)");
    expect(container.querySelector("a")).toBeNull(); // brackets alone never make a link
  });
});

// specs/076 ai3 T053: the docked assistant's SLDS look and link resolver; the default look is unchanged.
describe("MarkdownLite variant=slds + resolveHref", () => {
  it("uses SLDS classes and none of the Nexus colour tokens", () => {
    const { container } = render(<MarkdownLite variant="slds" text={"**đậm** và `mã`\n\n- một\n- hai"} />);
    const html = container.innerHTML;
    expect(html).toContain("slds-");
    expect(html).not.toMatch(/text-fg|rounded-input|text-fnt|bg-panel/);
  });
  it("resolveHref maps the path; null makes it plain text", () => {
    render(<MarkdownLite variant="slds" resolveHref={(h) => (h === "/reports" ? "/studio/du-an/a/dung/bao-cao" : null)} text={"[Báo cáo](/reports) và [Lạ](/xyz)"} />);
    expect(screen.getByRole("link", { name: "Báo cáo" }).getAttribute("href")).toBe("/studio/du-an/a/dung/bao-cao");
    expect(screen.queryByRole("link", { name: "Lạ" })).toBeNull();
    expect(screen.getByText(/\[Lạ\]\(\/xyz\)/)).toBeTruthy();
  });
  it("the default look keeps its classes", () => {
    const { container } = render(<MarkdownLite text="**a**" />);
    expect(container.innerHTML).toContain("text-fg");
  });
});
