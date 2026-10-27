// AccessMatrix (specs/074 lát f2-access-matrix, T189 + T455): roles x tables, three tiers per cell, writes that merge
// per role (300ms), read the newest role right before the PUT and never lose a grant outside the changed cell.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, type RoleDetail, type RoleGrant } from "@/lib/api";

const hoisted = vi.hoisted(() => ({ getRole: vi.fn(), update: vi.fn(), delivered: vi.fn() }));
vi.mock("@/lib/admin", async (orig) => ({ ...(await orig<typeof import("@/lib/admin")>()), getRole: hoisted.getRole, updateRolePermissions: hoisted.update }));

vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), getPublishState: () => hoisted.delivered() }));

vi.mock("next/navigation", async (orig) => ({ ...(await orig<typeof import("next/navigation")>()), useRouter: () => ({ push: vi.fn() }), usePathname: () => "/studio/du-an/demo/dung/bao-cao" }));
import { AccessMatrix, accessGhost } from "./AccessMatrix";
import { useProposedChanges } from "@/components/studio/assistant/ProposalOverlay";
import { mkChange, WithPreview } from "@/test/previewProbe";

const g = (entityName: string, action: RoleGrant["action"], accessLevel: RoleGrant["accessLevel"] = "all"): RoleGrant => ({ entityName, action, accessLevel });
const role = (id: string, label: string, actions: RoleGrant[]): RoleDetail => ({ id, name: id, label, tenantId: "t", actions });
const TABLES = [{ name: "invoice", label: "Hoá đơn" }, { name: "visit", label: "Lịch hẹn" }];
const FRONT = role("r1", "Lễ tân", [g("invoice", "view"), g("invoice", "create"), g("invoice", "edit"), g("visit", "view"), g("visit", "delete"), g("blocked", "view", "none")]);
const STAR = role("r2", "Quản lý", [g("*", "view"), g("*", "create"), g("*", "edit")]);
const ODD = role("r3", "Kế toán", [g("invoice", "create")]);
const href = (id: string) => `/studio/du-an/acme/dung/nguoi-dung-quyen/vai/${id}`;
const name = (r: string, tbl: string, lvl: string) => `${r} trên ${tbl}: ${lvl}`;

function setup(roles = [FRONT, STAR, ODD], extra: Partial<React.ComponentProps<typeof AccessMatrix>> = {}) {
  return render(<AccessMatrix roles={roles} tables={TABLES} roleHref={href} {...extra} />);
}

beforeEach(() => {
  hoisted.getRole.mockReset();
  hoisted.update.mockReset();
  hoisted.delivered.mockReset();
  hoisted.delivered.mockResolvedValue({ delivered: false });
  hoisted.getRole.mockImplementation(async (id: string) => [FRONT, STAR, ODD].find((r) => r.id === id));
  hoisted.update.mockImplementation(async (id: string, p: { actions: RoleGrant[] }) => ({ ...[FRONT, STAR, ODD].find((r) => r.id === id)!, actions: p.actions }));
});
afterEach(() => vi.useRealTimers());

describe("cells", () => {
  it("every editable cell is a real control in the Tab order with the reading name 'vai trên bảng: mức'", () => {
    setup();
    const c = screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") });
    expect(c).not.toHaveAttribute("tabindex", "-1");
    expect(screen.getByRole("combobox", { name: name("Lễ tân", "Lịch hẹn", "Chỉ xem") })).toBeInTheDocument();
    const options = within(c).getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["Không thấy", "Chỉ xem", "Xem và sửa"]);
    for (const el of screen.getAllByRole("combobox")) expect(el).not.toHaveAttribute("tabindex", "-1");
  });

  it("a table the role does not mention reads Không thấy, an explicit none on another table does not show up here", () => {
    setup([FRONT]);
    expect(screen.getAllByRole("combobox")).toHaveLength(2);
  });

  it("an inherited cell is a link to the role page, not a quick edit", () => {
    setup();
    const link = screen.getByRole("link", { name: name("Quản lý", "Hoá đơn", "theo mọi bảng, mở trang vai") });
    expect(link).toHaveAttribute("href", href("r2"));
    expect(link).toHaveTextContent("theo mọi bảng");
    expect(screen.queryByRole("combobox", { name: /Quản lý trên/ })).toBeNull();
  });

  it("a mixed cell reads Tuỳ chỉnh and opens the role page", () => {
    setup();
    const link = screen.getByRole("link", { name: name("Kế toán", "Hoá đơn", "Tuỳ chỉnh, mở trang vai") });
    expect(link).toHaveAttribute("href", href("r3"));
  });

  it("the role name is a link to its page and the role column is pinned", () => {
    setup();
    const a = screen.getByRole("link", { name: "Lễ tân" });
    expect(a).toHaveAttribute("href", href("r1"));
    expect(a.closest("th")?.className).toContain("sticky");
  });

  it("a cell of a proposed change wears the ghost class", () => {
    setup(undefined, { proposed: new Set(["r1|visit"]) });
    expect(screen.getByRole("combobox", { name: name("Lễ tân", "Lịch hẹn", "Chỉ xem") }).closest("td")?.className).toContain("nx-proposed");
    expect(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }).closest("td")?.className).not.toContain("nx-proposed");
  });

  it("touch: the control is at least 44px high on a coarse pointer", () => {
    setup();
    expect(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }).className).toContain("pointer-coarse:min-h-11");
  });
});

