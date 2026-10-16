import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { FieldMeta } from "@/lib/api";
import { draftFromField } from "@/lib/fieldInspector";
import { RelationFilter, relationFilterIncomplete } from "./RelationFilter";

vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  listFields: vi.fn().mockResolvedValue([
    { id: "d1", name: "customer", label: "Khách hàng", fieldType: "relation", options: { relatedEntity: "customer" } },
    { id: "d2", name: "model", label: "Đời máy", fieldType: "relation", options: { relatedEntity: "model" } },
  ]),
}));

const customer: FieldMeta = { id: "1", name: "customer", label: "Khách", fieldType: "relation", options: { relatedEntity: "customer" } };
const device: FieldMeta = { id: "2", name: "device", label: "Thiết bị", fieldType: "relation", options: { relatedEntity: "device" } };

function mount(onChange = vi.fn(), over: Partial<ReturnType<typeof draftFromField>> = {}) {
  const qc = new QueryClient();
  render(
    <QueryClientProvider client={qc}>
      <RelationFilter field={device} fields={[customer, device]} draft={{ ...draftFromField(device), ...over }} onChange={onChange} />
    </QueryClientProvider>,
  );
  return onChange;
}

describe("RelationFilter (#304)", () => {
  it("lists only OTHER relation fields of the entity as the field to filter by", () => {
    mount();
    fireEvent.click(screen.getByRole("combobox", { name: "Lọc theo ô" }));
    expect(screen.getByRole("option", { name: "Khách" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "Thiết bị" })).toBeNull();
  });

  it("once a sibling is chosen, offers only target relations of the same kind", async () => {
    mount(vi.fn(), { filterParent: "customer" });
    const box = await screen.findByRole("combobox", { name: "Chỉ hiện bản ghi có ô" });
    fireEvent.click(box);
    expect(screen.getByRole("option", { name: "Khách hàng" })).toBeTruthy();
    expect(screen.queryByRole("option", { name: "Đời máy" })).toBeNull();
  });

  it("a half-filled pair blocks the save", () => {
    expect(relationFilterIncomplete({ ...draftFromField(device), filterParent: "customer" })).toBe(true);
    expect(relationFilterIncomplete(draftFromField(device))).toBe(false);
  });
});
