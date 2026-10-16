// MassEditModal — hợp đồng "không im lặng" (MỘT XƯỞNG, tìm ra 2026-07-30).
//
// "Sửa hàng loạt" từng nhận cử chỉ của người dùng rồi bỏ đi không nói gì, và bỏ theo kiểu KHÓ NGỜ
// nhất — cùng một hành động, hai kết quả trái ngược:
//
//   • chọn một ô CHỮ rồi để trống  → payload bỏ hẳn khoá đó ⇒ KHÔNG ghi gì, không báo gì;
//   • chọn một ô ĐÚNG/SAI, không chạm → `emptyDefault` seed sẵn `false`, và `false` không phải
//     "rỗng" ⇒ payload MANG nó đi và ghi `false` lên toàn bộ N bản ghi.
//
// Người dùng làm đúng một việc ("thêm trường cần sửa") và nhận hai hành vi ngược nhau mà không có
// một chữ giải thích — đúng họ accept-then-swallow của §0b. Bản vá KHÔNG đổi hành vi ghi (đổi là
// chuyện của công tắc "Xoá trống", cần Founder chốt); nó chỉ buộc mỗi trường tự khai nó sẽ làm gì.
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MassEditModal } from "./MassEditModal";
import type { FieldMeta } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  // NOTE: the component calls bulkUpdateRecords. The original mock named massUpdateRecords —
  // a function this modal never calls — so the submit path was never actually stubbed and no
  // test could reach it. Mocking the real one is what lets the payload be asserted below.
  return {
    ...actual,
    bulkUpdateRecords: vi.fn(async () => ({ succeeded: 3, failed: 0, results: [] })),
    ensureAuth: vi.fn(),
  };
});

const f = (name: string, label: string, fieldType: string): FieldMeta =>
  ({ name, label, fieldType } as FieldMeta);

const FIELDS = [f("company", "Công ty", "text"), f("active", "Đang hoạt động", "boolean")];

function open3(fields = FIELDS) {
  return render(
    <MassEditModal
      entity="crm_accounts"
      entityLabel="Khách hàng"
      fields={fields}
      ids={["r1", "r2", "r3"]}
      open
      onClose={() => {}}
    />,
  );
}

// Thêm một trường qua đúng đường người dùng đi: nút "Thêm trường cần sửa" → chọn tên trường.
// Picker là menus/dropdown SLDS (Radix): mở bằng Enter trên nút, mỗi trường là một menuitem.
function addField(label: string) {
  fireEvent.keyDown(screen.getByRole("button", { name: /Thêm trường cần sửa/ }), { key: "Enter" });
  fireEvent.click(screen.getByRole("menuitem", { name: new RegExp("^" + label) }));
}

