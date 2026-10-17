// specs/076 ai9 (T171, T172): labels per class, the fail-closed delivery tri-state, and Hoàn tác of draft rows.
import { describe, it, expect, vi, beforeEach } from "vitest";
import * as api from "@/lib/api";
import { ApiError, type DraftChange, type PublishState } from "@/lib/api";
import { cardMode, deliveryOf, planUndo, resolveUndo, sealedVersion, undoProposal, type Delivery } from "./aiApply";
import type { AiDraftItem } from "./aiBuilder";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, listDraftChanges: vi.fn(), discardDraft: vi.fn() };
});

const item = (x: unknown) => x as AiDraftItem;
const STATE = (o: Partial<PublishState> = {}): PublishState => ({
  delivered: true, headVersion: { versionNo: 3, kind: "publish", publishedAt: "", publishedBy: "" }, pendingCount: 0, changesetHash: "h",
  draftableTypes: ["workflow", "report", "field", "layout"], directTypes: ["role"], propagationSeconds: 5, ...o,
});
const delivered = (o?: Partial<PublishState>): Delivery => ({ kind: "delivered", state: STATE(o) });
const RULE = item({ kind: "workflow", workflow: { name: "Nhắc hẹn", entityName: "lich_hen", trigger: "after_update", actions: [] }, change: { docType: "workflow", op: "update", label: "Nhắc hẹn", target: { entity: "lich_hen" } } });
const APPROVAL = item({ kind: "approval", approval: { name: "Duyệt", entityName: "x", steps: [] } });
const RECORDS = item({ kind: "records", records: { entity: "lich_hen", rows: [{ a: 1 }] } });
const ENTITY_APP = item({ kind: "entity", entity: { name: "t", label: "T", fields: [], appName: "pk" } });
const ENTITY = item({ kind: "entity", entity: { name: "t", label: "T", fields: [{ name: "a", label: "A", fieldType: "text" }] } });
const APP = item({ kind: "app", app: { name: "pk", label: "PK", navEntities: [] } });
const RELATION = item({ kind: "relationship", relationship: { name: "kh_sp", entityA: "khach_hang", entityB: "san_pham" }, change: { docType: "relationship", op: "create", label: "kh_sp" } });

describe("deliveryOf — fail closed (FR-085, FR-130, SC-021)", () => {
  const api404 = new ApiError(404, "UNKNOWN", "not found");
  const rows: [string, Parameters<typeof deliveryOf>[0], Delivery["kind"]][] = [
    ["200 delivered=false", { data: STATE({ delivered: false }) }, "undelivered"],
    ["200 delivered=true", { data: STATE() }, "delivered"],
    ["404 bare", { error: api404 }, "undelivered"],
    ["404 with a code", { error: new ApiError(404, "PROJECT_GONE", "x") }, "unknown"],
    ["401", { error: new ApiError(401, "UNAUTHORIZED", "x") }, "unknown"],
    ["403", { error: new ApiError(403, "FORBIDDEN", "x") }, "unknown"],
    ["500", { error: new ApiError(500, "UNKNOWN", "x") }, "unknown"],
    ["network", { error: new TypeError("Failed to fetch") }, "unknown"],
    ["timeout", { error: new Error("timeout") }, "unknown"],
    ["body without delivered", { data: { draftableTypes: [] } }, "unknown"],
    ["body without draftableTypes", { data: { delivered: true } }, "unknown"],
    ["first load", { isPending: true }, "loading"],
  ];
  it.each(rows)("%s ⇒ %s", (_n, q, kind) => expect(deliveryOf(q).kind).toBe(kind));

  it("an error wins over a stale 'delivered=false' still held in the cache", () => {
    expect(deliveryOf({ data: STATE({ delivered: false }), error: new ApiError(500, "UNKNOWN", "x") }).kind).toBe("unknown");
  });
});

