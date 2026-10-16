// specs/079 f7-sao-luu-khoa-ghi T103: lock/unlock the customer's writes, confirmed in place.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api";

vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), setTenantMaintenance: vi.fn(), clearTenantMaintenance: vi.fn() }));
import { clearTenantMaintenance, setTenantMaintenance } from "@/lib/api";
import { MaintenanceControl } from "./MaintenanceControl";

const onChanged = vi.fn();
const view = (p: Partial<{ isHome: boolean; until: string | null }> = {}) =>
  render(
    <QueryClientProvider client={new QueryClient()}>
      <MaintenanceControl tenantId="t1" isHome={p.isHome ?? false} until={p.until ?? null} onChanged={onChanged} />
    </QueryClientProvider>,
  );
const future = () => new Date(Date.now() + 600_000).toISOString();

beforeEach(() => {
  onChanged.mockReset();
  vi.mocked(setTenantMaintenance).mockReset().mockResolvedValue({ maintenanceUntil: future() });
  vi.mocked(clearTenantMaintenance).mockReset().mockResolvedValue(undefined);
});

describe("MaintenanceControl", () => {
  it("minutes outside 1..120 are blocked at the field, no confirm", async () => {
    view();
    const box = screen.getByLabelText(/Số phút/);
    await userEvent.clear(box);
    await userEvent.type(box, "121");
    await userEvent.click(screen.getByRole("button", { name: "Khoá ghi" }));
    expect(screen.getByText("Số phút phải từ 1 đến 120.")).toBeInTheDocument();
    expect(setTenantMaintenance).not.toHaveBeenCalled();
  });

  it("lock: confirm names the consequence, safe button focused, POST only after confirm", async () => {
    view();
    await userEvent.click(screen.getByRole("button", { name: "Khoá ghi" }));
    expect(screen.getByText(/xem được nhưng không lưu được trong 30 phút/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Huỷ" })).toHaveFocus();
    expect(setTenantMaintenance).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Khoá ghi 30 phút" }));
    await waitFor(() => expect(setTenantMaintenance).toHaveBeenCalledWith("t1", 30));
    expect(onChanged).toHaveBeenCalled();
  });

  it("locked: shows deadline; unlock needs confirm; 409 BACKUP_IN_PROGRESS shows a sentence", async () => {
    vi.mocked(clearTenantMaintenance).mockRejectedValue(new ApiError(409, "BACKUP_IN_PROGRESS", "x"));
    view({ until: future() });
    expect(screen.getByText(/Đang khoá ghi đến/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Mở khoá ngay" }));
    expect(clearTenantMaintenance).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Mở khoá" }));
    expect(await screen.findByText(/Đang có một bản sao lưu chạy/)).toBeInTheDocument();
  });

  it("home workshop draws nothing", () => {
    const { container } = view({ isHome: true });
    expect(container).toBeEmptyDOMElement();
  });
});
