// listColumns — cột "Chủ sở hữu" (OWNER-DIMENSION).
//
// Bối cảnh: `ownerId` là thứ ACL THỰC SỰ chặn theo (COALESCE(owner_id, created_by)), API trả về
// trên MỌI dòng, nhưng grep `ownerId|owner_id|"Chủ sở hữu"` trong LayoutList/listColumns/
// CellRenderer/KanbanBoard ra ZERO — "ai phụ trách" biến khỏi mọi danh sách, chỉ màn chi tiết mới
// thấy. Ba hợp đồng cần ghim: mặc định TẮT (không đổi list nào đang có), id → TÊN qua đúng danh bạ
// /users/directory (non-admin gọi được), và id không tra ra tên thì hiện ID NGẮN — tuyệt đối không
// bịa tên, không để ô trống (ô trống đọc thành "không ai phụ trách").
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { buildListGridColumns, OWNER_COLUMN_ID } from "./listColumns";
import { listUserDirectory, type ApiRecord, type DirectoryUser } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, listUserDirectory: vi.fn() };
});

const USERS: DirectoryUser[] = [
  { id: "u1", email: "an@example.com", fullName: "An Nguyễn" },
  { id: "u2", email: "binh@example.com", fullName: "" },
];

const OTHER_ID = "b1551a1a-5b97-4640-91b5-f49d13bb118f";

const record = (ownerId?: string): ApiRecord => ({ id: "r1", data: { title: "Bản ghi" }, version: 1, ownerId });

// Render just the owner column's cell for one record, inside a fresh query client.
const renderOwnerCell = (r: ApiRecord) => {
  const cols = buildListGridColumns([{ field: "title" }], {}, [], { showOwner: true });
  const owner = cols.find((c) => c.id === OWNER_COLUMN_ID);
  if (!owner) throw new Error("owner column missing");
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{owner.cell(r)}</QueryClientProvider>);
};

beforeEach(() => {
  vi.mocked(listUserDirectory).mockReset().mockResolvedValue(USERS);
});

describe("cột Chủ sở hữu", () => {
  it("mặc định KHÔNG có — mọi list đang chạy giữ nguyên bộ cột của layout", () => {
    const cols = buildListGridColumns([{ field: "title" }], {}, []);
    expect(cols.map((c) => c.id)).toEqual(["title"]);
  });

  it("bật showOwner → thêm đúng một cột 'Chủ sở hữu' ở cuối", () => {
    const cols = buildListGridColumns([{ field: "title" }], {}, [], { showOwner: true });
    expect(cols.map((c) => c.id)).toEqual(["title", OWNER_COLUMN_ID]);
    expect(cols[1].header).toBe("Chủ sở hữu");
  });

  it("id tra được trong danh bạ → hiện TÊN, không hiện uuid", async () => {
    renderOwnerCell(record("u1"));
    expect(await screen.findByText("An Nguyễn")).toBeInTheDocument();
    expect(screen.queryByText("u1")).toBeNull();
    // Đúng danh bạ: /users/directory (non-admin gọi được), không phải /admin/users.
    expect(vi.mocked(listUserDirectory)).toHaveBeenCalled();
  });

  it("người dùng chưa đặt họ tên → lùi về email, KHÔNG bịa tên", async () => {
    renderOwnerCell(record("u2"));
    expect(await screen.findByText("binh@example.com")).toBeInTheDocument();
  });

  it("id không tra ra ai (user đã tắt) → ID NGẮN, không bịa tên, không để trống", async () => {
    renderOwnerCell(record(OTHER_ID));
    expect(await screen.findByText(`#${OTHER_ID.slice(0, 8)}`)).toBeInTheDocument();
    expect(screen.queryByText(OTHER_ID)).toBeNull();
    expect(screen.queryByText("An Nguyễn")).toBeNull();
  });

  it("bản ghi không có ownerId (backend cũ) → gạch ngang, và KHÔNG gọi danh bạ", () => {
    renderOwnerCell(record(undefined));
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(vi.mocked(listUserDirectory)).not.toHaveBeenCalled();
  });
});
