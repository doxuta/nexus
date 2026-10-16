// specs/078 lát u7-du-lieu-ban-ghi: one record in the Studio frame (T076, T058a, FR-132, FR-133, FR-135).
// LayoutDetail is the REAL component (storage + 404 are its behaviour); LayoutForm is a stub that exposes its callbacks.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, ensureAuth, getLayout, getRecord, listEntities, listFields, listRecordShares, listUserDirectory, type EntityLayout, type EntityMeta } from "@/lib/api";
import { RecordPage } from "./RecordPage";

const push = vi.fn();
let tokenOk = true;
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => tokenOk }) }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u1", isAdmin: false } }) }));
vi.mock("@/lib/metadata/permissions", () => ({ usePermissions: () => ({ can: () => true, grants: [], isLoading: false, isError: false }) }));
vi.mock("@/lib/navigation-blocker", () => ({ useGuardedRouter: () => ({ push }) }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }), usePathname: () => "/studio/du-an/acme", useSearchParams: () => new URLSearchParams() }));
vi.mock("@/components/layout/LayoutForm", () => ({
  LayoutForm: (p: { recordId?: string; onSaved?: (id: string) => void; onCancel?: () => void }) => (
    <div>
      <p>{p.recordId ? "form-sua" : "form-moi"}</p>
      <button onClick={() => p.onSaved?.("new-1")}>luu</button>
      <button onClick={() => p.onCancel?.()}>huy</button>
    </div>
  ),
}));
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn(),
  getLayout: vi.fn(),
  listFields: vi.fn(),
  getRecord: vi.fn(),
  listEntities: vi.fn(),
  listUserDirectory: vi.fn(),
  listRecordShares: vi.fn(),
  listRecordNotes: vi.fn().mockResolvedValue([]),
  listRecordApprovals: vi.fn().mockResolvedValue([]),
}));

const ENTITIES: EntityMeta[] = [{ name: "khach_hang", label: "Khách hàng", tableName: "obj_khach_hang" }];
const LAYOUT: EntityLayout = { entityName: "khach_hang", views: { edit: {}, list: {}, detail: { sections: [{ title: "Thông tin", fields: ["name"] }] } } };

function mount(props: Partial<React.ComponentProps<typeof RecordPage>> = {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <RecordPage slug="acme" bang="khach_hang" id="r1" {...props} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  window.localStorage.clear();
  tokenOk = true;
  vi.mocked(ensureAuth).mockResolvedValue(undefined);
  vi.mocked(listEntities).mockResolvedValue(ENTITIES);
  vi.mocked(getLayout).mockResolvedValue(LAYOUT);
  vi.mocked(listFields).mockResolvedValue([{ name: "name", label: "Tên", fieldType: "text" }]);
  vi.mocked(getRecord).mockResolvedValue({ id: "r1", version: 1, data: { name: "Công ty Bí Mật" } });
  vi.mocked(listUserDirectory).mockResolvedValue([]);
  vi.mocked(listRecordShares).mockResolvedValue([]);
});

describe("RecordPage", () => {
  it("draws the frame with a caption and no invented app name or logo", async () => {
    mount();
    const frame = await screen.findByTestId("customer-preview-frame");
    expect(frame.textContent).toContain("Đúng như khách thấy");
    expect(frame.querySelector("img")).toBeNull();
    expect(screen.getAllByRole("heading", { level: 2 })[0].textContent).toBe("Bản ghi Khách hàng");
    expect(screen.getByRole("link", { name: /← Khách hàng/ })).toBeTruthy();
  });

  it("viewing a record writes nothing to localStorage (the browser is shared between customers)", async () => {
    const spy = vi.spyOn(Storage.prototype, "setItem");
    mount();
    await screen.findAllByText("Công ty Bí Mật");
    expect(spy.mock.calls.map((c) => String(c[0])).filter((k) => k.startsWith("nexus.recent-records.v1:"))).toEqual([]);
  });

  it("edit and clone buttons go to ?sua=1 and moi?cloneFrom=", async () => {
    mount();
    await screen.findAllByText("Công ty Bí Mật");
    await userEvent.click(screen.getByRole("button", { name: /^Sửa/ }));
    expect(push).toHaveBeenLastCalledWith("/studio/du-an/acme/van-hanh/du-lieu/khach_hang/r1?sua=1");
    await userEvent.click(screen.getByRole("button", { name: "Nhân bản" }));
    expect(push).toHaveBeenLastCalledWith("/studio/du-an/acme/van-hanh/du-lieu/khach_hang/moi?cloneFrom=r1");
  });

  it("the form: saved goes to the new record, cancel goes to the table list", async () => {
    mount({ id: undefined });
    expect(await screen.findByText("form-moi")).toBeTruthy();
    expect(screen.getAllByRole("heading", { level: 2 })[0].textContent).toBe("Bản ghi mới");
    await userEvent.click(screen.getByText("luu"));
    expect(push).toHaveBeenLastCalledWith("/studio/du-an/acme/van-hanh/du-lieu/khach_hang/new-1");
    await userEvent.click(screen.getByText("huy"));
    expect(push).toHaveBeenLastCalledWith("/studio/du-an/acme/van-hanh/du-lieu/khach_hang");
  });

  it("?sua=1 opens the form in edit mode", async () => {
    mount({ edit: true });
    expect(await screen.findByText("form-sua")).toBeTruthy();
  });

  it("404 RECORD_NOT_FOUND says the record may be in the recycle bin", async () => {
    vi.mocked(getRecord).mockRejectedValue(new ApiError(404, "RECORD_NOT_FOUND", "record not found"));
    mount();
    expect(await screen.findByText(/Không tìm thấy bản ghi, có thể đã chuyển vào thùng rác/)).toBeTruthy();
    expect(screen.queryByText("record not found")).toBeNull();
  });

  it("a table that is not in the project's list is 'not found' and no data call is made (FR-133)", async () => {
    mount({ bang: "admin" });
    expect(await screen.findByText("Không tìm thấy bảng này")).toBeTruthy();
    expect(screen.getByRole("link", { name: /Tất cả bảng/ })).toBeTruthy();
    expect(getRecord).not.toHaveBeenCalled();
    expect(getLayout).not.toHaveBeenCalled();
  });

  it("while the table list is loading: spinner, no data call", async () => {
    vi.mocked(listEntities).mockReturnValue(new Promise(() => {}));
    mount();
    await waitFor(() => expect(listEntities).toHaveBeenCalled());
    expect(getRecord).not.toHaveBeenCalled();
    expect(screen.queryByTestId("customer-preview-frame")).toBeNull();
  });

  it("table list fails: sentence + retry, no data call", async () => {
    vi.mocked(listEntities).mockRejectedValue(new ApiError(500, "INTERNAL", "boom"));
    mount();
    expect(await screen.findByRole("button", { name: "Thử lại" })).toBeTruthy();
    expect(getRecord).not.toHaveBeenCalled();
  });

  it("isolation (FR-135): the token does not belong to this project => no fetch at all", async () => {
    tokenOk = false;
    mount();
    await new Promise((r) => setTimeout(r, 50));
    expect(listEntities).not.toHaveBeenCalled();
    expect(getRecord).not.toHaveBeenCalled();
  });
});
