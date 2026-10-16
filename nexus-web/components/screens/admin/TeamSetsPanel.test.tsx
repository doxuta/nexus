// Component tests — TeamSetsPanel on SLDS (shared by /admin/sharing and /admin/teams; specs/038 T048).
// Read-only inspect view: one base-table row per engine-derived team set, its teams as badges with
// the primary team marked for sighted AND screen-reader users; honest empty / error states.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { TeamSetsPanel } from "@/components/screens/admin/TeamSetsPanel";
import { SharingAdmin } from "@/components/screens/admin/SharingAdmin";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { logout } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const SETS = [
  {
    id: "3938de57-c9d9-453a-bada-ec31eef1883e",
    primaryTeamId: "t-sales",
    teams: [{ id: "t-sales", name: "Đội Sales" }, { id: "t-support", name: "Đội hỗ trợ" }],
    createdAt: "2026-07-25T12:17:12Z",
  },
  { id: "0b539ff2-0000-0000-0000-000000000000", primaryTeamId: null, teams: [], createdAt: "2026-07-25T12:17:12Z" },
];

function mockTeamSets(res: () => Response) {
  global.fetch = vi.fn(async (url: RequestInfo | URL) =>
    String(url).includes("/admin/team-sets") ? res() : jsonRes({ error: "unmocked", code: "TEST_UNMOCKED" }, 500),
  ) as unknown as typeof fetch;
}

describe("TeamSetsPanel (SLDS)", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
  });

  it("lists each set as a table row: short id, team count, team badges with the primary marked", async () => {
    mockTeamSets(() => jsonRes({ data: { teamSets: SETS } }));
    render(<TeamSetsPanel />);

    const table = await screen.findByRole("table", { name: "Danh sách bộ nhóm" });
    const rows = within(table).getAllByRole("row");
    expect(rows).toHaveLength(1 + SETS.length);
    const first = within(rows[1]);
    expect(first.getByRole("rowheader")).toHaveTextContent("set·3938de57");
    expect(first.getByText("Đội hỗ trợ")).toHaveClass("slds-badge");
    expect(first.getByText("Đội Sales").closest(".slds-badge")).toHaveTextContent("Đội Sales (Nhóm chính)");
    expect(within(rows[2]).getByText(/Bộ nhóm rỗng/)).toBeInTheDocument();
    expect(screen.getByText("Bộ nhóm (Team Sets) (2)")).toBeInTheDocument();
  });

  it("no sets → illustration empty state, not an empty table", async () => {
    mockTeamSets(() => jsonRes({ data: { teamSets: [] } }));
    render(<TeamSetsPanel />);
    expect(await screen.findByText("Chưa có bộ nhóm nào")).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("load failure → error notification with the server message", async () => {
    mockTeamSets(() => jsonRes({ error: "Không đủ quyền", code: "FORBIDDEN" }, 403));
    render(<TeamSetsPanel />);
    expect(await screen.findByText(/Không đủ quyền/)).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });
});

describe("SharingAdmin (SLDS)", () => {
  let originalFetch: FetchFn;
  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    await logout();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    mockTeamSets(() => jsonRes({ data: { teamSets: SETS } }));
  });
  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
  });

  it("is read-only: model + check order + team sets + the honest roadmap note, no action buttons", async () => {
    render(<SharingAdmin />);
    expect(screen.getByRole("heading", { name: "Chia sẻ & Quyền sở hữu" })).toBeInTheDocument();
    expect(screen.getByText("owner")).toBeInTheDocument();
    expect(screen.getByText("created_by")).toBeInTheDocument();
    expect(screen.getByText(/Chia sẻ theo quy tắc — thuộc lộ trình/)).toBeInTheDocument();
    await screen.findByRole("table", { name: "Danh sách bộ nhóm" });
    expect(screen.queryAllByRole("button")).toHaveLength(0);
  });

  // specs/074 f2-access-team-share (T218): the Studio page draws SharingAdmin inside EmbeddedSurface: one surface, no card in a card.
  it("inside EmbeddedSurface the sharing page draws 0 .slds-card and 0 h1", async () => {
    mockTeamSets(() => jsonRes({ data: { teamSets: SETS } }));
    const { container } = render(<EmbeddedSurface><SharingAdmin /></EmbeddedSurface>);
    await screen.findByRole("table", { name: "Danh sách bộ nhóm" });
    expect(container.querySelectorAll(".slds-card")).toHaveLength(0);
    expect(container.querySelectorAll("h1")).toHaveLength(0);
  });
});