describe("writing", () => {
  it("merges quick changes of one role into ONE write that holds both, built on the newest getRole", async () => {
    // The server copy has a grant the page never saw (another tab added it): it must come out of the PUT as it went in.
    const newest = role("r1", "Lễ tân", [...FRONT.actions, g("extra", "view", "team")]);
    hoisted.getRole.mockResolvedValue(newest);
    setup([FRONT]);
    fireEvent.change(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }), { target: { value: "view" } });
    fireEvent.change(screen.getByRole("combobox", { name: name("Lễ tân", "Lịch hẹn", "Chỉ xem") }), { target: { value: "edit" } });
    expect(hoisted.update).not.toHaveBeenCalled(); // merged, not fired per change
    await waitFor(() => expect(hoisted.update).toHaveBeenCalledTimes(1), { timeout: 2000 });
    expect(hoisted.getRole).toHaveBeenCalledWith("r1");
    const [id, payload] = hoisted.update.mock.calls[0] as [string, { name: string; label: string; actions: RoleGrant[] }];
    expect(id).toBe("r1");
    expect(payload.name).toBe("r1");
    const a = payload.actions;
    expect(a).toContainEqual(g("extra", "view", "team"));
    expect(a).toContainEqual(g("blocked", "view", "none")); // an explicit none of another table survives
    expect(a).toContainEqual(g("visit", "delete")); // delete of the changed table stays
    expect(a).not.toContainEqual(g("invoice", "create"));
    expect(a).not.toContainEqual(g("invoice", "edit"));
    expect(a).toContainEqual(g("invoice", "view"));
    expect(a).toContainEqual(g("visit", "create"));
    expect(a).toContainEqual(g("visit", "edit"));
  });

  it("leaving the page inside the 300ms window still writes the change", async () => {
    setup([FRONT]);
    fireEvent.change(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }), { target: { value: "view" } });
    expect(hoisted.update).not.toHaveBeenCalled();
    cleanup();
    await waitFor(() => expect(hoisted.update).toHaveBeenCalledTimes(1));
  });
  it("different roles are written separately", async () => {
    setup([FRONT, ODD]);
    fireEvent.change(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }), { target: { value: "none" } });
    fireEvent.change(screen.getByRole("combobox", { name: name("Kế toán", "Lịch hẹn", "Không thấy") }), { target: { value: "view" } });
    await waitFor(() => expect(hoisted.update).toHaveBeenCalledTimes(2), { timeout: 2000 });
    expect(hoisted.update.mock.calls.map((c) => c[0]).sort()).toEqual(["r1", "r3"]);
  });

  it("a second change while the first is still being written is applied after it, on a fresh read", async () => {
    let release: (v: RoleDetail) => void = () => {};
    hoisted.update.mockImplementationOnce(() => new Promise<RoleDetail>((res) => { release = res; }));
    setup([FRONT]);
    fireEvent.change(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }), { target: { value: "view" } });
    await waitFor(() => expect(hoisted.update).toHaveBeenCalledTimes(1), { timeout: 2000 });
    fireEvent.change(screen.getByRole("combobox", { name: name("Lễ tân", "Lịch hẹn", "Chỉ xem") }), { target: { value: "none" } });
    await act(async () => { await new Promise((r) => setTimeout(r, 450)); });
    expect(hoisted.update).toHaveBeenCalledTimes(1); // waits for the first
    const afterFirst = role("r1", "Lễ tân", hoisted.update.mock.calls[0][1].actions);
    hoisted.getRole.mockResolvedValue(afterFirst);
    await act(async () => { release(afterFirst); });
    await waitFor(() => expect(hoisted.update).toHaveBeenCalledTimes(2), { timeout: 2000 });
    const second = hoisted.update.mock.calls[1][1].actions as RoleGrant[];
    expect(second).toContainEqual(g("invoice", "view")); // the first change is still there
    expect(second).not.toContainEqual(g("invoice", "edit"));
    expect(second).not.toContainEqual(g("visit", "view"));
  });

  it("says 'Đã lưu' in the polite live region", async () => {
    setup([FRONT]);
    fireEvent.change(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }), { target: { value: "view" } });
    const live = document.querySelector("[aria-live='polite']") as HTMLElement;
    expect(live).toBeTruthy();
    await waitFor(() => expect(live).toHaveTextContent("Đã lưu quyền của vai Lễ tân."), { timeout: 2000 });
  });

  it("the cell shows the new tier at once and keeps it once saved", async () => {
    setup([FRONT]);
    await act(async () => {}); // publish state read on mount

    fireEvent.change(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }), { target: { value: "none" } });
    expect(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Không thấy") })).toHaveValue("none");
    await waitFor(() => expect(hoisted.update).toHaveBeenCalled(), { timeout: 2000 });
    await waitFor(() => expect(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Không thấy") })).toHaveValue("none"));
  });

  it("a 422 pins the message to the cell that was changed and puts the old tier back; other cells stay clean", async () => {
    hoisted.update.mockRejectedValue(new ApiError(422, "VALIDATION_FAILED", "Vai này đang bị khoá"));
    setup([FRONT]);
    fireEvent.change(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }), { target: { value: "none" } });
    const cell = await screen.findByText("Vai này đang bị khoá", {}, { timeout: 2000 });
    expect(cell.closest("td")).toBe(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") }).closest("td"));
    expect(screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") })).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByRole("combobox", { name: name("Lễ tân", "Lịch hẹn", "Chỉ xem") })).not.toHaveAttribute("aria-invalid");
    const live = document.querySelector("[aria-live='polite']") as HTMLElement;
    expect(live).toHaveTextContent("Chưa lưu được quyền của vai Lễ tân");
  });
});

