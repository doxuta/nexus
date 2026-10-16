// Component tests — TeamsList admin screen (queue #20/#21 team hierarchy).
// Pins the contract a third person can see:
//   * the list is the team tree (SLDS tree-grid): a child sits one level under its parent, and its
//     row names the parent ("Cấp trên" column: Vùng Bắc), not a vague "Nhóm con";
//   * the detail screen has a "Cấp trên (nhóm cha)" select that PUTs { parentId } — and when the
//     server refuses a loop (422 TEAM_PARENT_CYCLE) the reason is rendered inline (role=alert),
//     not swallowed into a toast the reader may miss.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { TeamsList } from "@/components/screens/admin/TeamsList";
import { logout } from "@/lib/api";
import type { AdminTeam } from "@/lib/admin";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { "Content-Type": "application/json" },
  });
}

const REGION: AdminTeam = { id: "t-region", tenantId: "t-1", name: "Vùng Bắc" };
const BRANCH: AdminTeam = { id: "t-branch", tenantId: "t-1", name: "Chi nhánh Hà Nội", parentId: "t-region" };
const OFFICE: AdminTeam = { id: "t-office", tenantId: "t-1", name: "PGD Cầu Giấy", parentId: "t-branch" };
const CYCLE_REASON = "không thể đặt cấp trên: nhóm được chọn chính là nhóm này hoặc đang nằm dưới nó — sẽ tạo thành vòng tròn";

// URL-routed fetch mock: list/get/members/users + PUT captured.
function routedFetch(teams: AdminTeam[], onPut: (url: string, body: unknown) => Response) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.includes("/auth/login")) {
      return jsonRes({ data: {
        accessToken: "at-test", refreshToken: "rt-test",
        user: { id: "u1", email: "admin@example.com", fullName: "Admin", isAdmin: true },
      } }, 200);
    }
    if (u.endsWith("/admin/teams") && method === "GET") return jsonRes({ data: { teams } }, 200);
    if (u.includes("/admin/team-sets") && method === "GET") return jsonRes({ data: { teamSets: [] } }, 200);
    if (u.includes("/members") && method === "GET") return jsonRes({ data: { members: [] } }, 200);
    if (u.includes("/admin/users") && method === "GET") return jsonRes({ data: { users: [] } }, 200);
    if (u.includes("/admin/teams/") && method === "GET") {
      const id = u.split("/admin/teams/")[1];
      const t = teams.find((x) => x.id === id);
      return t ? jsonRes({ data: { team: t } }, 200) : jsonRes({ error: "không tìm thấy team", code: "TEAM_NOT_FOUND" }, 404);
    }
    if (u.includes("/admin/teams/") && method === "PUT") return onPut(u, JSON.parse(String(init?.body)));
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

