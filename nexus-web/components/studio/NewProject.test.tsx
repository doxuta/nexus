// specs/073 tao1 T520, T525: Dự án mới on its own page — form, typed refusals, show-once password, nothing secret left behind.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("./ContextGate", () => ({ ContextGate: (p: { children: ReactNode }) => <>{p.children}</> }));
vi.mock("@/lib/studioContext", () => ({ homeSlug: () => "northwind" }));
const api = vi.hoisted(() => ({
  createProject: vi.fn(), listBuiltinVerticals: vi.fn(), enterWorkspace: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

import { ApiError } from "@/lib/api";
import { NewProject } from "./NewProject";

function wrap() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={qc}><NewProject /></QueryClientProvider>);
  return qc;
}
async function fillAndSubmit(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText(/Tên dự án/), "Phòng khám An Tâm");
  await user.type(screen.getByLabelText(/Email chủ khách/), "chu@antam.vn");
  await user.click(screen.getByRole("button", { name: "Tạo dự án" }));
}
const created = (over = {}) => ({ slug: "phong-kham-an-tam", name: "Phòng khám An Tâm", adminEmail: "chu@antam.vn", password: "S3cret-once", start: { mode: "blank", status: "done" }, ...over });

beforeEach(() => {
  vi.clearAllMocks();
  sessionStorage.clear();
  localStorage.clear();
  api.listBuiltinVerticals.mockResolvedValue([{ slug: "clinic", manifest: { name: "Phòng khám", version: "1" }, entityCount: 7 }]);
});

describe("Dự án mới", () => {
  it("is a real form: slug follows the name, 'Tả bằng lời' is hidden while the assistant cannot read a brief, the pack list is real data", async () => {
    const user = userEvent.setup();
    wrap();
    await user.type(screen.getByLabelText(/Tên dự án/), "Phòng khám An Tâm");
    expect(screen.getByLabelText(/Mã khách/)).toHaveValue("phong-kham-an-tam");
    expect(screen.queryByText("Tả bằng lời")).toBeNull();
    await user.click(screen.getByLabelText(/Gói mẫu theo ngành/));
    expect(await screen.findByText(/7 thực thể/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("Ngành is a combobox: typing offers real pack names, free text is still sent", async () => {
    const user = userEvent.setup();
    wrap();
    await screen.findByLabelText(/Gói mẫu theo ngành/);
    const box = screen.getByRole("combobox", { name: /Ngành/ });
    await user.type(box, "phong");
    await user.click(await screen.findByRole("option", { name: /Phòng khám/ }));
    expect(box).toHaveValue("Phòng khám");
    await user.clear(box);
    await user.type(box, "Giáo dục");
    expect(box).toHaveValue("Giáo dục");
    await user.type(screen.getByLabelText(/Tên dự án/), "X");
    await user.type(screen.getByLabelText(/Email chủ khách/), "a@b.vn");
    api.createProject.mockResolvedValue(created());
    await user.click(screen.getByRole("button", { name: "Tạo dự án" }));
    await waitFor(() => expect(api.createProject).toHaveBeenCalledWith(expect.objectContaining({ industry: "Giáo dục" })));
  });

  it("sends the chosen pack, shows the password once BEFORE the open button, and keeps it out of every store", async () => {
    const user = userEvent.setup();
    api.createProject.mockResolvedValue(created());
    const qc = wrap();
    await user.click(screen.getByLabelText(/Gói mẫu theo ngành/));
    await screen.findByText(/7 thực thể/);
    await fillAndSubmit(user);
    expect(api.createProject).toHaveBeenCalledWith(expect.objectContaining({ slug: "phong-kham-an-tam", generatePassword: true, start: { mode: "pack", pack: "clinic" } }));
    expect(await screen.findByTestId("tenant-password")).toHaveTextContent("S3cret-once");
    expect(screen.queryByRole("button", { name: "Mở dự án" })).toBeNull();
    for (const store of [sessionStorage, localStorage]) expect(JSON.stringify({ ...store })).not.toContain("S3cret-once");
    expect(JSON.stringify(window.history.state)).not.toContain("S3cret-once");
    expect(window.location.href).not.toContain("S3cret-once");
    expect(JSON.stringify(qc.getMutationCache().getAll().map((m) => m.state))).not.toContain("S3cret-once");
    expect(JSON.stringify(qc.getQueryCache().getAll().map((q) => q.state.data))).not.toContain("S3cret-once");
    await user.click(screen.getByRole("checkbox"));
    await user.click(screen.getByRole("button", { name: "Xong" }));
    expect(screen.queryByTestId("tenant-password")).toBeNull();
    await user.click(await screen.findByRole("button", { name: "Mở dự án" }));
    expect(api.enterWorkspace).toHaveBeenCalledWith("phong-kham-an-tam");
  });

  it("409 lands under Mã khách; 404 builtin shows a notification", async () => {
    const user = userEvent.setup();
    api.createProject.mockRejectedValueOnce(new ApiError(409, "TENANT_ALREADY_EXISTS", "trùng"));
    wrap();
    await fillAndSubmit(user);
    expect(await screen.findByText(/đã có workspace khác dùng/)).toBeInTheDocument();
    api.createProject.mockRejectedValueOnce(new ApiError(404, "VERTICAL_BUILTIN_NOT_FOUND", "x"));
    await user.click(screen.getByRole("button", { name: "Tạo dự án" }));
    expect(await screen.findByText(/không còn trong Nexus/)).toBeInTheDocument();
  });

  it("a failed starter is said plainly; a hand-typed password never reaches the reveal", async () => {
    const user = userEvent.setup();
    api.createProject.mockResolvedValue(created({ password: null, start: { mode: "pack", status: "failed", error: { code: "E", message: "gói lỗi" } } }));
    wrap();
    await user.click(screen.getByLabelText(/Tự sinh mật khẩu/));
    const pw = screen.getByLabelText(/Mật khẩu đặt tay/);
    expect(pw).toHaveAttribute("type", "password");
    expect(pw).toHaveAttribute("autocomplete", "new-password");
    await user.type(pw, "Mat-khau-dai-1");
    await fillAndSubmit(user);
    expect(await screen.findByText(/gói lỗi/)).toBeInTheDocument();
    expect(screen.queryByTestId("tenant-password")).toBeNull();
    await waitFor(() => expect(screen.getByRole("button", { name: "Mở dự án" })).toBeInTheDocument());
  });
});
