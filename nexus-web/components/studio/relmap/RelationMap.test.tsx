// specs/075 T145/T159: the relation map page — four states, selection modes, search, a11y group, ?bang= focus, callers pinned.
// xyflow is replaced by a minimal stand-in (jsdom has no layout): it renders the node component of every node and one
// button per edge, which is all the map's own logic touches.
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@xyflow/react", () => {
  const stub = {
    zoomIn: vi.fn(), zoomOut: vi.fn(), fitView: vi.fn(), setCenter: vi.fn(), getZoom: () => 1,
    getNode: (id: string) => ({ id, position: { x: 0, y: 0 } }),
  };
  return {
    ReactFlowProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
    useReactFlow: () => stub,
    applyNodeChanges: (_c: unknown, ns: unknown) => ns,
    Handle: () => null,
    Position: { Left: "left", Right: "right" },
    BaseEdge: () => null,
    EdgeLabelRenderer: ({ children }: { children: React.ReactNode }) => <>{children}</>,
    getSmoothStepPath: () => ["M0 0", 0, 0],
    ReactFlow: ({ nodes, edges, nodeTypes, onEdgeClick, onConnect }: { nodes: Array<{ id: string; data: unknown }>; edges: Array<{ id: string }>; nodeTypes: Record<string, React.ComponentType<{ data: unknown }>>; onEdgeClick: (e: unknown, edge: { id: string }) => void; onConnect: (c: { source: string; target: string }) => void }) => {
      const Node = nodeTypes.table;
      return (
        <div data-testid="flow">
          {nodes.map((n) => <div key={n.id} data-node={n.id}><Node data={n.data} /></div>)}
          <b data-testid="connect" onClick={() => onConnect({ source: "crm_accounts", target: "crm_notes" })} />
          {edges.map((e) => <i key={e.id} data-edge={e.id} onClick={() => onEdgeClick({}, e)} />)}
        </div>
      );
    },
  };
});

const api = vi.hoisted(() => ({
  ensureAuth: vi.fn(),
  listEntities: vi.fn(),
  listFields: vi.fn(),
  listAllRelationships: vi.fn(),
  getRelationshipImpact: vi.fn(),
  deleteRelationshipById: vi.fn(),
  setRelationshipViewConfig: vi.fn(),
  addField: vi.fn(),
  createRelationship: vi.fn(),
  getCachedMe: () => ({ tenantId: "t1" }),
}));
vi.mock("@/lib/api", async (importOriginal) => ({ ...(await importOriginal<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn(), info: vi.fn() } }));


// publish state of the project, switched per test (specs/075 L5a)
const publish = vi.hoisted(() => ({ state: { delivered: false, draftableTypes: ["layout", "field"], directTypes: [] as string[] } as Record<string, unknown> }));
vi.mock("@/components/studio/usePublishState", () => ({ usePublishState: () => ({ data: publish.state }) }));

import { RelationMap } from "./RelationMap";

const ents = [
  { id: "1", name: "crm_accounts", label: "Khách hàng" },
  { id: "2", name: "crm_events", label: "Lịch hẹn" },
  { id: "3", name: "crm_notes", label: "Ghi chú" },
];
const rel = { id: "r1", name: "crm_events_account", parentEntityName: "crm_accounts", childEntityName: "crm_events", relationshipType: "1-N", childFkColumn: "account_id" };

const nodeBtn = (id: string) => document.querySelector(`[data-node=${id}] button`) as HTMLElement;
function mount(props: { focusEntity?: string } = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><RelationMap slug="northwind" {...props} /></QueryClientProvider>);
}

beforeEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
  api.ensureAuth.mockResolvedValue(undefined);
  api.listEntities.mockResolvedValue(ents);
  api.listFields.mockImplementation(async (name: string) =>
    name === "crm_events" ? [{ name: "account", label: "Khách hàng liên quan", fieldType: "relation", options: { relatedEntity: "crm_accounts" } }] : [{ name: "name", label: "Tên", fieldType: "text" }]);
  api.listAllRelationships.mockResolvedValue([rel]);
  api.getRelationshipImpact.mockResolvedValue({ relationship: rel, affectedRecords: 2, dependentRollups: [], dependentLookups: [], deletable: true });
});

