import { beforeEach, describe, expect, it, vi } from "vitest";

// specs/063 S7-9: Hoàn tác of a "sẽ xoá khi xuất bản" mark = discarding that pending change.
const api = vi.hoisted(() => {
  class ApiError extends Error {
    constructor(public status: number, public code: string, message: string) {
      super(message);
    }
  }
  return { ApiError, listDraftChanges: vi.fn(), discardDraft: vi.fn() };
});
vi.mock("./api", () => api);
const emit = vi.fn();
vi.mock("./draftEvents", () => ({ emitDraftsChanged: (s: string) => emit(s) }));

import { undoDraftDelete } from "./draftUndo";

const change = (id: string, docType: string, docKey: string, op: string) => ({ id, docType, docKey, op, label: "", entityName: "", updatedAt: "", updatedBy: "", conflict: false });

beforeEach(() => {
  api.listDraftChanges.mockReset();
  api.discardDraft.mockReset().mockResolvedValue(undefined);
  emit.mockReset();
});

describe("undoDraftDelete", () => {
  it("discards exactly the delete change of that field and announces it", async () => {
    api.listDraftChanges.mockResolvedValue({
      changes: [change("c1", "field", "f1", "update"), change("c2", "field", "f1", "delete"), change("c3", "entity", "f1", "delete")],
      changesetHash: "h",
    });
    await expect(undoDraftDelete("field", "f1")).resolves.toBe(true);
    expect(api.discardDraft).toHaveBeenCalledTimes(1);
    expect(api.discardDraft).toHaveBeenCalledWith("c2");
    expect(emit).toHaveBeenCalled();
  });

  it("resolves false and discards nothing when the mark is already gone", async () => {
    api.listDraftChanges.mockResolvedValue({ changes: [change("c1", "field", "f1", "update")], changesetHash: "h" });
    await expect(undoDraftDelete("field", "f1")).resolves.toBe(false);
    expect(api.discardDraft).not.toHaveBeenCalled();
  });

  it("DRAFT_NOT_FOUND (undone in another tab) counts as undone; other errors surface", async () => {
    api.listDraftChanges.mockResolvedValue({ changes: [change("c2", "entity", "e1", "delete")], changesetHash: "h" });
    api.discardDraft.mockRejectedValueOnce(new api.ApiError(404, "DRAFT_NOT_FOUND", "x"));
    await expect(undoDraftDelete("entity", "e1")).resolves.toBe(true);
    api.discardDraft.mockRejectedValueOnce(new api.ApiError(500, "PUBLISH_OP_FAILED", "boom"));
    await expect(undoDraftDelete("entity", "e1")).rejects.toThrow("boom");
  });
});
