// readForBuilder — the ONE slot through which the screen editor and the relation map read metadata (specs/075 L5a, T194).
// On a delivered project the live read hides what the builder drafted (new fields/tables/links, edited layout), so an
// editor reading it would save over its own draft. Here the GET carries the draft-overlay header (lib/previewMode.ts
// withDraftRead: GET only, per call — writes are never tagged). Not delivered: plain read, no header.
import { getLayout, listAllRelationships, listEntities, listFields } from "@/lib/api";
import { withDraftRead, withLiveRead } from "@/lib/previewMode";

// `live` = the builder asks to see what the customer sees: the GET goes without the draft header (T198 "Xem bản đang chạy").
const via = <T>(delivered: boolean, fn: () => T, live = false): T => (live ? withLiveRead(fn) : delivered ? withDraftRead(fn) : fn());

export const readForBuilder = {
  getLayout: (delivered: boolean, entity: string, live = false) => via(delivered, () => getLayout(entity), live),
  listFields: (delivered: boolean, entity: string, live = false) => via(delivered, () => listFields(entity), live),
  listEntities: (delivered: boolean) => via(delivered, () => listEntities()),
  listAllRelationships: (delivered: boolean, entity?: string) => via(delivered, () => listAllRelationships(entity)),
};
