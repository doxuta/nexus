import { describe, expect, it, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

const api = vi.hoisted(() => ({ listFields: vi.fn(), getLayout: vi.fn(), listRecords: vi.fn() }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), ...api }));

import { editViewOf, screenKey, useScreenEditorData } from "./useScreenEditorData";

const wrap = (qc: QueryClient) => function W({ children }: { children: ReactNode }) {
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
};

beforeEach(() => {
  api.listFields.mockReset().mockResolvedValue([{ name: "a" }]);
  api.getLayout.mockReset().mockResolvedValue({ entityName: "t", views: { edit: { isDefault: true }, detail: {}, list: {} } });
  api.listRecords.mockReset().mockResolvedValue([]);
});

describe("useScreenEditorData", () => {
  it("no record gives record = null (nothing invented) and caches under the screen key", async () => {
    const qc = new QueryClient();
    const { result } = renderHook(() => useScreenEditorData("t"), { wrapper: wrap(qc) });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.record).toBeNull();
    expect(api.listRecords).toHaveBeenCalledWith("t", "limit=5");
    expect(qc.getQueryData(screenKey("t"))).toBeTruthy();
    expect(screenKey("t")).toEqual(["studio", "screen", "t", false, false]);
  });
  it("uses the first record", async () => {
    api.listRecords.mockResolvedValue([{ id: "r1", data: { a: "x" } }]);
    const { result } = renderHook(() => useScreenEditorData("t"), { wrapper: wrap(new QueryClient()) });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.record?.id).toBe("r1");
    expect(result.current.data?.records.map((r) => r.id)).toEqual(["r1"]);
  });
  it("a record read that fails does not fail the editor", async () => {
    api.listRecords.mockRejectedValue(new Error("403"));
    const { result } = renderHook(() => useScreenEditorData("t"), { wrapper: wrap(new QueryClient()) });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.record).toBeNull();
  });
});

describe("editViewOf", () => {
  it("auto-generated view differs from a saved one", () => {
    const auto = editViewOf({ entityName: "t", views: { edit: { isDefault: true, sections: [{ fields: ["a"] }] } } } as never);
    expect(auto).toMatchObject({ isDefault: true, layoutId: null });
    const saved = editViewOf({ entityName: "t", views: { edit: { id: "L1", sections: [{ fields: ["a"] }] } } } as never);
    expect(saved).toMatchObject({ isDefault: false, layoutId: "L1" });
  });
  it("a list view is auto-generated when it has no saved columns, and keeps its id when it has", () => {
    expect(editViewOf({ entityName: "t", views: { list: { isDefault: true, columns: [{ field: "a" }] } } } as never, "list")).toMatchObject({ isDefault: true, layoutId: null });
    expect(editViewOf({ entityName: "t", views: { list: {} } } as never, "list")).toMatchObject({ isDefault: true, layoutId: null });
    expect(editViewOf({ entityName: "t", views: { list: { id: "L2", columns: [{ field: "a" }] } } } as never, "list")).toMatchObject({ isDefault: false, layoutId: "L2" });
  });
});
