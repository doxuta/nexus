// specs/073 kh4 T273 (FR-013): the four sub tabs of Vận hành and Bảo trì, a named nav, short labels below 768px.
import { render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import { parseStudioPath } from "@/lib/studioRoutes";
import { ZoneTabs as Tabs } from "./ZoneTabs";

// The Vận hành tab dot reads the shared feed queries, so the tabs need a QueryClient.
const ZoneTabs = (p: React.ComponentProps<typeof Tabs>) => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <Tabs {...p} />
  </QueryClientProvider>
);

const at = (path: string) => parseStudioPath(`/studio/du-an/northwind/${path}`)!.route;

describe("ZoneTabs", () => {
  it("Vận hành: a nav 'Mục trong khu' with the four tabs as links, the page's tab current", () => {
    render(<ZoneTabs slug="northwind" route={at("van-hanh/nguoi-dung/nhat-ky")} />);
    const nav = screen.getByRole("navigation", { name: "Mục trong khu" });
    const links = within(nav).getAllByRole("link");
    expect(links.map((a) => a.getAttribute("href"))).toEqual([
      "/studio/du-an/northwind/van-hanh/nguoi-dung",
      "/studio/du-an/northwind/van-hanh/du-lieu",
      "/studio/du-an/northwind/van-hanh/tu-dong",
      "/studio/du-an/northwind/van-hanh/suc-khoe",
    ]);
    expect(links.map((a) => a.getAttribute("aria-current"))).toEqual(["page", null, null, null]);
  });

  it("Bảo trì: Lịch sử xuất bản · Sao lưu · Khám sức khoẻ · Nâng cấp, with the short labels beside the full ones", () => {
    render(<ZoneTabs slug="northwind" route={at("bao-tri/kham")} />);
    const nav = screen.getByRole("navigation", { name: "Mục trong khu" });
    expect(within(nav).getAllByRole("link").map((a) => a.getAttribute("aria-current"))).toEqual([null, null, "page", null]);
    const spans = [...nav.querySelectorAll("span")].map((s) => s.textContent);
    expect(spans).toEqual(expect.arrayContaining(["Lịch sử xuất bản", "Phiên bản", "Khám sức khoẻ", "Khám", "Nâng cấp"]));
  });

  it("the full label hides below 768px and the short one hides from 768px up", () => {
    render(<ZoneTabs slug="northwind" route={at("van-hanh/tu-dong")} />);
    expect(screen.getByText("Việc tự động & kết nối")).toHaveClass("max-md:hidden");
    expect(screen.getByText("Tự động")).toHaveClass("md:hidden");
  });

  it("draws nothing for a Dựng page, Cài đặt, or an unknown path", () => {
    const a = render(<ZoneTabs slug="northwind" route={at("dung/du-lieu")} />);
    expect(a.container).toBeEmptyDOMElement();
    const b = render(<ZoneTabs slug="northwind" route={at("cai-dat")} />);
    expect(b.container).toBeEmptyDOMElement();
    const c = render(<ZoneTabs slug="northwind" route={null} />);
    expect(c.container).toBeEmptyDOMElement();
  });
});
