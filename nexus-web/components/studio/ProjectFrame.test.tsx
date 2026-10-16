// specs/073 kh1b1 T154/T157/T168 (FR-013..FR-019): the project frame — strip, zones, steps, one surface, focus, title.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { Card } from "@/components/slds/Card";

let pathname = "/studio/du-an/northwind/dung/du-lieu";
vi.mock("next/navigation", () => ({
  usePathname: () => pathname,
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
}));
vi.mock("./DraftBar", () => ({ DraftBar: () => <span data-testid="draft-bar" /> }));
vi.mock("@/components/shell/ModeSwitch", () => ({ useFacePaths: () => ({ appHref: "/accounts" }) }));

interface Id {
  status: "loading" | "error" | "ready";
  me?: { isPlatformOperator?: boolean; workspace?: { tenantSlug: string; tenantName: string } | null };
  matchesAll: boolean;
}
let id: Id = { status: "ready", me: { isPlatformOperator: true, workspace: null }, matchesAll: true };
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: id.status, me: id.me, home: "northwind", sessionReadable: true, matches: () => id.matchesAll, retry: vi.fn() }),
  useStudioTransition: () => false,
  useStudioTransitionFailed: () => false,
  useReloadOnIdentityStorage: () => {},
  beginTransition: vi.fn(),
  clearAndAssign: vi.fn(),
  endPreviewIfAny: vi.fn(),
  enterProject: vi.fn(),
  exitProject: vi.fn(),
  switchProject: vi.fn(),
}));

let entityRows: unknown[] | Error = [];
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  listEntities: () => (entityRows instanceof Error ? Promise.reject(entityRows) : Promise.resolve(entityRows)),
}));

import { ProjectFrame } from "./ProjectFrame";

let qc = new QueryClient();
const wrap = (slug: string, body: ReactNode = <p>nội dung</p>) => (
  <QueryClientProvider client={qc}>
    <ProjectFrame slug={slug}>{body}</ProjectFrame>
  </QueryClientProvider>
);

beforeEach(() => {
  qc = new QueryClient();
  entityRows = [];
  pathname = "/studio/du-an/northwind/dung/du-lieu";
  id = { status: "ready", me: { isPlatformOperator: true, workspace: null }, matchesAll: true };
  document.title = "";
});

describe("ProjectFrame — an openable project", () => {
  it("DraftBar (draft → publish, kh7 T371) sits in the strip of a customer project, never in the workshop home", () => {
    id = { ...id, me: { isPlatformOperator: true, workspace: { tenantSlug: "northwind", tenantName: "Khách A" } } };
    const { unmount } = render(wrap("northwind"));
    expect(within(screen.getByTestId("project-header")).getByTestId("draft-bar")).toBeInTheDocument();
    unmount();
    id = { ...id, me: { isPlatformOperator: true, workspace: null } };
    const home = render(wrap("northwind"));
    expect(screen.queryByTestId("draft-bar")).toBeNull();
    home.unmount();
    id = { ...id, matchesAll: false };
    render(wrap("acme"));
    expect(screen.queryByTestId("draft-bar")).toBeNull();
  });

  it("h1 is the workshop name; zones and steps are two separately named navs", () => {
    render(wrap("northwind"));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Xưởng Nexus");
    expect(screen.getByRole("navigation", { name: "Khu của dự án" })).toBeInTheDocument();
    const steps = screen.getByRole("navigation", { name: "Các bước dựng" });
    // the workshop has 7 steps (no "deliver") and nothing is ticked: nothing is known about a workshop
    expect(within(steps).getAllByRole("link")).toHaveLength(7);
    expect(within(steps).getByRole("link", { name: /Dữ liệu/ })).toHaveAttribute("aria-current", "step");
  });

  it("one card for the whole surface: a builder's own Card is flat inside it", () => {
    const { container } = render(wrap("northwind", <Card title="Danh sách bảng">thân</Card>));
    expect(container.querySelectorAll(".slds-card")).toHaveLength(1);
    expect(screen.getByText("Danh sách bảng").closest("section")).not.toBeNull();
  });

  it("the surface is the one main#nx-main, which the shell's skip link targets", () => {
    render(wrap("northwind"));
    const mains = screen.getAllByRole("main");
    expect(mains).toHaveLength(1);
    expect(mains[0]).toHaveAttribute("id", "nx-main");
    expect(within(mains[0]).getByText("nội dung")).toBeInTheDocument();
  });

  it("document.title is '{page} · {project} · Nexus Studio'", () => {
    render(wrap("northwind"));
    expect(document.title).toBe("Dữ liệu · Xưởng Nexus · Nexus Studio");
  });

  it("a customer project in a build session names itself by the session", () => {
    id = { status: "ready", me: { workspace: { tenantSlug: "acme", tenantName: "Nha khoa Acme" } }, matchesAll: true };
    pathname = "/studio/du-an/acme/dung/du-lieu";
    render(wrap("acme"));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Nha khoa Acme");
    // a customer project has the "deliver" step too
    expect(within(screen.getByRole("navigation", { name: "Các bước dựng" })).getAllByRole("link")).toHaveLength(8);
  });

  it("the first mount puts focus on the h1; a page change inside the project puts it on the surface's first h2, not the h1", () => {
    const { rerender } = render(wrap("northwind"));
    expect(screen.getByRole("heading", { level: 1 })).toHaveFocus();
    pathname = "/studio/du-an/northwind/dung/man-hinh";
    rerender(wrap("northwind"));
    const h2 = within(screen.getByRole("main")).getAllByRole("heading", { level: 2, hidden: true })[0];
    expect(h2).toHaveFocus();
    expect(h2).toHaveTextContent("Màn hình");
    expect(document.title).toBe("Màn hình · Xưởng Nexus · Nexus Studio");
  });

  it("while the identity is still loading nothing is focused; the h1 is focused once the real strip is drawn", () => {
    id = { status: "loading", matchesAll: false };
    const { rerender } = render(wrap("northwind"));
    expect(screen.getByRole("heading", { level: 1 })).not.toHaveFocus();
    id = { status: "ready", me: { isPlatformOperator: true, workspace: null }, matchesAll: true };
    rerender(wrap("northwind"));
    expect(screen.getByRole("heading", { level: 1 })).toHaveFocus();
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Xưởng Nexus");
  });

  it("changing the project puts focus back on the h1", () => {
    const { rerender } = render(wrap("northwind"));
    pathname = "/studio/du-an/acme/dung/du-lieu";
    id = { status: "ready", me: { workspace: { tenantSlug: "acme", tenantName: "Acme" } }, matchesAll: true };
    rerender(wrap("acme"));
    expect(screen.getByRole("heading", { level: 1 })).toHaveFocus();
  });

  it("does not take focus on the first mount while the user is typing in a field", () => {
    const input = document.createElement("input");
    document.body.appendChild(input);
    input.focus();
    render(wrap("northwind"));
    expect(input).toHaveFocus();
    input.remove();
  });

  it("an unmatched path shows the plain 'no such page' inside the frame, no steps bar", () => {
    pathname = "/studio/du-an/northwind/khong-co-trang-nay";
    render(wrap("northwind"));
    expect(screen.queryByRole("navigation", { name: "Các bước dựng" })).not.toBeInTheDocument();
    expect(document.title).toBe("Không có trang này trong dự án · Xưởng Nexus · Nexus Studio");
  });
});