describe("cardMode — label, confirm, apply-all, footer per class", () => {
  it("undelivered: plain Áp dụng, nothing else", () => {
    expect(cardMode(RULE, { kind: "undelivered" })).toMatchObject({ label: "studio.assistant.card.apply", confirm: false, inApplyAll: true, foot: null, canUndo: false, note: null });
  });
  it("delivered draft class: Áp dụng vào bản nháp + draft footer + undo", () => {
    expect(cardMode(RULE, delivered())).toMatchObject({ label: "studio.assistant.draft.applyDraft", confirm: false, inApplyAll: true, foot: "page.studio.ai.foot.draft", canUndo: true, headVersionNo: 3 });
  });
  it("headVersion null counts as 0", () => {
    expect(cardMode(RULE, delivered({ headVersion: null })).headVersionNo).toBe(0);
  });
  it("direct class: Áp ngay + confirm, not in apply-all, no undo", () => {
    const m = cardMode(item({ kind: "role" }), delivered({ draftableTypes: [], directTypes: ["role"] }));
    expect(m).toMatchObject({ label: "studio.assistant.draft.applyNow", confirm: true, inApplyAll: false, foot: "page.studio.ai.foot.live", canUndo: false });
  });
  it("an access card (propose_access, a role change) is direct: Áp ngay + confirm, out of apply-all, the live footer", () => {
    const access = item({ kind: "access", access: { role: "crm_sales_rep", grants: [{ entityName: "tickets", action: "view", accessLevel: "all" }] },
      change: { docType: "role", docKey: "r1", op: "update", label: "Vai", target: { name: "crm_sales_rep", entity: "tickets" } } });
    expect(cardMode(access, delivered())).toMatchObject({ label: "studio.assistant.draft.applyNow", confirm: true, inApplyAll: false, foot: "page.studio.ai.foot.live", canUndo: false });
    // Access applies at once to real users whether or not the project was ever published (verifier round 1, MEDIUM).
    expect(cardMode(access, { kind: "undelivered" })).toMatchObject({ label: "studio.assistant.draft.applyNow", confirm: true, inApplyAll: false, canUndo: false });
  });
  it("untraced class adds 'không quay lại được'", () => {
    expect(cardMode(APPROVAL, delivered())).toMatchObject({ label: "studio.assistant.draft.applyNow", confirm: true, inApplyAll: false, foot: "studio.assistant.draft.footer.untraced", canUndo: false });
  });
  it("records: confirm, real-data footer, out of apply-all", () => {
    expect(cardMode(RECORDS, delivered(), false)).toMatchObject({ confirm: true, inApplyAll: false, foot: "page.studio.ai.foot.records", canUndo: false });
    // Sample rows are marked and fire no automation (ai10): they apply at once.
    expect(cardMode(RECORDS, delivered())).toMatchObject({ confirm: false, inApplyAll: true, canUndo: false });
  });
  it("table with appName and any app card: Áp ngay + confirm, out of apply-all", () => {
    expect(cardMode(ENTITY_APP, delivered())).toMatchObject({ label: "studio.assistant.draft.applyNow", confirm: true, inApplyAll: false, foot: "page.studio.ai.foot.createdHiddenApp", canUndo: false });
    expect(cardMode(APP, delivered())).toMatchObject({ confirm: true, inApplyAll: false, foot: "page.studio.ai.foot.appLive" });
  });
  it("new table when draftableTypes has field: created-hidden footer", () => {
    expect(cardMode(ENTITY, delivered())).toMatchObject({ label: "studio.assistant.card.apply", confirm: false, foot: "page.studio.ai.foot.createdHidden", canUndo: false });
  });
  it("new N-N relation on a delivered workspace: created hidden like a table, not Áp ngay / live", () => {
    expect(cardMode(RELATION, delivered())).toMatchObject({ label: "studio.assistant.card.apply", confirm: false, inApplyAll: true, foot: "page.studio.ai.foot.createdHidden", canUndo: false });
  });
  it("unknown: as delivered-untraced + note + recheck; never a draft label or undo", () => {
    expect(cardMode(RULE, { kind: "unknown" })).toMatchObject({
      label: "studio.assistant.draft.applyNow", confirm: true, inApplyAll: false, canUndo: false, note: "unknown", recheck: true, foot: "studio.assistant.draft.footer.unknown",
    });
  });
  it("loading: Áp dụng disabled + checking note", () => {
    expect(cardMode(RULE, { kind: "loading" })).toMatchObject({ disabled: true, note: "checking", inApplyAll: false, foot: null, canUndo: false });
  });
  it("mutation guard: 'unknown' must not behave like 'undelivered'", () => {
    expect(cardMode(RULE, { kind: "unknown" }).label).not.toBe(cardMode(RULE, { kind: "undelivered" }).label);
  });
});