describe("TeamsList — team hierarchy", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("names each child's parent in the list", async () => {
    global.fetch = routedFetch([REGION, BRANCH, OFFICE], () => jsonRes({}, 500)) as unknown as typeof fetch;
    render(<TeamsList />);
    await waitFor(() => expect(screen.getByText("PGD Cầu Giấy")).toBeInTheDocument());
    const rowOf = (name: string) => screen.getByRole("button", { name }).closest("tr")!;
    expect(within(rowOf("Chi nhánh Hà Nội")).getByText("Vùng Bắc")).toBeInTheDocument();
    expect(within(rowOf("PGD Cầu Giấy")).getByText("Chi nhánh Hà Nội")).toBeInTheDocument();
  });

  it("the team-set card under the tree lists each set; the primary team is announced as such", async () => {
    const base = routedFetch([REGION, BRANCH], () => jsonRes({}, 500));
    const SET = {
      id: "3938de57-c9d9-453a-bada-ec31eef1883e",
      primaryTeamId: "t-region",
      teams: [{ id: "t-region", name: "Vùng Bắc" }, { id: "t-branch", name: "Chi nhánh Hà Nội" }],
      createdAt: "2026-07-25T12:17:12Z",
    };
    global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) =>
      String(url).includes("/admin/team-sets") ? jsonRes({ data: { teamSets: [SET] } }, 200) : base(url, init),
    ) as unknown as typeof fetch;
    render(<TeamsList />);
    const table = await screen.findByRole("table", { name: "Danh sách bộ nhóm" });
    const row = within(table).getAllByRole("row")[1];
    expect(within(row).getByRole("rowheader")).toHaveTextContent("set·3938de57");
    expect(within(row).getByText("Vùng Bắc").closest(".slds-badge")).toHaveTextContent("Vùng Bắc (Nhóm chính)");
    expect(within(row).getByText("Chi nhánh Hà Nội").closest(".slds-badge")).not.toHaveTextContent("Nhóm chính");
  });

  it("draws the hierarchy as a tree that collapses and expands", async () => {
    global.fetch = routedFetch([REGION, BRANCH, OFFICE], () => jsonRes({}, 500)) as unknown as typeof fetch;
    render(<TeamsList />);
    const tree = await screen.findByRole("treegrid", { name: "Cây nhóm làm việc" });
    await waitFor(() => expect(within(tree).getByText("PGD Cầu Giấy")).toBeInTheDocument());
    const rowOf = (name: string) => within(tree).getByRole("button", { name }).closest("tr")!;
    expect(rowOf("Vùng Bắc")).toHaveAttribute("aria-level", "1");
    expect(rowOf("Chi nhánh Hà Nội")).toHaveAttribute("aria-level", "2");
    expect(rowOf("PGD Cầu Giấy")).toHaveAttribute("aria-level", "3");

    fireEvent.click(within(rowOf("Vùng Bắc")).getByTitle("Thu gọn Vùng Bắc"));
    expect(rowOf("Vùng Bắc")).toHaveAttribute("aria-expanded", "false");
    expect(within(tree).queryByText("PGD Cầu Giấy")).not.toBeInTheDocument();
    fireEvent.click(within(rowOf("Vùng Bắc")).getByTitle("Mở rộng Vùng Bắc"));
    expect(within(tree).getByRole("button", { name: "Chi nhánh Hà Nội" })).toBeInTheDocument();
    expect(within(tree).getByRole("button", { name: "PGD Cầu Giấy" })).toBeInTheDocument();
  });

  it("refused parent (grandchild) shows the server's reason inline and keeps the old parent", async () => {
    const puts: unknown[] = [];
    global.fetch = routedFetch([REGION, BRANCH, OFFICE], (_url, body) => {
      puts.push(body);
      return jsonRes({ error: CYCLE_REASON, code: "TEAM_PARENT_CYCLE" }, 422);
    }) as unknown as typeof fetch;
    render(<TeamsList />);
    fireEvent.click(await screen.findByRole("button", { name: "Vùng Bắc" }));

    const select = await screen.findByLabelText(/Cấp trên/);
    // Own id is never offered as a parent.
    expect(Array.from((select as HTMLSelectElement).options).map((o) => o.value)).not.toContain("t-region");
    fireEvent.change(select, { target: { value: "t-office" } });
    fireEvent.click(screen.getAllByRole("button", { name: /Lưu/ })[1]);

    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(CYCLE_REASON));
    expect(puts).toEqual([{ parentId: "t-office" }]);
  });

  it("a legal parent is PUT as { parentId } and a detach as parentId \"\"", async () => {
    const puts: unknown[] = [];
    global.fetch = routedFetch([REGION, BRANCH, OFFICE], (url, body) => {
      puts.push(body);
      const id = url.split("/admin/teams/")[1];
      const t = [REGION, BRANCH, OFFICE].find((x) => x.id === id)!;
      return jsonRes({ data: { team: { ...t, parentId: (body as { parentId?: string }).parentId || undefined } } }, 200);
    }) as unknown as typeof fetch;
    render(<TeamsList />);
    fireEvent.click(await screen.findByRole("button", { name: "PGD Cầu Giấy" }));

    const select = await screen.findByLabelText(/Cấp trên/);
    expect((select as HTMLSelectElement).value).toBe("t-branch");
    fireEvent.change(select, { target: { value: "t-region" } });
    fireEvent.click(screen.getAllByRole("button", { name: /Lưu/ })[1]);
    await waitFor(() => expect(puts).toEqual([{ parentId: "t-region" }]));

    fireEvent.change(select, { target: { value: "" } });
    fireEvent.click(screen.getAllByRole("button", { name: /Lưu/ })[1]);
    await waitFor(() => expect(puts).toEqual([{ parentId: "t-region" }, { parentId: "" }]));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  // specs/074 f2-access-team-share (T216): the customer face keeps its Modal; only the Studio page (`inline`) uses the form in the page.
  it("customer face: '+ Tạo nhóm' still opens the Modal", async () => {
    global.fetch = routedFetch([REGION], () => jsonRes({}, 500)) as unknown as typeof fetch;
    render(<TeamsList />);
    fireEvent.click(await screen.findByRole("button", { name: "Tạo nhóm" }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
  });
});
