// specs/073 kh1b2 (T169): the table page keeps the open field in `?truong=` through initialField / onSelectField.
import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { FieldMeta } from "@/lib/api";

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u1", isAdmin: true } }) }));

const listFields = vi.fn();
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  ensureAuth: vi.fn().mockResolvedValue(undefined),
  listEntities: () => Promise.resolve([{ id: "e1", name: "crm_contacts", label: "Liên hệ", tableName: "obj_crm_contacts" }]),
  appsCatalog: () => Promise.resolve([]),
  appNav: () => Promise.resolve({ items: [] }),
  listFields: (...a: unknown[]) => listFields(...a),
}));

import { EntityFieldEditor } from "./EntityFieldEditor";

const phone: FieldMeta = { id: "f-phone", name: "phone", label: "Điện thoại", fieldType: "text", isRequired: false, options: { maxLength: 20 }, description: "" };
const email: FieldMeta = { id: "f-email", name: "email", label: "Email", fieldType: "email", isRequired: true };

function renderWith(props: { initialField?: string | null; onSelectField?: (n: string | null) => void }) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <EntityFieldEditor entityName="crm_contacts" {...props} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  listFields.mockResolvedValue([phone, email]);
});

describe("EntityFieldEditor — initialField / onSelectField", () => {
  it("opens the field the URL names once the list has loaded; the only report is that same code (the route ignores it)", async () => {
    const onSelectField = vi.fn();
    renderWith({ initialField: "phone", onSelectField });
    expect(await screen.findByDisplayValue("Điện thoại")).toBeInTheDocument();
    for (const call of onSelectField.mock.calls) expect(call).toEqual(["phone"]);
  });

  it("an unknown code opens nothing", async () => {
    renderWith({ initialField: "khong_co" });
    await screen.findByRole("button", { name: "Điện thoại" });
    expect(screen.queryByDisplayValue("Điện thoại")).toBeNull();
  });

  it("picking a field reports its code; closing the inspector reports null and does not reopen it", async () => {
    const onSelectField = vi.fn();
    renderWith({ initialField: "email", onSelectField });
    expect(await screen.findByDisplayValue("Email")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(onSelectField).toHaveBeenLastCalledWith(null));
    expect(screen.queryByDisplayValue("Email")).toBeNull();
    fireEvent.click(await screen.findByRole("button", { name: "Điện thoại" }));
    await waitFor(() => expect(onSelectField).toHaveBeenLastCalledWith("phone"));
  });

  it("when the URL drops ?truong= from outside, the panel it had opened closes", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const ui = (f: string | null) => (
      <QueryClientProvider client={qc}>
        <EntityFieldEditor entityName="crm_contacts" initialField={f} />
      </QueryClientProvider>
    );
    const { rerender } = render(ui("email"));
    expect(await screen.findByDisplayValue("Email")).toBeInTheDocument();
    rerender(ui(null));
    await waitFor(() => expect(screen.queryByDisplayValue("Email")).toBeNull());
  });
});
