// specs/078 T066 — in-place mass edit: form, outcome sentences, and the scope gate before EVERY chunk (FR-131).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, ensureAuth: vi.fn().mockResolvedValue(undefined) };
});

import { MassEditBand } from "./MassEditBand";
import type { FieldMeta } from "@/lib/api";

const FIELDS = [
  { id: "f1", name: "city", label: "Thành phố", fieldType: "text" },
  { id: "f2", name: "locked", label: "Khoá", fieldType: "text", readOnly: true },
] as unknown as FieldMeta[];

const ids = (n: number) => Array.from({ length: n }, (_, i) => `id-${i}`);
const ok = (updated: number, failed = 0, results: unknown[] = []) =>
  new Response(JSON.stringify({ data: { updated, failed, results } }), { status: 200, headers: { "Content-Type": "application/json" } });

const originalFetch = global.fetch;
let fetchSpy: ReturnType<typeof vi.fn>;
beforeEach(() => {
  window.localStorage.setItem("nexus_remember", "1");
  window.localStorage.setItem("nexus_token", "t");
  fetchSpy = vi.fn();
  global.fetch = fetchSpy as unknown as typeof fetch;
});
afterEach(() => {
  cleanup();
  global.fetch = originalFetch;
  window.localStorage.clear();
});

async function fillAndApply(n: number) {
  await userEvent.selectOptions(screen.getByLabelText("Trường"), "city");
  await userEvent.type(screen.getByRole("textbox"), "Hà Nội");
  await userEvent.click(screen.getByRole("button", { name: `Áp dụng cho ${n} bản ghi` }));
}

describe("MassEditBand", () => {
  it("focuses the field picker, offers only writable fields, says what the customer will see", async () => {
    render(<MassEditBand entity="crm_contacts" fields={FIELDS} ids={ids(3)} onClose={() => {}} />);
    const sel = screen.getByLabelText("Trường");
    expect(sel).toHaveFocus();
    expect(screen.queryByRole("option", { name: "Khoá" })).toBeNull();
    await userEvent.selectOptions(sel, "city");
    expect(screen.getByText(/Khách thấy giá trị mới ngay ở 3 bản ghi/)).toBeInTheDocument();
  });

  it("Esc closes", async () => {
    const onClose = vi.fn();
    render(<MassEditBand entity="e" fields={FIELDS} ids={ids(2)} onClose={onClose} />);
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalled();
  });

  it("sends bulk-update with the chosen value and reports success", async () => {
    fetchSpy.mockResolvedValue(ok(3));
    const onApplied = vi.fn();
    render(<MassEditBand entity="crm_contacts" fields={FIELDS} ids={ids(3)} onApplied={onApplied} onClose={() => {}} />);
    await fillAndApply(3);
    expect(await screen.findByText("Đã cập nhật 3 bản ghi.")).toBeInTheDocument();
    const [url, init] = fetchSpy.mock.calls[0];
    expect(String(url)).toContain("/crm_contacts/bulk-update");
    expect(JSON.parse(init.body)).toEqual({ ids: ids(3), values: { city: "Hà Nội" } });
    expect(onApplied).toHaveBeenCalledTimes(1);
  });

  it("partial result: ok count, failed count and the first reason", async () => {
    fetchSpy.mockResolvedValue(ok(1, 1, [{ id: "id-1", ok: false, code: "RECORD_DUPLICATE", error: "Trùng email" }]));
    render(<MassEditBand entity="e" fields={FIELDS} ids={ids(2)} onClose={() => {}} />);
    await fillAndApply(2);
    expect(await screen.findByText(/Thành công 1, lỗi 1/)).toBeInTheDocument();
  });

  it("403 is told in place", async () => {
    fetchSpy.mockResolvedValue(new Response(JSON.stringify({ error: { code: "FORBIDDEN", message: "no" } }), { status: 403, headers: { "Content-Type": "application/json" } }));
    render(<MassEditBand entity="e" fields={FIELDS} ids={ids(2)} onClose={() => {}} />);
    await fillAndApply(2);
    expect(await screen.findByText("Tài khoản này không có quyền sửa hàng loạt ở bảng này.")).toBeInTheDocument();
  });

  it("scope gate: scope lost after chunk 1 of 3 => chunks 2 and 3 are never sent", async () => {
    fetchSpy.mockResolvedValue(ok(500));
    let calls = 0;
    // true for chunk 1, false from chunk 2 on
    const guard = { assertScope: () => ++calls <= 1 };
    render(<MassEditBand entity="e" fields={FIELDS} ids={ids(1200)} guard={guard} onClose={() => {}} />);
    await fillAndApply(1200);
    expect(await screen.findByText("Phiên dựng đã đổi nên dừng lại. Đã làm xong 500 trên 1200; phần còn lại chưa động tới.")).toBeInTheDocument();
    expect(fetchSpy).toHaveBeenCalledTimes(1);
  });

  it("aborted signal => nothing is sent", async () => {
    const c = new AbortController();
    c.abort();
    render(<MassEditBand entity="e" fields={FIELDS} ids={ids(3)} guard={{ signal: c.signal }} onClose={() => {}} />);
    await fillAndApply(3);
    await waitFor(() => expect(screen.getByText(/Phiên dựng đã đổi nên dừng lại/)).toBeInTheDocument());
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});
