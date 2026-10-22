// AppHomePanel (specs/074 lát 27): picks home dashboard / queues / default-for-roles, previews them in the customer
// frame, saves via a FULL-REPLACE PUT, and says what went wrong on 422/409.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { AppHomePanel } from "./AppHomePanel";
import { ApiError, listDashboards, listRoles, listWorkQueuesAdmin, saveAppHome, type AdminApp } from "@/lib/api";
import { onDraftsChanged } from "@/lib/draftEvents";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, listDashboards: vi.fn(), listRoles: vi.fn(), listWorkQueuesAdmin: vi.fn(), saveAppHome: vi.fn() };
});
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));

const APP: AdminApp = {
  id: "a1", name: "nha_khoa", label: "Nha khoa", icon: null, color: "ac", isDefault: false, sortOrder: 2,
  description: "mô tả", navCount: 1,
};

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}><AppHomePanel app={APP} /></QueryClientProvider>);
}

beforeEach(() => {
  vi.mocked(listDashboards).mockReset().mockResolvedValue([{ id: "d1", name: "Tổng quan", description: null, widgets: [] }]);
  vi.mocked(listWorkQueuesAdmin).mockReset().mockResolvedValue([
    { id: "q1", label: "Lịch hẹn hôm nay", isActive: true },
  ] as never);
  vi.mocked(listRoles).mockReset().mockResolvedValue([{ id: "r1", name: "le_tan", label: "Lễ tân" }]);
  vi.mocked(saveAppHome).mockReset().mockResolvedValue(APP);
});

describe("AppHomePanel", () => {
  it("previews the picks and saves only the three home fields", async () => {
    const user = userEvent.setup();
    mount();
    await screen.findByRole("option", { name: "Tổng quan" });
    await user.selectOptions(screen.getByLabelText("Trang tổng quan của app"), "d1");
    await user.click(await screen.findByLabelText("Lịch hẹn hôm nay"));
    await user.click(await screen.findByLabelText("Lễ tân"));
    const preview = screen.getByTestId("app-home-preview");
    expect(preview).toHaveTextContent("Trang tổng quan: Tổng quan");
    expect(preview).toHaveTextContent("Việc: Lịch hẹn hôm nay");
    expect(preview).toHaveTextContent("Vào thẳng app này: Lễ tân");
    await user.click(screen.getByRole("button", { name: "Lưu trang chủ" }));
    await waitFor(() => expect(saveAppHome).toHaveBeenCalledWith(APP, {
      homeDashboardId: "d1", homeQueueIds: ["q1"], defaultRoleIds: ["r1"],
    }));

  });

  it("explains a 422 on a vanished pick and a 409 conflict", async () => {
    const user = userEvent.setup();
    mount();
    await screen.findByLabelText("Lễ tân");
    vi.mocked(saveAppHome).mockRejectedValueOnce(new ApiError(422, "APP_DEFAULT_ROLE_INVALID", "x"));
    await user.click(screen.getByRole("button", { name: "Lưu trang chủ" }));
    expect(await screen.findByText(/không còn tồn tại/)).toBeInTheDocument();
    vi.mocked(saveAppHome).mockRejectedValueOnce(new ApiError(409, "CONFLICT", "x"));
    await user.click(screen.getByRole("button", { name: "Lưu trang chủ" }));
    expect(await screen.findByText(/vừa được người khác sửa/)).toBeInTheDocument();
  });

  // #53 (verifier): on a delivered project the save is PUT /admin/apps/:id, an app draft (DraftAppStore.Update), so the header
  // recounts: once the PUT has answered, never before, never on a refusal.
  it("announces on the drafts bus once the save has answered, never before, never on a refusal", async () => {
    const heard: string[] = [];
    const off = onDraftsChanged((s) => heard.push(s));
    try {
      const user = userEvent.setup();
      let answer!: () => void;
      vi.mocked(saveAppHome).mockReturnValueOnce(new Promise((r) => { answer = () => r(APP); }));
      mount();
      await screen.findByLabelText("Lễ tân");
      await user.click(screen.getByRole("button", { name: "Lưu trang chủ" }));
      await waitFor(() => expect(saveAppHome).toHaveBeenCalledTimes(1));
      await new Promise((r) => setTimeout(r, 20));
      expect(heard).toEqual([]);
      answer();
      await waitFor(() => expect(heard).toEqual(["app"]));
      vi.mocked(saveAppHome).mockRejectedValueOnce(new ApiError(409, "CONFLICT", "x"));
      await waitFor(() => expect(screen.getByRole("button", { name: "Lưu trang chủ" })).toBeEnabled());
      await user.click(screen.getByRole("button", { name: "Lưu trang chủ" }));
      await waitFor(() => expect(saveAppHome).toHaveBeenCalledTimes(2));
      await new Promise((r) => setTimeout(r, 20));
      expect(heard).toEqual(["app"]);
    } finally {
      off();
    }
  });
});
