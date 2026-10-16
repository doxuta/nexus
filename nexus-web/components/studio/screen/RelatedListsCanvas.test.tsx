import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const api = vi.hoisted(() => ({ listAllRelationships: vi.fn(), setRelationshipViewConfig: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import { ApiError, type AdminRelationship } from "@/lib/api";
import { RelatedListsCanvas } from "./RelatedListsCanvas";

const rel = (id: string, child: string, over: Partial<AdminRelationship> = {}): AdminRelationship =>
  ({ id, name: id, parentEntityName: "t", childEntityName: child, relationshipType: "1-N", ...over }) as AdminRelationship;
const mount = (onSelect = vi.fn()) =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RelatedListsCanvas slug="northwind" entity="t" labelOf={(n) => n.toUpperCase()} selectedId={null} onSelect={onSelect} />
    </QueryClientProvider>,
  );
const written = () => Object.fromEntries(api.setRelationshipViewConfig.mock.calls.map((c) => [c[0], c[1]]));

beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
});

describe("RelatedListsCanvas (specs/075 T081)", () => {
  it("lists only relationships where this table is the parent; child-only links are not listed", async () => {
    api.listAllRelationships.mockResolvedValue([rel("r1", "x"), rel("r2", "t", { parentEntityName: "other" })]);
    mount();
    expect(await screen.findByText("X")).toBeInTheDocument();
    expect(screen.getAllByRole("listitem")).toHaveLength(1);
  });

  it("empty: sentence + Tạo liên kết link to the relation map", async () => {
    api.listAllRelationships.mockResolvedValue([]);
    mount();
    expect(await screen.findByText("Bảng này chưa trỏ tới bảng nào")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Tạo liên kết" }).getAttribute("href")).toContain("so-do?bang=t");
  });

  it("Xuống swaps order of two neighbours, resends the WHOLE document of each and keeps other keys", async () => {
    api.listAllRelationships.mockResolvedValue([
      rel("r1", "a", { viewConfig: { label: "A", columns: ["n"], sort: { field: "id", dir: "desc" }, allowCreate: false, order: 1 } }),
      rel("r2", "b", { viewConfig: { order: 2 } }),
    ]);
    api.setRelationshipViewConfig.mockResolvedValue(null);
    mount();
    await screen.findByText("A");
    await userEvent.click(screen.getAllByRole("button", { name: /^Xuống/ })[0]);
    await waitFor(() => expect(api.setRelationshipViewConfig).toHaveBeenCalledTimes(2));
    expect(written().r1).toEqual({ label: "A", columns: ["n"], sort: { field: "id", dir: "desc" }, allowCreate: false, order: 2 });
    expect(written().r2).toEqual({ order: 1 });
  });

  it("first reorder gives every row an explicit order in display order", async () => {
    api.listAllRelationships.mockResolvedValue([rel("r1", "a"), rel("r2", "b"), rel("r3", "c")]);
    api.setRelationshipViewConfig.mockResolvedValue(null);
    mount();
    await screen.findByText("A");
    await userEvent.click(screen.getAllByRole("button", { name: /^Xuống/ })[0]);
    await waitFor(() => expect(api.setRelationshipViewConfig).toHaveBeenCalledTimes(3));
    expect(written()).toEqual({ r1: { order: 1 }, r2: { order: 0 }, r3: { order: 2 } });
  });

  it("only rows whose order changes are written once all rows have an order", async () => {
    api.listAllRelationships.mockResolvedValue([
      rel("r1", "a", { viewConfig: { order: 0 } }), rel("r2", "b", { viewConfig: { order: 1 } }), rel("r3", "c", { viewConfig: { order: 2 } }),
    ]);
    api.setRelationshipViewConfig.mockResolvedValue(null);
    mount();
    await screen.findByText("A");
    await userEvent.click(screen.getAllByRole("button", { name: /^Xuống/ })[1]);
    await waitFor(() => expect(api.setRelationshipViewConfig).toHaveBeenCalledTimes(2));
    expect(Object.keys(written()).sort()).toEqual(["r2", "r3"]);
  });

  it("one failing write is reported as a partial save", async () => {
    api.listAllRelationships.mockResolvedValue([rel("r1", "a"), rel("r2", "b")]);
    api.setRelationshipViewConfig.mockResolvedValueOnce(null).mockRejectedValueOnce(new ApiError(500, "X", "boom"));
    mount();
    await screen.findByText("A");
    await userEvent.click(screen.getAllByRole("button", { name: /^Xuống/ })[0]);
    expect(await screen.findByRole("alert")).toHaveTextContent(/một phần/i);
  });
});
