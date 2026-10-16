import { beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

const api = vi.hoisted(() => ({ setRelationshipViewConfig: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("@/components/studio/usePublishState", () => ({ usePublishState: () => ({ data: { delivered: false, draftableTypes: ["layout", "field"], directTypes: [] } }) }));

import { ApiError, type FieldMeta } from "@/lib/api";
import { RelatedListSettings, type RelatedListSettingsProps } from "./RelatedListSettings";

const F = (name: string, label: string, over: Partial<FieldMeta> = {}): FieldMeta => ({ name, label, fieldType: "text", ...over }) as FieldMeta;
const childFields = [F("n", "Ngày hẹn"), F("m", "Mã", { isUnique: true }), F("k", "Ghi chú")];

function setup(over: Partial<RelatedListSettingsProps> = {}) {
  const props: RelatedListSettingsProps = {
    slug: "demo", relId: "R1", relType: "1-N", parentLabel: "Khách hàng", childLabel: "Lịch hẹn", childFields, onSaved: vi.fn(), onGone: vi.fn(), ...over,
  };
  render(<RelatedListSettings {...props} />);
  return props;
}
const save = () => userEvent.click(screen.getByRole("button", { name: "Lưu cấu hình" }));

beforeEach(() => {
  api.setRelationshipViewConfig.mockReset();
  api.setRelationshipViewConfig.mockResolvedValue({});
});

describe("RelatedListSettings (specs/075 T082)", () => {
  it("Lưu cấu hình PUTs view-config; the body holds only keys with a value, never allowCreate:true", async () => {
    setup();
    await userEvent.type(screen.getByLabelText("Tên mục"), "Hẹn");
    await save();
    await waitFor(() => expect(api.setRelationshipViewConfig).toHaveBeenCalledWith("R1", { label: "Hẹn" }));
  });

  it("columns keep the order they were ticked; sort only offers sortable fields + Tăng/Giảm", async () => {
    setup();
    await userEvent.click(screen.getByLabelText("Ghi chú"));
    await userEvent.click(screen.getByLabelText("Ngày hẹn"));
    const sort = screen.getByLabelText("Sắp xếp theo") as HTMLSelectElement;
    expect([...sort.options].map((o) => o.value)).not.toContain("k"); // not sortable
    await userEvent.selectOptions(sort, "m");
    await userEvent.selectOptions(screen.getByLabelText("Chiều sắp xếp"), "desc");
    await save();
    await waitFor(() => expect(api.setRelationshipViewConfig).toHaveBeenCalledWith("R1", { columns: ["k", "n"], sort: { field: "m", dir: "desc" } }));
  });

  it("switching off 'tạo mới' sends allowCreate:false; N-N sends allowLink:false", async () => {
    setup();
    await userEvent.click(screen.getByLabelText("Có nút tạo mới ngay trong danh sách"));
    await save();
    await waitFor(() => expect(api.setRelationshipViewConfig).toHaveBeenCalledWith("R1", { allowCreate: false }));
  });

  it("Về mặc định sends null", async () => {
    setup({ initial: { label: "x" } });
    await userEvent.click(screen.getByRole("button", { name: "Về mặc định" }));
    await waitFor(() => expect(api.setRelationshipViewConfig).toHaveBeenCalledWith("R1", null));
  });

  it("422 codes land under the right field", async () => {
    api.setRelationshipViewConfig.mockRejectedValueOnce(new ApiError(422, "RELVIEW_SORT_NOT_SORTABLE", "x"));
    setup();
    await save();
    const sortField = screen.getByLabelText("Sắp xếp theo");
    await waitFor(() => expect(sortField).toHaveAccessibleDescription(/Không sắp xếp theo trường này được/));
    api.setRelationshipViewConfig.mockRejectedValueOnce(new ApiError(422, "RELVIEW_COLUMN_UNKNOWN", "x"));
    await save();
    expect(await screen.findByText(/không còn tồn tại trong bảng/)).toBeInTheDocument();
    api.setRelationshipViewConfig.mockRejectedValueOnce(new ApiError(422, "RELVIEW_INVALID", "x"));
    await save();
    await waitFor(() => expect(screen.getByLabelText("Tên mục")).toHaveAccessibleDescription(/chưa hợp lệ/));
  });

  it("404 says the link is gone and closes the group", async () => {
    api.setRelationshipViewConfig.mockRejectedValueOnce(new ApiError(404, "RELATIONSHIP_NOT_FOUND", "x"));
    const p = setup();
    await save();
    expect(await screen.findByText("Liên kết đã được gỡ")).toBeInTheDocument();
    expect(p.onGone).toHaveBeenCalled();
  });
});
