// Hoàn tác of a "sẽ xoá khi xuất bản" mark (specs/063-draft-publish S7-9): the mark IS a pending change
// (docType field|entity, op delete), so undoing it is discarding that change — no endpoint of its own.
import { discardDraft, listDraftChanges, ApiError } from "./api";
import { emitDraftsChanged } from "./draftEvents";

/** Discards the pending delete of one field / table. Resolves false when there was none (already undone elsewhere). */
export async function undoDraftDelete(docType: "field" | "entity", docKey: string): Promise<boolean> {
  const { changes } = await listDraftChanges();
  const c = changes.find((x) => x.docType === docType && x.docKey === docKey && x.op === "delete");
  if (!c) return false;
  try {
    await discardDraft(c.id);
  } catch (e) {
    if (!(e instanceof ApiError && e.code === "DRAFT_NOT_FOUND")) throw e; // gone already = same end state
  }
  emitDraftsChanged("undo-delete");
  return true;
}