describe("MassEditModal — mỗi trường phải tự khai nó sẽ làm gì", () => {
  it("ô CHỮ để trống: nói thẳng là sẽ KHÔNG bị thay đổi", () => {
    open3();
    addField("Công ty");
    // Chuỗi bị <strong> cắt làm nhiều node — khớp phần văn bản liền mạch.
    expect(screen.getByText(/Đang để trống/)).toBeInTheDocument();
    // Và nút gửi vẫn xám, kèm lời giải thích thay vì im lặng.
    expect(screen.getByText("Chưa có gì để cập nhật — nhập giá trị vào trường đã chọn.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Cập nhật 3 bản ghi/ })).toBeDisabled();
  });

  it("ô CHỮ có giá trị: nói rõ sẽ ghi lên đúng bao nhiêu bản ghi", () => {
    open3();
    addField("Công ty");
    fireEvent.change(screen.getByPlaceholderText("Nhập…"), { target: { value: "Acme" } });
    expect(screen.getByText(/Sẽ ghi lên/)).toBeInTheDocument();
    expect(screen.queryByText(/Đang để trống/)).toBeNull();
    expect(screen.queryByText("Chưa có gì để cập nhật — nhập giá trị vào trường đã chọn.")).toBeNull();
    expect(screen.getByRole("button", { name: /Cập nhật 3 bản ghi/ })).toBeEnabled();
  });

  it("ô ĐÚNG/SAI chỉ cần CHỌN là đã ghi — phải nói ra, vì đây là chỗ khác hẳn ô chữ", () => {
    open3();
    addField("Đang hoạt động");
    // Chưa chạm gì mà nút đã bật: giá trị `false` seed sẵn ĐANG được gửi thật.
    expect(screen.getByRole("button", { name: /Cập nhật 3 bản ghi/ })).toBeEnabled();
    // Và màn hình phải nói vậy, không để người dùng tự đoán.
    expect(screen.getByText(/Sẽ ghi lên/)).toBeInTheDocument();
    expect(screen.queryByText(/Đang để trống/)).toBeNull();
  });

  it("chưa chọn trường nào thì KHÔNG hiện lời giải thích nút xám (không làm ồn vô cớ)", () => {
    open3();
    expect(screen.queryByText("Chưa có gì để cập nhật — nhập giá trị vào trường đã chọn.")).toBeNull();
  });
});

// ── "Xoá trống" (A-PhầnB) ────────────────────────────────────────────────────
//
// Blanking N records has no undo. It was held back until the audit trail could survive a full
// bulk burst (audit.BufferFor + Dropped(), shipped alongside this) — because the before-images in
// that trail are the only thing that could reconstruct what was erased.
//
// The two properties worth pinning are both about NOT collapsing two intents into one gesture:
// leaving a field blank must still mean "don't touch it", and blanking must be an explicit act
// that costs a second, differently-labelled click.
describe("MassEditModal — công tắc Xoá trống", () => {
  const clearSwitch = (n = 0) => screen.getAllByRole("checkbox")[n];

  it("để trống ≠ xoá trống: trường bỏ trống vẫn KHÔNG bị đụng", async () => {
    const { bulkUpdateRecords } = await import("@/lib/api");
    open3();
    addField("Công ty");
    // No value typed, no switch flipped → nothing to send, so the button stays disabled.
    expect(screen.getByText(/sẽ .*không.* bị thay đổi/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Cập nhật 3 bản ghi/ })).toBeDisabled();
    expect(bulkUpdateRecords).not.toHaveBeenCalled();
  });

  it("bật công tắc → gửi null tường minh, và nói rõ quy mô trước khi bấm", async () => {
    const { bulkUpdateRecords } = await import("@/lib/api");
    vi.mocked(bulkUpdateRecords).mockClear();
    open3();
    addField("Công ty");
    fireEvent.click(clearSwitch());

    expect(screen.getByText(/1 trường × 3 bản ghi sẽ bị xoá trống/)).toBeInTheDocument();

    // First click ARMS (does not send) — the label changes to say what the next click does.
    fireEvent.click(screen.getByRole("button", { name: /Cập nhật \+ xoá trống 3 bản ghi/ }));
    expect(bulkUpdateRecords).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: /Xác nhận xoá trống 3 bản ghi/ }));
    await waitFor(() => expect(bulkUpdateRecords).toHaveBeenCalledTimes(1));
    const [, ids, payload] = vi.mocked(bulkUpdateRecords).mock.calls[0];
    expect(ids).toEqual(["r1", "r2", "r3"]);
    // Explicit null — NOT an omitted key (which the server reads as "leave it alone").
    expect(payload).toHaveProperty("company", null);
  });

  it("tắt công tắc lại thì huỷ luôn trạng thái đã-xác-nhận (không kế thừa sang lựa chọn khác)", () => {
    open3();
    addField("Công ty");
    fireEvent.click(clearSwitch());
    fireEvent.click(screen.getByRole("button", { name: /Cập nhật \+ xoá trống/ }));
    expect(screen.getByRole("button", { name: /Xác nhận xoá trống/ })).toBeInTheDocument();

    fireEvent.click(clearSwitch()); // untick
    expect(screen.queryByRole("button", { name: /Xác nhận xoá trống/ })).toBeNull();
  });
});

// Esc thuộc về popup trong cùng (combobox/lịch): Radix đóng popup bằng preventDefault mà không
// stopPropagation, nên listener Esc của modal phải nhường khi phím đã được xử lý — nếu không, một
// lần Esc vừa đóng danh sách vừa đóng luôn modal và mất hết giá trị đã chọn.
describe("MassEditModal — Esc", () => {
  it("Esc trên danh sách đang mở chỉ đóng danh sách; Esc lần nữa mới đóng modal", async () => {
    const onClose = vi.fn();
    const stage = { name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["new", "won"] } } as unknown as FieldMeta;
    render(
      <MassEditModal entity="crm_accounts" entityLabel="Khách hàng" fields={[stage]} ids={["r1"]} open onClose={onClose} />,
    );
    addField("Giai đoạn");
    const box = screen.getByRole("combobox", { name: /Giai đoạn/ });
    fireEvent.click(box);
    expect(await screen.findByRole("listbox")).toBeInTheDocument();

    fireEvent.keyDown(box, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
    expect(onClose).not.toHaveBeenCalled();

    fireEvent.keyDown(box, { key: "Escape" });
    expect(onClose).toHaveBeenCalledOnce();
  });
});