describe("ProjectFrame — a project that cannot be opened (T157)", () => {
  it("h1 is just the code; no zones, no steps; the gate sentence replaces the page and the page never mounts", () => {
    id = { status: "ready", me: { isPlatformOperator: true, workspace: null }, matchesAll: false };
    pathname = "/studio/du-an/abc/dung/du-lieu";
    const body = vi.fn(() => <p>trang không được mount</p>);
    const Body = () => body();
    render(wrap("abc", <Body />));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("abc");
    expect(screen.queryByRole("navigation", { name: "Khu của dự án" })).not.toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "Các bước dựng" })).not.toBeInTheDocument();
    expect(screen.getByText("Mở dự án abc để dựng.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Mở dự án" })).toBeInTheDocument();
    expect(body).not.toHaveBeenCalled();
  });

  it("an invalid project code never reaches the page either", () => {
    id = { status: "ready", me: { isPlatformOperator: true, workspace: null }, matchesAll: false };
    pathname = "/studio/du-an/Nexus%2F..";
    act(() => {});
    render(wrap("Nexus%2F.."));
    expect(screen.queryByText("nội dung")).not.toBeInTheDocument();
  });

  it("sub tabs 'Mục trong khu' are drawn in Vận hành and Bảo trì, never in Dựng or Cài đặt", () => {
    for (const [path, shown] of [
      ["/studio/du-an/northwind/van-hanh/nguoi-dung", true],
      ["/studio/du-an/northwind/bao-tri/kham", true],
      ["/studio/du-an/northwind/dung/du-lieu", false],
      ["/studio/du-an/northwind/cai-dat", false],
    ] as const) {
      pathname = path;
      const { unmount } = render(wrap("northwind"));
      expect(screen.queryByRole("navigation", { name: "Mục trong khu" }) !== null).toBe(shown);
      unmount();
    }
  });
});

describe("ProjectFrame — data step status is inferred from the project's real tables (f2-data-list-leftovers)", () => {
  const customer = () => {
    id = { ...id, me: { isPlatformOperator: true, workspace: { tenantSlug: "northwind", tenantName: "Khách A" } } };
  };
  it("no table: Chưa đủ; tables: Đã đủ with the count", async () => {
    customer();
    const a = render(wrap("northwind"));
    expect(await screen.findByText("Chưa đủ")).toBeInTheDocument();
    a.unmount();
    qc = new QueryClient();
    entityRows = [{ name: "a" }, { name: "b" }];
    render(wrap("northwind"));
    expect(await screen.findByText("Đã đủ")).toBeInTheDocument();
  });
  it("load error or workshop home: no verdict, the static sentence stays", async () => {
    customer();
    entityRows = new Error("boom");
    const a = render(wrap("northwind"));
    await act(async () => {});
    expect(screen.queryByText("Chưa đủ")).toBeNull();
    expect(screen.queryByText("Đã đủ")).toBeNull();
    a.unmount();
    id = { ...id, me: { isPlatformOperator: true, workspace: null } };
    entityRows = [{ name: "a" }];
    render(wrap("northwind"));
    await act(async () => {});
    expect(screen.queryByText("Đã đủ")).toBeNull();
  });
});
