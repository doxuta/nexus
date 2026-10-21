// Issue #17: a screen-editor write (Lưu on the form or the list, "Đưa về màn tự sinh") is a pending change on a delivered
// project. It must reach the Studio header's count at once, through the drafts bus the other draft writers use, not on the
// next reload. The header here is fed the way ProjectFrame feeds it (usePublishState → ProjectHeader), on the app's real
// query defaults: with a 2-minute staleTime nothing else re-reads the publish state.
import { beforeEach, describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClientProvider } from "@tanstack/react-query";

const api = vi.hoisted(() => ({
  listFields: vi.fn(), getLayout: vi.fn(), listRecords: vi.fn(), createLayout: vi.fn(), updateLayout: vi.fn(), deleteLayout: vi.fn(),
  getTenantBrandingDoc: vi.fn(), getPublishState: vi.fn(),
}));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));
vi.mock("@/lib/studioContext", async (orig) => ({ ...(await orig<typeof import("@/lib/studioContext")>()), useStudioIdentity: () => ({ matches: () => true }) }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

import type { EntityMeta, FieldMeta } from "@/lib/api";
import { tf } from "@/lib/i18n";
import { makeClient } from "@/lib/query/client";
import { ProjectHeader } from "@/components/studio/ProjectHeader";
import { usePublishState } from "@/components/studio/usePublishState";
import type { ScreenMan } from "./ScreenBar";
import { ScreenEditor } from "./ScreenEditor";

const F = (name: string, label: string): FieldMeta => ({ id: name, name, label, fieldType: "text" }) as FieldMeta;
const entities = [{ name: "t", label: "Liên hệ" }] as EntityMeta[];
const layout = {
  entityName: "t",
  views: {
    edit: { id: "L1", sections: [{ title: "Chung", columns: 2, fields: ["a", "b"] }] },
    detail: {},
    list: { id: "L7", columns: [{ field: "a" }, { field: "b" }] },
  },
};

// The server: every layout write leaves one more draft behind, counted only once the write has answered (a later tick),
// so an announcement sent before the awaited write resolves would recount too early and leave the header stale.
let drafts = 0;
const answered = <T,>(v: T) => new Promise<T>((r) => setTimeout(() => { drafts++; r(v); }, 20));
const wrote = () => answered({ id: "L1" });

function Header() {
  const s = usePublishState("northwind").data;
  return <ProjectHeader slug="northwind" identity={{ name: "Northwind" }} zone="dung" publish={s && { delivered: s.delivered, pendingCount: s.pendingCount, versionNo: s.headVersion?.versionNo }} />;
}
function Host({ man }: { man: ScreenMan }) {
  const [truong, setTruong] = useState<string | null>(null);
  return <ScreenEditor slug="northwind" entities={entities} bang="t" man={man} truong={truong} onTruong={setTruong} onPick={vi.fn()} />;
}
const mount = (man: ScreenMan) =>
  render(
    <QueryClientProvider client={makeClient()}>
      <Header />
      <Host man={man} />
    </QueryClientProvider>,
  );
const pendingLine = (n: number) => tf("studio.publish.status.pending", { n });

beforeEach(() => {
  Object.values(api).forEach((f) => f.mockReset());
  drafts = 0;
  api.listFields.mockResolvedValue([F("a", "Tên"), F("b", "Email")]);
  api.getLayout.mockResolvedValue(layout);
  api.listRecords.mockResolvedValue([]);
  api.getTenantBrandingDoc.mockResolvedValue({ branding: {} });
  api.getPublishState.mockImplementation(async () => ({
    delivered: true, pendingCount: drafts, draftableTypes: ["layout", "field"], directTypes: [], headVersion: { versionNo: 2 },
  }));
  api.createLayout.mockImplementation(wrote);
  api.updateLayout.mockImplementation(wrote);
  api.deleteLayout.mockImplementation(() => answered(undefined));
});

describe("a screen-editor write raises the header's pending count without a reload (#17)", () => {
  it("Lưu on the form", async () => {
    mount("form");
    expect(await screen.findByText(tf("studio.publish.status.live", { n: 2 }))).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("button", { name: /^Trường Email,/ }));
    await userEvent.type(await screen.findByRole("textbox", { name: "Nhãn khách thấy" }), "X");
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    expect(await screen.findByText(pendingLine(1))).toBeInTheDocument();
    expect(api.updateLayout).toHaveBeenCalledTimes(1);
  });

  it("Lưu on the list", async () => {
    mount("danh-sach");
    expect(await screen.findByText(tf("studio.publish.status.live", { n: 2 }))).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("checkbox", { name: "Hiện cột Email" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu thay đổi" }));
    expect(await screen.findByText(pendingLine(1))).toBeInTheDocument();
    expect(api.updateLayout).toHaveBeenCalledTimes(1);
  });

  it.each<[string, ScreenMan]>([["form", "form"], ["list", "danh-sach"]])("'Đưa về màn tự sinh' on the %s", async (_, man) => {
    mount(man);
    expect(await screen.findByText(tf("studio.publish.status.live", { n: 2 }))).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("button", { name: "Thao tác của màn hình" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Đưa màn về màn tự sinh" }));
    await userEvent.click(screen.getByRole("button", { name: "Đưa về màn tự sinh" }));
    expect(await screen.findByText(pendingLine(1))).toBeInTheDocument();
    expect(api.deleteLayout).toHaveBeenCalledTimes(1);
  });
});