const row = (o: Partial<DraftChange>): DraftChange => ({
  id: "r1", docType: "workflow", docKey: "wf-1", label: "Nhắc hẹn", op: "update", entityName: "lich_hen", updatedAt: "2026-10-14T10:00:00Z", updatedBy: "u1",
  conflict: false, changedPaths: [], moreChanged: 0, ...o,
});

describe("resolveUndo (FR-082)", () => {
  it("one NEW matching row ⇒ its id", () => {
    expect(resolveUndo(RULE, [], [row({})])).toEqual({ ok: true, rows: [expect.objectContaining({ id: "r1" })] });
  });
  it("the builder already had a draft of the same document (row in before, updatedAt moved) ⇒ hidden 'seen', never a delete", () => {
    const before = [row({ updatedAt: "2026-10-14T09:00:00Z" })];
    expect(resolveUndo(RULE, before, [row({})])).toEqual({ ok: false, why: "seen" });
  });
  it("a NEW row of the same type that does not match entity/label (another tab) ⇒ hidden", () => {
    expect(resolveUndo(RULE, [], [row({}), row({ id: "r2", label: "Khác", entityName: "khac" })])).toEqual({ ok: false, why: "nomatch" });
  });
  it("more candidates than the change declares ⇒ hidden", () => {
    expect(resolveUndo(RULE, [], [row({}), row({ id: "r2" })])).toEqual({ ok: false, why: "nomatch" });
  });
  it("no row at all ⇒ hidden", () => {
    expect(resolveUndo(RULE, [], [])).toEqual({ ok: false, why: "nomatch" });
  });
  it("a NEW rule: the server words change.label as Luật \"X\", the row carries the bare name ⇒ still matched", () => {
    const created = item({ kind: "workflow", workflow: { name: "Nhắc hẹn", entityName: "lich_hen", trigger: "after_update", actions: [] }, change: { docType: "workflow", op: "create", label: 'Luật "Nhắc hẹn"', target: { entity: "lich_hen", name: "Nhắc hẹn" } } });
    expect(resolveUndo(created, [], [row({ docKey: "wf-new" })])).toEqual({ ok: true, rows: [expect.objectContaining({ id: "r1" })] });
    expect(resolveUndo(created, [], [row({ docKey: "wf-new", label: "Khác" })])).toEqual({ ok: false, why: "nomatch" });
  });
  it("rows of OTHER types are never claimed", () => {
    const r = resolveUndo(RULE, [], [row({}), row({ id: "r9", docType: "report", label: "B", entityName: "z" })]);
    expect(r).toEqual({ ok: true, rows: [expect.objectContaining({ id: "r1" })] });
  });
  it("known docKey: matches that row even when it is not new", () => {
    const it2 = item({ ...RULE, change: { ...RULE.change, docKey: "wf-1" } });
    expect(resolveUndo(it2, [row({ id: "old", docKey: "wf-1", updatedAt: "2026-10-14T08:00:00Z" })], [row({ id: "old", docKey: "wf-1" })])).toEqual({ ok: false, why: "seen" });
  });
});

