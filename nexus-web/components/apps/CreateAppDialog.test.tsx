// CreateAppDialog on SLDS (modals/base) — specs/038 T039. Pins the old dialog's behaviour: the slug
// follows the label until touched, icon/colour are toggles (aria-pressed), server error codes land on
// the right field, and edit is a FULL-REPLACE PUT that carries isDefault/sortOrder verbatim.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CreateAppDialog } from "./CreateAppDialog";
import { ApiError, createApp, updateApp, type AdminApp } from "@/lib/api";
import { onDraftsChanged } from "@/lib/draftEvents";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual, createApp: vi.fn(), updateApp: vi.fn(),
    listDashboards: vi.fn().mockResolvedValue([{ id: "dash-1", name: "Lịch hôm nay", widgets: [] }]),
    listWorkQueuesAdmin: vi.fn().mockResolvedValue([
      { id: "q-1", label: "Lịch hẹn hôm nay", isActive: true }, { id: "q-2", label: "Hẹn hôm nay", isActive: true },
    ]),
    listRoles: vi.fn().mockResolvedValue([{ id: "role-1", name: "le_tan", label: "Lễ tân" }]),
  };
});
vi.mock("@/lib/apps", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/apps")>();
  return { ...actual, useActiveApp: () => ({ setActiveApp: vi.fn() }) };
});
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const APP: AdminApp = {
  id: "app-1", name: "sales", label: "Bán hàng", icon: "briefcase", color: "ok",
  isDefault: true, sortOrder: 3, description: "Đội sale", navCount: 2,
};

function renderDialog(app: AdminApp | null = null, onOpenChange = vi.fn()) {
  const qc = new QueryClient();
  render(
    <QueryClientProvider client={qc}>
      <CreateAppDialog open onOpenChange={onOpenChange} app={app} />
    </QueryClientProvider>,
  );
  return { dialog: screen.getByRole("dialog"), onOpenChange };
}

beforeEach(() => {
  vi.mocked(createApp).mockReset();
  vi.mocked(updateApp).mockReset();
});

describe("CreateAppDialog", () => {
  it("create: the slug follows the label, icon toggles are stateful button-icons, a taken slug errors on the slug field", async () => {
    vi.mocked(createApp).mockRejectedValue(new ApiError(409, "APP_ALREADY_EXISTS", "exists"));
    const { dialog } = renderDialog();
    expect(within(dialog).getByRole("heading", { name: "Tạo app mới" })).toBeInTheDocument();
    const submit = within(dialog).getByRole("button", { name: "Tạo app" });
    expect(submit).toBeDisabled();

    await userEvent.type(within(dialog).getByLabelText(/Tên app/), "Quản lý Kho");
    expect(within(dialog).getByLabelText("Mã app (dùng trong đường dẫn)")).toHaveValue("quan_ly_kho");

    const pkg = within(dialog).getByRole("button", { name: "package" });
    expect(pkg).toHaveClass("slds-button_icon-border-filled");
    await userEvent.click(pkg);
    expect(pkg).toHaveAttribute("aria-pressed", "true");
    expect(pkg).toHaveClass("slds-is-selected");
    const green = within(dialog).getByRole("button", { name: "Màu Xanh lá" });
    await userEvent.click(green);
    expect(green).toHaveAttribute("aria-pressed", "true");

    await userEvent.click(submit);
    await waitFor(() => expect(createApp).toHaveBeenCalledWith({
      name: "quan_ly_kho", label: "Quản lý Kho", icon: "package", color: "ok",
    }));
    expect(await within(dialog).findByText("Mã app này đã có app khác dùng — chọn mã khác.")).toBeInTheDocument();
  });

  it("an invalid hand-typed slug blocks submit and says why", async () => {
    const { dialog } = renderDialog();
    await userEvent.type(within(dialog).getByLabelText(/Tên app/), "Kho");
    const slug = within(dialog).getByLabelText("Mã app (dùng trong đường dẫn)");
    await userEvent.clear(slug);
    await userEvent.type(slug, "1kho");
    expect(slug).toHaveAttribute("aria-invalid", "true");
    expect(within(dialog).getByRole("button", { name: "Tạo app" })).toBeDisabled();
  });

  it("edit: slug locked, full-replace PUT keeps isDefault/sortOrder, closes on success", async () => {
    vi.mocked(updateApp).mockResolvedValue(APP);
    const { dialog, onOpenChange } = renderDialog(APP);
    expect(within(dialog).getByRole("heading", { name: "Sửa app" })).toBeInTheDocument();
    expect(within(dialog).getByLabelText("Mã app (dùng trong đường dẫn)")).toBeDisabled();
    expect(within(dialog).getByText("Mã app cố định sau khi tạo.")).toBeInTheDocument();

    const name = within(dialog).getByLabelText(/Tên app/);
    await userEvent.clear(name);
    await userEvent.type(name, "Bán hàng miền Bắc");
    await userEvent.click(within(dialog).getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateApp).toHaveBeenCalledWith("app-1", {
      label: "Bán hàng miền Bắc", description: "Đội sale", icon: "briefcase", color: "ok",
      isDefault: true, sortOrder: 3, homeDashboardId: "",
    }));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("edit: picking a home dashboard sends homeDashboardId; create mode has no such picker", async () => {
    vi.mocked(updateApp).mockResolvedValue(APP);
    const { dialog } = renderDialog(APP);
    const picker = within(dialog).getByLabelText("Trang chủ của app");
    await within(dialog).findByRole("option", { name: "Lịch hôm nay" });
    await userEvent.selectOptions(picker, "dash-1");
    await userEvent.click(within(dialog).getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateApp).toHaveBeenCalledWith("app-1", expect.objectContaining({ homeDashboardId: "dash-1" })));
  });

  it("edit: picking home queues and a default role sends only what changed (#483)", async () => {
    vi.mocked(updateApp).mockResolvedValue(APP);
    const { dialog } = renderDialog({ ...APP, homeQueueIds: ["q-1"] });
    await userEvent.click(await within(dialog).findByRole("checkbox", { name: "Hẹn hôm nay" }));
    await userEvent.click(await within(dialog).findByRole("checkbox", { name: "Lễ tân" }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateApp).toHaveBeenCalledWith("app-1", expect.objectContaining({
      homeQueueIds: ["q-1", "q-2"], defaultRoleIds: ["role-1"],
    })));
  });

  it("edit: a checkbox pick that is left alone is NOT sent (omitted key = keep)", async () => {
    vi.mocked(updateApp).mockResolvedValue(APP);
    const { dialog } = renderDialog({ ...APP, homeQueueIds: ["q-1"] });
    await within(dialog).findByRole("checkbox", { name: "Hẹn hôm nay" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Lưu thay đổi" }));
    await waitFor(() => expect(updateApp).toHaveBeenCalled());
    const sent = vi.mocked(updateApp).mock.calls[0][1];
    expect(sent).not.toHaveProperty("homeQueueIds");
    expect(sent).not.toHaveProperty("defaultRoleIds");
  });

  it("create: no home-page picker (a new app has nothing to show yet)", () => {
    const { dialog } = renderDialog();
    expect(within(dialog).queryByLabelText("Trang chủ của app")).toBeNull();
  });
});

describe("CreateAppDialog from the customer face (kh6)", () => {
  it("lands on the App page of the session's project, with the new app named: no 404 on the old route", async () => {
    vi.mocked(createApp).mockResolvedValue({ ...APP, name: "kho", label: "Kho" });
    const hrefSet = vi.fn();
    const original = window.location;
    Object.defineProperty(window, "location", { configurable: true, value: { ...original, set href(v: string) { hrefSet(v); } } });
    try {
      const { dialog } = renderDialog();
      await userEvent.type(within(dialog).getByLabelText(/Tên app/), "Kho");
      await userEvent.click(within(dialog).getByRole("button", { name: "Tạo app" }));
      await waitFor(() => expect(hrefSet).toHaveBeenCalledWith("/studio/du-an/northwind/dung/man-hinh/app?app=kho"));
    } finally {
      Object.defineProperty(window, "location", { configurable: true, value: original });
    }
  });
});

describe("CreateAppDialog inside a Studio project (kh2)", () => {
  it("with onCreated it stays on the page: no redirect to the old /studio/apps route, no lens write", async () => {
    vi.mocked(createApp).mockResolvedValue({ ...APP, name: "kho", label: "Kho" });
    const hrefSet = vi.fn();
    const original = window.location;
    Object.defineProperty(window, "location", { configurable: true, value: { ...original, set href(v: string) { hrefSet(v); } } });
    const onCreated = vi.fn();
    const qc = new QueryClient();
    render(
      <QueryClientProvider client={qc}>
        <CreateAppDialog open onOpenChange={vi.fn()} onCreated={onCreated} />
      </QueryClientProvider>,
    );
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText(/Tên app/), "Kho");
    await userEvent.click(within(dialog).getByRole("button", { name: "Tạo app" }));
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith("kho"));
    expect(hrefSet).not.toHaveBeenCalled();
    Object.defineProperty(window, "location", { configurable: true, value: original });
  });
});

