// HistoryTab — the record page's "Lịch sử" tab rendering an approval decision row (DAILY-QUEUE #70).
// LayoutDetail.historyRows.test.ts pins the pure filter; this one pins the CALL SITE, so un-wiring
// historyDiffRows from the tab (back to a raw Object.entries) turns a test red instead of printing
// "[object Object]" on the customer's record page while every gate stays green.
// lib/api is mocked at the module seam (NotesTab/RelatedTab pattern); the tab itself is real code.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HistoryTab } from "./LayoutDetail";
import { ensureAuth, listRecordHistory, listTeamDirectory, listUserDirectory, type AuditEntry } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    listRecordHistory: vi.fn(),
    listUserDirectory: vi.fn(),
    listTeamDirectory: vi.fn(),
  };
});

// The row the server writes for an approval decision since #70: `definition` is the chain the
// decision executed — an object no field of the record describes.
const APPROVE_ROW: AuditEntry = {
  id: "a1",
  actorUserId: "u-rep",
  entityName: "crm_contracts",
  recordId: "r1",
  action: "approve",
  diff: {
    after: {
      approvalInstanceId: "i-1",
      comment: "ok bước 1",
      definition: { steps: [{ name: "Trưởng phòng duyệt", approverType: "role", approverId: "r-1" }] },
      processId: "p-1",
      step: 0,
    },
  },
  at: "2026-09-29T03:00:00Z",
};

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listRecordHistory).mockReset().mockResolvedValue({ entries: [APPROVE_ROW] });
  vi.mocked(listUserDirectory).mockReset().mockResolvedValue([]);
  vi.mocked(listTeamDirectory).mockReset().mockResolvedValue([]);
});

describe("HistoryTab — approval decision row (#70)", () => {
  it("lists the decision's scalar keys and never prints [object Object] for its definition", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { container } = render(
      <QueryClientProvider client={qc}>
        <HistoryTab entity="crm_contracts" recordId="r1" allFields={[]} />
      </QueryClientProvider>,
    );

    // SLDS activity timeline: each changed key is a label of the item's detail fields.
    expect(await screen.findByText("processId")).toBeInTheDocument();
    expect(screen.getByText("step")).toBeInTheDocument();
    expect(screen.getByText("comment")).toBeInTheDocument();
    expect(container.textContent).not.toContain("[object Object]");
    expect(screen.queryByText("definition")).not.toBeInTheDocument();
  });
});

// specs/051 FR-007 · DAILY-QUEUE #35: a share writes audit action `share` (diff.after.sharedWith) and a
// revoke writes `unshare` (diff.BEFORE.sharedWith). Both read as Vietnamese titles, never the raw action,
// and both name the person — the revoke row has no `after`, so without its own row it would name nobody.
describe("HistoryTab — share / unshare rows", () => {
  it("titles the rows Chia sẻ / Gỡ chia sẻ and names the person each time", async () => {
    vi.mocked(listUserDirectory).mockResolvedValue([
      { id: "u-a", email: "test@example.com", fullName: "Người Thử" },
      { id: "u-b", email: "binh@example.com", fullName: "Bình Trần" },
    ]);
    vi.mocked(listRecordHistory).mockResolvedValue({
      entries: [
        { id: "h2", actorUserId: "u-a", entityName: "crm_opportunities", recordId: "r1", action: "unshare", diff: { before: { sharedWith: "u-b" } }, at: "2026-09-30T04:00:00Z" },
        { id: "h1", actorUserId: "u-a", entityName: "crm_opportunities", recordId: "r1", action: "share", diff: { after: { sharedWith: "u-b" } }, at: "2026-09-30T03:00:00Z" },
      ],
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { container } = render(
      <QueryClientProvider client={qc}>
        <HistoryTab entity="crm_opportunities" recordId="r1" allFields={[]} />
      </QueryClientProvider>,
    );

    expect(await screen.findByText("Gỡ chia sẻ")).toBeInTheDocument();
    expect(screen.getByText("Chia sẻ")).toBeInTheDocument();
    expect(await screen.findAllByText("Người được chia sẻ")).toHaveLength(2);
    expect(await screen.findAllByText("Bình Trần")).toHaveLength(2);
    expect(container.textContent).not.toContain("sharedWith");
    expect(container.textContent).not.toMatch(/\bunshare\b|\bshare\b/);
  });
});

// #224: sharing with a whole team writes diff.after.sharedWithTeam (share) / diff.before.sharedWithTeam
// (unshare) — the team id, resolved to a name through /teams/directory; a team deleted since reads
// "Nhóm đã xoá", never the uuid.
describe("HistoryTab — team share / unshare rows", () => {
  it("names the team on both rows and marks a deleted team", async () => {
    vi.mocked(listTeamDirectory).mockResolvedValue([{ id: "t-kd", name: "Kinh doanh" }]);
    vi.mocked(listRecordHistory).mockResolvedValue({
      entries: [
        { id: "h3", actorUserId: "u-a", entityName: "crm_opportunities", recordId: "r1", action: "share", diff: { after: { sharedWithTeam: "t-gone" } }, at: "2026-09-30T05:00:00Z" },
        { id: "h2", actorUserId: "u-a", entityName: "crm_opportunities", recordId: "r1", action: "unshare", diff: { before: { sharedWithTeam: "t-kd" } }, at: "2026-09-30T04:00:00Z" },
        { id: "h1", actorUserId: "u-a", entityName: "crm_opportunities", recordId: "r1", action: "share", diff: { after: { sharedWithTeam: "t-kd" } }, at: "2026-09-30T03:00:00Z" },
      ],
    });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { container } = render(
      <QueryClientProvider client={qc}>
        <HistoryTab entity="crm_opportunities" recordId="r1" allFields={[]} />
      </QueryClientProvider>,
    );

    expect(await screen.findAllByText("Nhóm được chia sẻ")).toHaveLength(3);
    expect(await screen.findAllByText("Kinh doanh")).toHaveLength(2);
    expect(screen.getByText("Nhóm đã xoá")).toBeInTheDocument();
    expect(container.textContent).not.toContain("t-kd");
    expect(container.textContent).not.toContain("sharedWithTeam");
  });
});
