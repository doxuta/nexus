// specs/062 L3 T056 — the per-record History tab names a Nexus builder who edited the record.
// Chain: customer user (directory) -> engine id -> server actorLabel (+ "Người dựng Nexus" badge) -> short id.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { HistoryTab } from "@/components/layout/detail/HistoryTab";
import { listRecordHistory, type AuditEntry } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn(async () => undefined),
  listRecordHistory: vi.fn(),
  listUserDirectory: vi.fn(async () => [{ id: "u1", email: "nv@example.com", fullName: "Nhân viên" }]),
}));

const BUILDER = "b0000000-aaaa-4bbb-8ccc-000000000001";
const ENGINE = "f1000000-0000-0000-0000-000000000000";
const row = (id: string, actorUserId: string, extra: Partial<AuditEntry> = {}): AuditEntry => ({
  id, actorUserId, entityName: "crm_contacts", recordId: "r1", action: "update",
  diff: { before: { first_name: "A" }, after: { first_name: "B" } }, at: "2026-10-08T01:00:00Z", ...extra,
});

function renderTab() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <HistoryTab entity="crm_contacts" recordId="r1" allFields={[]} />
    </QueryClientProvider>,
  );
}

describe("HistoryTab actor naming", () => {
  beforeEach(() => vi.mocked(listRecordHistory).mockReset());

  it("a builder row shows the server label with the builder badge, never a short id", async () => {
    vi.mocked(listRecordHistory).mockResolvedValue({ entries: [row("a", BUILDER, { actorLabel: "An Nguyễn (Nexus)" })] });
    renderTab();
    expect(await screen.findByText("An Nguyễn (Nexus)")).toBeInTheDocument();
    expect(screen.getByText("Người dựng Nexus")).toBeInTheDocument();
    expect(screen.queryByText(BUILDER.slice(0, 8))).not.toBeInTheDocument();
  });

  it("a customer user keeps their directory name and gets no badge; an engine id keeps its system label", async () => {
    vi.mocked(listRecordHistory).mockResolvedValue({
      entries: [row("a", "u1"), row("b", ENGINE, { actorLabel: "should not win" })],
    });
    renderTab();
    expect(await screen.findByText("Nhân viên")).toBeInTheDocument();
    expect(screen.getByText("Quy tắc tự động")).toBeInTheDocument();
    expect(screen.queryByText("Người dựng Nexus")).not.toBeInTheDocument();
  });

  it("an unknown actor without a label still falls back to the short id", async () => {
    vi.mocked(listRecordHistory).mockResolvedValue({ entries: [row("a", BUILDER)] });
    renderTab();
    expect(await screen.findByText(BUILDER.slice(0, 8))).toBeInTheDocument();
    expect(screen.queryByText("Người dựng Nexus")).not.toBeInTheDocument();
  });
});