describe("planUndo / undoProposal", () => {
  const listDraftChanges = vi.mocked(api.listDraftChanges);
  const discardDraft = vi.mocked(api.discardDraft);
  const saved = { status: "done" as const, changeIds: ["r1"], headVersionNo: 3, updatedAt: "2026-10-14T10:00:00Z", stamps: { r1: { at: "2026-10-14T10:00:00Z", by: "u1" } } };
  beforeEach(() => { listDraftChanges.mockReset(); discardDraft.mockReset(); });
  const list = (...changes: DraftChange[]) => listDraftChanges.mockResolvedValue({ changes, changesetHash: "h" });

  it("deletes ONLY the stored ids, never another draft row", async () => {
    list(row({}), row({ id: "other", docType: "report", label: "B" }));
    discardDraft.mockResolvedValue(undefined);
    expect(await undoProposal(saved, {})).toEqual({ status: "undone" });
    expect(discardDraft.mock.calls).toEqual([["r1"]]);
  });
  it("planUndo names the rows that would go", async () => {
    list(row({}));
    expect(await planUndo(saved, {})).toMatchObject({ ok: true, labels: ["Nhắc hẹn"] });
  });
  it("every row of the card is gone (published / discarded elsewhere) ⇒ stale, never 'undone'", async () => {
    list(row({ id: "other" }));
    expect(await planUndo(saved, {})).toEqual({ ok: false, reason: "stale" });
    expect(await undoProposal(saved, {})).toEqual({ status: "stale" });
    list();
    expect(await undoProposal(saved, {})).toEqual({ status: "stale" });
    expect(discardDraft).not.toHaveBeenCalled();
  });
  it("row edited further (updatedAt) ⇒ stale, 0 DELETE", async () => {
    list(row({ updatedAt: "2026-10-14T11:00:00Z" }));
    expect(await undoProposal(saved, {})).toEqual({ status: "stale" });
    expect(discardDraft).not.toHaveBeenCalled();
  });
  it("row edited by somebody else (updatedBy) ⇒ stale, 0 DELETE", async () => {
    list(row({ updatedBy: "u2" }));
    expect(await undoProposal(saved, {})).toEqual({ status: "stale" });
    expect(discardDraft).not.toHaveBeenCalled();
  });
  it("404 DRAFT_NOT_FOUND = already gone ⇒ undone", async () => {
    list(row({}));
    discardDraft.mockRejectedValue(new ApiError(404, "DRAFT_NOT_FOUND", "x"));
    expect(await undoProposal(saved, {})).toEqual({ status: "undone" });
  });
  it("403/5xx stops: error, nothing else deleted", async () => {
    list(row({}), row({ id: "r2" }));
    discardDraft.mockRejectedValue(new ApiError(500, "UNKNOWN", "x"));
    const r = await undoProposal({ ...saved, changeIds: ["r1", "r2"] }, {});
    expect(r.status).toBe("error");
    expect(discardDraft).toHaveBeenCalledTimes(1);
  });
  it("assertScope is asked before each DELETE; false ⇒ 0 DELETE", async () => {
    list(row({}));
    expect((await undoProposal(saved, { assertScope: () => false })).status).toBe("scope");
    expect(discardDraft).not.toHaveBeenCalled();
  });
  it("a failed read of the drafts ⇒ error, 0 DELETE", async () => {
    listDraftChanges.mockRejectedValue(new ApiError(500, "UNKNOWN", "x"));
    expect((await undoProposal(saved, {})).status).toBe("error");
    expect(discardDraft).not.toHaveBeenCalled();
  });
});

describe("sealedVersion", () => {
  const done = { status: "done" as const, changeIds: ["r1"], headVersionNo: 3 };
  it("a later head version seals the card", () => expect(sealedVersion(done, delivered({ headVersion: { versionNo: 4, kind: "publish", publishedAt: "", publishedBy: "" } }))).toBe(4));
  it("same head ⇒ not sealed", () => expect(sealedVersion(done, delivered())).toBeNull());
  it("applied when head was null (0), now v1 ⇒ sealed", () => {
    expect(sealedVersion({ status: "done", changeIds: ["r1"], headVersionNo: 0 }, delivered({ headVersion: { versionNo: 1, kind: "baseline", publishedAt: "", publishedBy: "" } }))).toBe(1);
  });
  it("cards with no stored trace are never sealed; unknown delivery never seals", () => {
    expect(sealedVersion({ status: "done" }, delivered({ headVersion: { versionNo: 9, kind: "publish", publishedAt: "", publishedBy: "" } }))).toBeNull();
    expect(sealedVersion(done, { kind: "unknown" })).toBeNull();
  });
});
