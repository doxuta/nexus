// Queue #13 — a field the caller may see but NOT edit must disappear from EVERY write surface,
// not just the single-record form.
//
// Queue #12 put `readOnly` on GET /metadata/fields and taught LayoutForm to render a locked field
// as text. The two BULK write surfaces never read the flag: "Sửa hàng loạt" still offered the
// locked field in its picker, and "Tạo nhiều dòng" still drew a live input for it. The user filled
// it in, pressed save, and the server refused the whole batch with FIELD_FORBIDDEN — because
// CheckWriteFields refuses on the PRESENCE of a locked key, not on its value. Twenty rows lost for
// a field the UI should never have offered.
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { MassEditModal } from "./MassEditModal";
import { BatchCreateForm } from "./BatchCreateForm";
import { listFields, type FieldMeta } from "@/lib/api";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), refresh: vi.fn() }),
  usePathname: () => "/crm_opportunities",
  useSearchParams: () => new URLSearchParams(),
}));

vi.mock("@/lib/metadata/hooks", () => ({
  useEntityLabel: (entity: string) => entity,
}));

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    bulkUpdateRecords: vi.fn(async () => ({ succeeded: 0, failed: 0, results: [] })),
    listFields: vi.fn(async () => LOCKED_FIELDS),
    ensureAuth: vi.fn(),
  };
});

const f = (name: string, label: string, fieldType: string, readOnly?: boolean): FieldMeta =>
  ({ name, label, fieldType, readOnly } as FieldMeta);

const LOCKED_FIELDS = [
  f("company", "Công ty", "text"),
  f("stage", "Giai đoạn", "text", true), // locked by field ACL for this caller's role
];

describe("a locked field never reaches a bulk write surface", () => {
  it("Sửa hàng loạt does not offer the locked field in its picker", () => {
    render(
      <MassEditModal
        entity="crm_opportunities"
        entityLabel="Cơ hội"
        fields={LOCKED_FIELDS}
        ids={["r1", "r2", "r3"]}
        open
        onClose={() => {}}
      />,
    );
    // The picker is an SLDS menus/dropdown (Radix): Enter opens it, each field is a menuitem.
    fireEvent.keyDown(screen.getByRole("button", { name: /Thêm trường cần sửa/ }), { key: "Enter" });
    expect(screen.getByRole("menuitem", { name: /^Công ty/ })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: /^Giai đoạn/ })).toBeNull();
  });

  it("Tạo nhiều dòng draws no column for the locked field", async () => {
    render(<BatchCreateForm entity="crm_opportunities" onDone={() => {}} onCancel={() => {}} />);
    expect(await screen.findByText("Công ty")).toBeInTheDocument();
    expect(screen.queryByText("Giai đoạn")).toBeNull();
  });

  // queue #146: dropping the locked column is right, but when that field is REQUIRED and the
  // engine does not fill it, every row of the batch is refused (RECORD_REQUIRED_LOCKED). Say so
  // before the user fills twenty rows.
  it("Tạo nhiều dòng warns up front when a dropped locked field is required and not engine-filled", async () => {
    vi.mocked(listFields).mockResolvedValueOnce([
      f("company", "Công ty", "text"),
      { ...f("amount", "Giá trị", "currency", true), isRequired: true },
    ]);
    render(<BatchCreateForm entity="crm_opportunities" onDone={() => {}} onCancel={() => {}} />);
    const warning = await screen.findByText(/Chưa tạo được bản ghi/);
    const note = warning.closest(".slds-scoped-notification")!;
    expect(note).toHaveClass("slds-theme_warning");
    expect(note).toHaveTextContent("“Giá trị”");
    expect(screen.getByText("Công ty")).toBeInTheDocument();
  });

  it("Tạo nhiều dòng: a dropped locked field that is optional warns about nothing", async () => {
    render(<BatchCreateForm entity="crm_opportunities" onDone={() => {}} onCancel={() => {}} />);
    expect(await screen.findByText("Công ty")).toBeInTheDocument();
    expect(screen.queryByText(/Chưa tạo được bản ghi/)).toBeNull();
  });
});
