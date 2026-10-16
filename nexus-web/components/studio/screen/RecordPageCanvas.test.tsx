import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

const api = vi.hoisted(() => ({ listAllRelationships: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

import type { ApiRecord, FieldMeta } from "@/lib/api";
import { draftFromSections } from "@/lib/layoutDraft";
import { RecordPageCanvas, type RecordPageCanvasProps } from "./RecordPageCanvas";

const F = (name: string, label: string): FieldMeta => ({ name, label, fieldType: "text" }) as FieldMeta;
const fields = [F("a", "Tên"), F("b", "Email"), F("c", "Chức danh")];
const draft = () => draftFromSections([{ title: "Chung", columns: 2, fields: ["a", "b", "c"] }]);
const record = { id: "r1", version: 1, data: { a: "Nguyễn Văn A", b: "x@y.vn", c: "Giám đốc" } } as ApiRecord;

function setup(over: Partial<RecordPageCanvasProps> = {}) {
  const props: RecordPageCanvasProps = {
    slug: "northwind", entity: "t", tableLabel: "Liên hệ", fields, draft: draft(), record, appName: "Phòng khám", selected: null,
    relId: null, onRel: vi.fn(), onSelect: vi.fn(), onDeselect: vi.fn(), onAddSection: vi.fn(), ...over,
  };
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RecordPageCanvas {...props} />
    </QueryClientProvider>,
  );
  return props;
}

beforeEach(() => {
  api.listAllRelationships.mockReset();
  api.listAllRelationships.mockResolvedValue([]);
});

describe("RecordPageCanvas (specs/075 T080)", () => {
  it("draws the customer record home with a detail row from the first section's first fields", () => {
    setup();
    expect(screen.getByTestId("customer-preview-frame")).toBeInTheDocument();
    expect(screen.getAllByText("Nguyễn Văn A").length).toBeGreaterThan(0);
    expect(screen.getAllByText("x@y.vn").length).toBeGreaterThan(0);
  });

  it("no record: title is Hồ sơ {bảng} and a note says nothing is invented", () => {
    setup({ record: null });
    expect(screen.getByText("Hồ sơ Liên hệ")).toBeInTheDocument();
    expect(screen.getByText(/chưa có bản ghi nào/i)).toBeInTheDocument();
  });

  it("field cells are controls; no Bắt buộc nhập / Chỉ đọc anywhere", () => {
    setup();
    expect(screen.getAllByRole("button", { name: /^Trường / })).toHaveLength(3);
    expect(screen.queryByText(/Bắt buộc nhập|Chỉ đọc/)).toBeNull();
  });

  it("built-in tabs are one line, not an empty card", async () => {
    setup();
    await userEvent.click(screen.getByRole("tab", { name: "Lịch sử" }));
    expect(await screen.findByText("Phần này tự có, không cần chỉnh")).toBeInTheDocument();
  });

  it("explains the detail row when no first-section field is placed", () => {
    setup({ draft: draftFromSections([]) });
    expect(screen.getByText(/Đặt trường vào khung đầu/)).toBeInTheDocument();
  });
});