// #53: on a delivered project an edit (PUT /admin/apps/:id) is an app draft, so the header's pending count must recount once
// the PUT has answered, never before and never on a refusal. A new app applies at once (POST, no draft): nothing to announce.
describe("CreateAppDialog — the drafts bus (#53)", () => {
  it("edit announces once the save has answered, never before, never on a refusal", async () => {
    const heard: string[] = [];
    const off = onDraftsChanged((s) => heard.push(s));
    try {
      let answer!: () => void;
      vi.mocked(updateApp).mockReturnValueOnce(new Promise((r) => { answer = () => r(APP); }));
      const { dialog, onOpenChange } = renderDialog(APP);
      await userEvent.click(within(dialog).getByRole("button", { name: "Lưu thay đổi" }));
      await waitFor(() => expect(updateApp).toHaveBeenCalledTimes(1));
      await new Promise((r) => setTimeout(r, 20));
      expect(heard).toEqual([]);
      answer();
      await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
      expect(heard).toEqual(["app"]);

      vi.mocked(updateApp).mockRejectedValueOnce(new ApiError(422, "APP_LABEL_REQUIRED", "x"));
      await userEvent.click(within(dialog).getByRole("button", { name: "Lưu thay đổi" }));
      await waitFor(() => expect(updateApp).toHaveBeenCalledTimes(2));
      await new Promise((r) => setTimeout(r, 20));
      expect(heard).toEqual(["app"]);
    } finally {
      off();
    }
  });

  it("a create announces nothing (it applies at once)", async () => {
    const heard: string[] = [];
    const off = onDraftsChanged((s) => heard.push(s));
    try {
      vi.mocked(createApp).mockResolvedValue({ ...APP, name: "kho", label: "Kho" });
      const onCreated = vi.fn();
      render(
        <QueryClientProvider client={new QueryClient()}>
          <CreateAppDialog open onOpenChange={vi.fn()} onCreated={onCreated} />
        </QueryClientProvider>,
      );
      await userEvent.type(within(screen.getByRole("dialog")).getByLabelText(/Tên app/), "Kho");
      await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Tạo app" }));
      await waitFor(() => expect(onCreated).toHaveBeenCalledWith("kho"));
      expect(heard).toEqual([]);
    } finally {
      off();
    }
  });
});