describe("delivered project: 'Áp ngay cho người dùng thật' (lát f2-direct-apply-access, T377)", () => {
  const front = () => screen.getByRole("combobox", { name: name("Lễ tân", "Hoá đơn", "Xem và sửa") });

  it("asks in the page before the PUT; confirm writes, cancel writes nothing and the cell stays", async () => {
    hoisted.delivered.mockResolvedValue({ delivered: true });
    setup([FRONT]);
    expect(await screen.findByText("Áp ngay cho người dùng thật")).toBeInTheDocument(); // the warn label next to the matrix
    fireEvent.change(front(), { target: { value: "view" } });
    expect(await screen.findByText(/Áp ngay cho người dùng thật: Lễ tân × Hoá đơn: Chỉ xem/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    await act(async () => { await new Promise((r) => setTimeout(r, 450)); });
    expect(hoisted.update).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    await act(async () => { await new Promise((r) => setTimeout(r, 450)); });
    expect(hoisted.update).not.toHaveBeenCalled();
    expect(front()).toHaveValue("edit");

    fireEvent.change(front(), { target: { value: "view" } });
    fireEvent.click(await screen.findByRole("button", { name: "Áp ngay" }));
    await waitFor(() => expect(hoisted.update).toHaveBeenCalledTimes(1), { timeout: 2000 });
  });

  it("not delivered: no label, no question, the write goes out", async () => {
    setup([FRONT]);
    await act(async () => {});
    expect(screen.queryByText("Áp ngay cho người dùng thật")).toBeNull();
    fireEvent.change(front(), { target: { value: "view" } });
    expect(screen.queryByRole("button", { name: "Áp ngay" })).toBeNull();
    await waitFor(() => expect(hoisted.update).toHaveBeenCalledTimes(1), { timeout: 2000 });
  });

  it("Xem như: button per role only when the engine can preview; click calls viewAs.open with that role, same tab", async () => {
    const open = vi.fn().mockResolvedValue(undefined);
    const winOpen = vi.spyOn(window, "open");
    const { unmount } = setup([FRONT, ODD]);
    expect(screen.queryByRole("button", { name: /Xem như/ })).toBeNull();
    unmount();
    setup([FRONT, ODD], { viewAs: { appLabel: "Phòng khám", open } });
    expect(screen.getAllByRole("button", { name: /^Xem như/ })).toHaveLength(2);
    fireEvent.click(screen.getByRole("button", { name: "Xem như Lễ tân" }));
    await waitFor(() => expect(open).toHaveBeenCalledWith({ id: "r1", name: "r1", label: "Lễ tân" }));
    expect(winOpen).not.toHaveBeenCalled();
  });
});

// issue #2: an access card (propose_access) is a "role" change — docKey = role id, before/after = { grants, labels } of the
// tables it touches. The page turns the previewed card into `proposed` cells and ghost columns (accessGhost), as here.
describe("assistant preview (an access card)", () => {
  const card = (before: RoleGrant[], after: RoleGrant[], labels: Record<string, string>) =>
    mkChange("role", "update", { docKey: "r3", target: { name: "r3", entity: Object.keys(labels)[0] }, before: { grants: before, labels }, after: { grants: after, labels } });
  // `tables` is what the page draws (?app= / ?bang= may leave some out), `built` every table of the project.
  function Page({ roles, tables, built = tables }: { roles: RoleDetail[]; tables: { name: string; label: string }[]; built?: { name: string; label: string }[] }) {
    const ghost = accessGhost(useProposedChanges("role")[0] ?? null, roles, built, tables);
    return <AccessMatrix roles={roles} tables={[...tables, ...ghost.extra]} proposed={ghost.proposed} roleHref={href} />;
  }
  const renderWith = (c: ReturnType<typeof mkChange> | null, tables = TABLES) =>
    render(<WithPreview change={c}><Page roles={[FRONT, ODD]} tables={tables} built={TABLES} /></WithPreview>);
  const onVisit = card([g("invoice", "create")], [g("invoice", "create"), g("visit", "view"), g("visit", "create"), g("visit", "edit", "owner")], { visit: "Lịch hẹn" });

  it("tints the card's cell, says the level it leads to and the one it replaces; other cells stay plain", async () => {
    renderWith(onVisit);
    const td = (await screen.findByText("Xem và sửa · Đề xuất")).closest("td")!;
    expect(td.className).toContain("nx-proposed");
    expect(td.textContent).toContain("Trước: Không thấy");
    expect(td.getAttribute("data-proposal-key")).toBe("role-update");
    expect(within(td).getByRole("combobox", { name: name("Kế toán", "Lịch hẹn", "Không thấy") })).toBeInTheDocument();
    expect(document.querySelectorAll(".nx-proposed")).toHaveLength(1);
  });

  it("brings the first ghost cell into view (a wide matrix scrolls sideways)", async () => {
    const seen: Element[] = [];
    const spy = vi.spyOn(HTMLElement.prototype, "scrollIntoView").mockImplementation(function (this: HTMLElement) { seen.push(this); });
    renderWith(onVisit);
    const td = (await screen.findByText("Xem và sửa · Đề xuất")).closest("td")!;
    await waitFor(() => expect(seen).toContain(td));
    spy.mockRestore();
  });

  it("the builder can still set the ghosted cell by hand", async () => {
    renderWith(onVisit);
    await screen.findByText("Xem và sửa · Đề xuất");
    fireEvent.change(screen.getByRole("combobox", { name: name("Kế toán", "Lịch hẹn", "Không thấy") }), { target: { value: "view" } });
    await waitFor(() => expect(hoisted.update).toHaveBeenCalledTimes(1), { timeout: 2000 });
    expect(hoisted.update.mock.calls[0][1].actions).toEqual(expect.arrayContaining([g("invoice", "create"), g("visit", "view")]));
  });

  it("a table the card names that is not built yet is a read-only ghost column", async () => {
    renderWith(card([], [g("tickets", "view"), g("tickets", "create"), g("tickets", "edit", "owner")], { tickets: "Phiếu hỗ trợ" }));
    const head = (await screen.findByRole("columnheader", { name: /Phiếu hỗ trợ/ }));
    expect(head.className).toContain("nx-proposed");
    expect(head.textContent).toContain("Đề xuất");
    const td = screen.getByText("Xem và sửa · Đề xuất").closest("td")!;
    expect(within(td).queryByRole("combobox")).toBeNull();
    expect(screen.queryByRole("combobox", { name: /Phiếu hỗ trợ/ })).toBeNull();
  });

  // issue #23: the page filtered to an app that leaves the card's (built) table out still draws its ghost cell, on the
  // table's real, editable column.
  it("a built table outside the ?app= filter gets its column, so the ghost cell is drawn", async () => {
    renderWith(onVisit, [TABLES[0]]);
    const td = (await screen.findByText("Xem và sửa · Đề xuất")).closest("td")!;
    expect(td.className).toContain("nx-proposed");
    expect(within(td).getByRole("combobox", { name: name("Kế toán", "Lịch hẹn", "Không thấy") })).toBeInTheDocument();
    const head = screen.getByRole("columnheader", { name: "Lịch hẹn" });
    expect(head.className).not.toContain("nx-proposed");
    expect(screen.getAllByRole("columnheader")).toHaveLength(3);
  });

  it("Bỏ (clear) removes the ghost and leaves the real grants alone", async () => {
    renderWith(onVisit);
    await screen.findByText("Xem và sửa · Đề xuất");
    fireEvent.click(screen.getByRole("button", { name: "clear-preview" }));
    await waitFor(() => expect(document.querySelectorAll(".nx-proposed")).toHaveLength(0));
    expect(screen.queryByText(/Đề xuất/)).toBeNull();
    expect(hoisted.update).not.toHaveBeenCalled();
  });

  it("no active change: no ghost (regression)", () => {
    renderWith(null);
    expect(document.querySelectorAll(".nx-proposed")).toHaveLength(0);
  });
});
