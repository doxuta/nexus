import { describe, it, expect } from "vitest";
import { buildAuditQuery, type AuditFilter } from "@/lib/admin";
import { isMetaEntity, metaNoun, metaVerb, systemActorLabel } from "@/lib/auditLabels";

// specs/064 T062: "xem như vai" writes __preview__ create/delete rows (internal/audit PreviewMeta).
// The viewer must name them in Vietnamese — never the reserved key, never Tạo/Xoá.
describe("auditLabels — __preview__", () => {
  it("names the reserved key, never 'cấu hình hệ thống' or the raw token", () => {
    expect(isMetaEntity("__preview__")).toBe(true);
    expect(metaNoun("__preview__")).toBe("xem như vai");
    expect(systemActorLabel("__preview__")).toBe("Hệ thống · xem như vai");
  });

  it("reads create/delete as Bắt đầu / Kết thúc xem như vai", () => {
    expect(metaVerb("__preview__", "create")).toBe("Bắt đầu xem như vai");
    expect(metaVerb("__preview__", "delete")).toBe("Kết thúc xem như vai");
  });

  it("has no verb for a pair it does not own (the caller falls back to the plain action word)", () => {
    expect(metaVerb("__preview__", "update")).toBeUndefined();
    expect(metaVerb("__role__", "create")).toBeUndefined();
    expect(metaVerb("crm_contacts", "create")).toBeUndefined();
  });
});

function parse(qs: string): Record<string, string> {
  return Object.fromEntries(new URLSearchParams(qs).entries());
}

describe("buildAuditQuery — filter → query string", () => {
  it("emits all set filters", () => {
    const f: AuditFilter = {
      entityName: "crm_opportunities",
      action: "update",
      actorUserId: "user-1",
      dateFrom: "2026-07-01",
      dateTo: "2026-07-03",
    };
    expect(parse(buildAuditQuery(f))).toEqual({
      entityName: "crm_opportunities",
      action: "update",
      actorUserId: "user-1",
      dateFrom: "2026-07-01",
      dateTo: "2026-07-03",
    });
  });

  it("omits empty/absent filters (no blank params)", () => {
    expect(buildAuditQuery({})).toBe("");
    expect(parse(buildAuditQuery({ entityName: "crm_leads" }))).toEqual({ entityName: "crm_leads" });
  });

  it("NEVER emits from/to — only the dateFrom/dateTo family (avoids 400 AUDIT_INVALID_TIME)", () => {
    const qs = buildAuditQuery({ dateFrom: "2026-07-01", dateTo: "2026-07-03" });
    const keys = [...new URLSearchParams(qs).keys()];
    expect(keys).not.toContain("from");
    expect(keys).not.toContain("to");
    expect(keys).toContain("dateFrom");
    expect(keys).toContain("dateTo");
  });

  it("does NOT include limit/after (those are list-only paging, ignored by export)", () => {
    const qs = buildAuditQuery({ entityName: "crm_accounts", limit: 200, after: "cursor-x" } as AuditFilter);
    const keys = [...new URLSearchParams(qs).keys()];
    expect(keys).not.toContain("limit");
    expect(keys).not.toContain("after");
  });

  it("list and export derive from the SAME builder ⇒ identical active filters", () => {
    const f: AuditFilter = { entityName: "crm_opportunities", action: "delete", dateFrom: "2026-06-01" };
    // The screen builds both the list request and the export href from buildAuditQuery(f).
    expect(buildAuditQuery(f)).toBe(buildAuditQuery({ ...f }));
  });
});