describe("RelationMap", () => {
  it("draws the header summary, one text button per table, and a labelled group", async () => {
    mount();
    expect(await screen.findByText("3 bảng · 1 liên kết")).toBeInTheDocument();
    expect(screen.getByRole("group", { name: /Sơ đồ liên kết: 3 bảng, 1 liên kết/ })).toBeInTheDocument();
    expect(nodeBtn("crm_accounts")).toHaveAttribute("aria-pressed", "false");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("selecting a table switches the right column to table mode; the link list is the keyboard way to a link", async () => {
    mount();
    await screen.findByText("3 bảng · 1 liên kết");
    expect(screen.getByText("Liên kết (1)")).toBeInTheDocument();
    fireEvent.click(nodeBtn("crm_events"));
    expect(screen.getByRole("link", { name: "Mở bảng" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu/crm_events");
    expect(nodeBtn("crm_events")).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(screen.getByRole("button", { name: "Tất cả liên kết" }));
    fireEvent.click(screen.getAllByRole("button", { name: /Khách hàng → Lịch hẹn/ })[0]);
    await waitFor(() => expect(api.getRelationshipImpact).toHaveBeenCalledWith("r1"));
  });

  it("clicking an edge opens the link; a removal confirm reloads the map after DELETE", async () => {
    api.deleteRelationshipById.mockResolvedValue(undefined);
    const { container } = mount();
    await screen.findByText("3 bảng · 1 liên kết");
    fireEvent.click(container.querySelector("[data-edge=r1]")!);
    await screen.findByText("2 bản ghi đang giữ liên kết này.");
    fireEvent.click(screen.getByRole("button", { name: "Gỡ liên kết…" }));
    expect(api.deleteRelationshipById).not.toHaveBeenCalled();
    api.listAllRelationships.mockResolvedValue([]);
    api.listFields.mockResolvedValue([]); // the soft-deleted relation field is gone too
    fireEvent.click(await screen.findByRole("button", { name: "Gỡ liên kết" }));
    await waitFor(() => expect(api.deleteRelationshipById).toHaveBeenCalledWith("r1"));
    expect(await screen.findByText("3 bảng · 0 liên kết")).toBeInTheDocument();
  });

  it("search ignores diacritics, marks the match, and an unmatched query offers 'Xoá tìm'", async () => {
    mount();
    await screen.findByText("3 bảng · 1 liên kết");
    const box = screen.getByPlaceholderText("Tìm bảng");
    fireEvent.change(box, { target: { value: "lich" } });
    expect(nodeBtn("crm_events").getAttribute("style")).toMatch(/border-width: 2px/);
    fireEvent.change(box, { target: { value: "zzz" } });
    expect(screen.getByText("Không có bảng nào khớp “zzz”")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Xoá tìm" }));
    expect(screen.queryByText(/Không có bảng nào khớp/)).toBeNull();
  });

  it("⌘F focuses the search only when the focus is inside the map", async () => {
    mount();
    await screen.findByText("3 bảng · 1 liên kết");
    const box = screen.getByPlaceholderText("Tìm bảng");
    const inside = fireEvent.keyDown(nodeBtn("crm_accounts"), { key: "f", metaKey: true });
    expect(inside).toBe(false); // preventDefault called
    expect(box).toHaveFocus();
    const outside = fireEvent.keyDown(box, { key: "f", metaKey: true });
    expect(outside).toBe(true);
  });

  it("an arrow key on a table moves it and announces it in the live region", async () => {
    mount();
    await screen.findByText("3 bảng · 1 liên kết");
    fireEvent.keyDown(nodeBtn("crm_accounts"), { key: "ArrowRight" });
    expect(await screen.findByText("Đã dời Khách hàng")).toBeInTheDocument();
    expect(screen.getByText("Đã dời Khách hàng").closest("[aria-live=polite]")).not.toBeNull();
  });

  it("?bang= narrows to the table and its direct neighbours; 'Xem tất cả' drops the focus", async () => {
    mount({ focusEntity: "crm_accounts" });
    expect(await screen.findByText("2 bảng · 1 liên kết")).toBeInTheDocument();
    expect(document.querySelector("[data-node=crm_notes]")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Xem tất cả" }));
    expect(await screen.findByText("3 bảng · 1 liên kết")).toBeInTheDocument();
  });

  it("states: loading, error with a real retry, no tables", async () => {
    api.listEntities.mockRejectedValueOnce(new Error("down"));
    mount();
    expect(screen.getByText("Đang tải sơ đồ liên kết…", { selector: "span" })).toBeInTheDocument();
    fireEvent.click(await screen.findByRole("button", { name: "Thử lại" }));
    expect(await screen.findByText("3 bảng · 1 liên kết")).toBeInTheDocument();
  });
  it("no tables: says so and links to the table list", async () => {
    api.listEntities.mockResolvedValue([]);
    mount();
    expect(await screen.findByText("Dự án chưa có bảng nào, nên chưa có gì để nối.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Mở danh sách bảng" })).toHaveAttribute("href", "/studio/du-an/northwind/dung/du-lieu");
  });
  it("no links: the map still draws and the panel says there is nothing to pick", async () => {
    api.listAllRelationships.mockResolvedValue([]);
    api.listFields.mockResolvedValue([]);
    mount();
    expect(await screen.findByText("3 bảng · 0 liên kết")).toBeInTheDocument();
    expect(screen.getAllByText("Chưa có liên kết nào giữa các bảng.").length).toBeGreaterThan(0);
  });
});

describe("RelationMap — create (L4b)", () => {
  it("'Liên kết mới' opens the form from the header, the list and a table", async () => {
    mount();
    await screen.findByText("3 bảng · 1 liên kết");
    const open = () => expect(screen.getByRole("button", { name: "Tạo liên kết" })).toBeInTheDocument();
    fireEvent.click(screen.getAllByRole("button", { name: "Liên kết mới" })[0]);
    open();
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    fireEvent.click(nodeBtn("crm_notes"));
    fireEvent.click(screen.getByRole("button", { name: "Liên kết mới từ bảng này" }));
    open();
    expect(screen.queryByRole("dialog")).toBeNull();
  });
  it("dragging one table onto another opens the form with both filled in", async () => {
    mount();
    await screen.findByText("3 bảng · 1 liên kết");
    fireEvent.click(screen.getByTestId("connect"));
    expect(screen.getByLabelText("Tên trường liên kết")).toHaveValue("account");
    expect(screen.getAllByText("Khách hàng").length).toBeGreaterThan(0);
  });
  it("after creating, the map reloads and the new link is selected", async () => {
    api.addField.mockResolvedValue({ name: "account" });
    mount();
    await screen.findByText("3 bảng · 1 liên kết");
    fireEvent.click(screen.getByTestId("connect"));
    const created = { id: "r2", name: "crm_notes_account", parentEntityName: "crm_accounts", childEntityName: "crm_notes", relationshipType: "1-N", childFkColumn: "account_id" };
    api.listAllRelationships.mockResolvedValue([rel, created]);
    api.listFields.mockImplementation(async (name: string) =>
      name === "crm_notes" ? [{ name: "account", label: "Khách hàng", fieldType: "relation", options: { relatedEntity: "crm_accounts" } }] : []);
    fireEvent.click(screen.getByRole("button", { name: "Tạo liên kết" }));
    await waitFor(() => expect(api.getRelationshipImpact).toHaveBeenCalledWith("r2"));
    expect(await screen.findByText("3 bảng · 2 liên kết")).toBeInTheDocument();
  });
});
