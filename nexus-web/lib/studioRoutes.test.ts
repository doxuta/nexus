// specs/073 kh1a T141 + T173: the Studio route table, href builder, parser, safeNext and the token gate.
import fs from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import vi from "@/lib/i18n/vi";
import { isPanelKey } from "@/lib/workshopPanels";
import {
  NHA_MOUNT_ALLOWLIST,
  ROUTES,
  currentProjectSlug,
  isMounted,
  parseStudioPath,
  projectTokenMatches,
  routeForPanel,
  calendarsHref,
  safeNext,
  studioHref,
  tabOfRoute,
  viewOfMan,
  ZONE_TABS,
  type RouteId,
} from "@/lib/studioRoutes";

const S = "acme";
const first = `/studio/du-an/${S}/dung/du-lieu`;
const sampleParams = (id: RouteId) => {
  const r = ROUTES.find((x) => x.id === id)!;
  return Object.fromEntries(r.params.map((p) => [p, p === "bang" ? "customers" : id === "bao-tri/sao-luu/khoi-phuc/:id" ? "0b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d" : "ab12"]));
};

describe("route table", () => {
  it("has a label in vi.ts, a ctx, and an aiPanel that is a known panel code (or null for the codes 067 adds)", () => {
    expect(ROUTES.length).toBeGreaterThan(45);
    for (const r of ROUTES) {
      expect(vi[r.labelKey], r.id).toBeTruthy();
      expect(["phien", "nha"]).toContain(r.ctx);
      if (r.aiPanel !== null) expect(isPanelKey(r.aiPanel), `${r.id} -> ${r.aiPanel}`).toBe(true);
    }
  });

  it("route ids are unique", () => {
    expect(new Set(ROUTES.map((r) => r.id)).size).toBe(ROUTES.length);
  });

  it("nha routes never carry the assistant and only mount allow-listed components (FR-024)", () => {
    const nha = ROUTES.filter((r) => r.ctx === "nha");
    expect(nha.map((r) => r.id).sort()).toEqual(["bao-tri/sao-luu", "bao-tri/sao-luu/khoi-phuc/:id", "cai-dat"]);
    for (const r of nha) {
      expect(r.aiPanel).toBeNull();
      if (r.mounts !== null) expect(NHA_MOUNT_ALLOWLIST).toContain(r.mounts);
    }
  });

  it("kh1b2 + kh2 + kh3 mount Dữ liệu (list, table page, diagram), Màn hình and Tự động; a rule stays pending; isMounted reads the flag", () => {
    const mountedIds = ROUTES.filter((r) => r.mounted).map((r) => r.id);
    // kh4 adds Vận hành/Bảo trì (own test); kh3: access, reports list, branding, trial, deliver.
    for (const id of [
      "dung/du-lieu", "dung/du-lieu/:bang", "dung/du-lieu/so-do", "dung/du-lieu/them-tu-goi", "dung/man-hinh", "dung/man-hinh/:bang", "dung/man-hinh/app", "dung/tu-dong", "dung/tu-dong/luat/:id", "dung/tu-dong/luat/moi",
      "dung/tu-dong/duyet", "dung/tu-dong/hang-doi", "dung/tu-dong/cong-nhan", "dung/tu-dong/cong-nhan/:id", "dung/tu-dong/lich-lam-viec",
      "dung/nguoi-dung-quyen", "dung/nguoi-dung-quyen/vai/:id", "dung/nguoi-dung-quyen/quyen-truong", "dung/nguoi-dung-quyen/doi",
      "dung/nguoi-dung-quyen/chia-se", "dung/bao-cao", "dung/bao-cao/tong-quan/:id", "dung/thuong-hieu", "dung/chay-thu", "dung/giao-cho-khach",
    ]) expect(mountedIds, id).toContain(id);
    for (const id of ["dung/bao-cao/:id"]) expect(mountedIds, id).not.toContain(id);
    expect(ROUTES.find((r) => r.id === "dung/du-lieu")!.mounts).toBe("EntityListPage");
    expect(ROUTES.find((r) => r.id === "dung/du-lieu/:bang")!.mounts).toBe("EntityPage");
    expect(ROUTES.find((r) => r.id === "dung/du-lieu/so-do")!.mounts).toBe("RelationMap");
    expect(ROUTES.find((r) => r.id === "dung/tu-dong")!.mounts).toBe("AutomationPage");
    expect(isMounted("dung/du-lieu/:bang")).toBe(true);
    expect(isMounted("dung/man-hinh/app")).toBe(true);
    expect(isMounted("dung/tu-dong/luat/:id")).toBe(true);
    expect(ROUTES.find((r) => r.id === "dung/tu-dong/luat/:id")!.mounts).toBe("RulePage");
  });

  it("a rule has its own page; luat/moi is a page, not a rule id, and carries bang/khi/ten", () => {
    expect(studioHref(S, "dung/tu-dong/luat/:id", { id: "wf-1" }, { app: "ticketing" })).toBe(`/studio/du-an/${S}/dung/tu-dong/luat/wf-1?app=ticketing`);
    const href = studioHref(S, "dung/tu-dong/luat/moi", {}, { bang: "tk_tickets", khi: "after_create", ten: "Nhắc việc mới" });
    expect(href).toBe(`/studio/du-an/${S}/dung/tu-dong/luat/moi?bang=tk_tickets&khi=after_create&ten=${encodeURIComponent("Nhắc việc mới")}`);
    const back = parseStudioPath(href);
    expect(back?.route?.id).toBe("dung/tu-dong/luat/moi");
    expect(back?.query).toEqual({ bang: "tk_tickets", khi: "after_create", ten: "Nhắc việc mới" });
    expect(parseStudioPath(`/studio/du-an/${S}/dung/tu-dong/luat/wf-1`)?.params.id).toBe("wf-1");
    expect(studioHref(S, "dung/tu-dong/luat/moi", {}, { bang: "Bad Name", ten: "a\nb" })).toBe(`/studio/du-an/${S}/dung/tu-dong/luat/moi`);
  });

  it("a table page and the diagram never shadow each other: so-do is a page, not a table", () => {
    expect(parseStudioPath("/studio/du-an/northwind/dung/du-lieu/so-do?bang=crm_accounts")?.route?.id).toBe("dung/du-lieu/so-do");
    expect(parseStudioPath("/studio/du-an/northwind/dung/du-lieu/crm_accounts")?.params.bang).toBe("crm_accounts");
  });

  it("?man= maps to the screen-editor view; unknown values fall to undefined (the page opens the form)", () => {
    expect(viewOfMan("form")).toBe("Form");
    expect(viewOfMan("ho-so")).toBe("Detail");
    expect(viewOfMan("danh-sach")).toBe("List");
    expect(viewOfMan("x")).toBeUndefined();
    expect(viewOfMan(undefined)).toBeUndefined();
  });

  it("the table page accepts only ?truong= and ?moi= (no ?tab= / ?view=, FR-038; moi=1 = just created, 074 lát 1)", () => {
    expect(ROUTES.find((r) => r.id === "dung/du-lieu/:bang")!.query).toEqual(["truong", "moi"]);
  });
});

describe("kh4: Vận hành, Bảo trì mount (T271, T275)", () => {
  const byId = (id: string) => ROUTES.find((r) => r.id === id)!;
  it("mounts the Vận hành and Bảo trì pages that have a component today", () => {
    const want: Record<string, string> = {
      "van-hanh/nguoi-dung": "UserList",
      "van-hanh/nguoi-dung/dang-nhap-mot-lan": "SsoProviders",
      "van-hanh/nguoi-dung/nhat-ky": "AuditLog",
      "van-hanh/du-lieu": "DataTablesPage",
      "van-hanh/du-lieu/:bang": "EntityRecordsPage",
      "van-hanh/du-lieu/:bang/moi": "RecordPage",
      "van-hanh/du-lieu/:bang/tao-nhieu": "OperateBatchCreate",
      "van-hanh/du-lieu/:bang/thung-rac": "OperateRecycleBin",
      "van-hanh/tu-dong": "OperateOverview",
      "van-hanh/tu-dong/lan-chay": "WorkflowRunHistory",
      "van-hanh/tu-dong/viec-nen": "BackgroundJobs",
      "van-hanh/tu-dong/khoa-truy-cap": "ApiKeys",
      "van-hanh/suc-khoe": "HealthPage",
      "van-hanh/tu-dong/khoa-ket-noi": "Credentials",
      "van-hanh/tu-dong/cong-nhan": "InboundLink",
      "bao-tri/sao-luu": "BackupsPage",
      "bao-tri/kham": "DoctorScreen",
      "bao-tri/nang-cap": "PackDiffPanel",
      "bao-tri/nang-cap/:id": "UpgradePage",
      "bao-tri/lich-su": "HistoryPage",
      "bao-tri/lich-su/:id": "VersionPage",
      "cai-dat": "ProjectSettings",
    };
    for (const [id, comp] of Object.entries(want)) {
      expect(byId(id).mounted, id).toBe(true);
      expect(byId(id).mounts, id).toBe(comp);
    }
  });

  it("the restore plan page (nha) is mounted by RestorePage", () => {
    expect(byId("bao-tri/sao-luu/khoi-phuc/:id").mounts).toBe("RestorePage");
  });

  it("Dữ liệu nhận vào is a link to the build page of the same name, not a pending page", () => {
    expect(studioHref(S, "dung/tu-dong/cong-nhan")).toBe(`/studio/du-an/${S}/dung/tu-dong/cong-nhan`);
    expect(isMounted("dung/tu-dong/cong-nhan")).toBe(true);
    expect(ROUTES.find((r) => r.id === "dung/tu-dong/cong-nhan/:id")!.mounts).toBe("HookPage");
    expect(ROUTES.find((r) => r.id === "dung/tu-dong/lich-lam-viec")!.mounts).toBe("CalendarPage");
    expect(isMounted("van-hanh/tu-dong/cong-nhan")).toBe(true);
  });

  it("each zone has four tabs, each tab owns its sub pages, and Cài đặt belongs to no tab", () => {
    expect(ZONE_TABS["van-hanh"].map((x) => x.route)).toEqual([
      "van-hanh/nguoi-dung", "van-hanh/du-lieu", "van-hanh/tu-dong", "van-hanh/suc-khoe",
    ]);
    expect(ZONE_TABS["bao-tri"].map((x) => x.route)).toEqual([
      "bao-tri/lich-su", "bao-tri/sao-luu", "bao-tri/kham", "bao-tri/nang-cap",
    ]);
    expect(tabOfRoute(byId("van-hanh/nguoi-dung/nhat-ky"))).toBe("van-hanh/nguoi-dung");
    expect(tabOfRoute(byId("van-hanh/tu-dong/khoa-truy-cap"))).toBe("van-hanh/tu-dong");
    expect(tabOfRoute(byId("bao-tri/sao-luu/khoi-phuc/:id"))).toBe("bao-tri/sao-luu");
    expect(tabOfRoute(byId("cai-dat"))).toBeNull();
    expect(tabOfRoute(byId("dung/du-lieu"))).toBeNull();
    expect(tabOfRoute(null)).toBeNull();
  });
});

describe("studioHref / parseStudioPath round trip", () => {
  it("every route goes there and back", () => {
    for (const r of ROUTES) {
      const params = sampleParams(r.id);
      const href = studioHref(S, r.id, params);
      expect(href.startsWith(`/studio/du-an/${S}`), r.id).toBe(true);
      const parsed = parseStudioPath(href);
      expect(parsed?.slug, r.id).toBe(S);
      expect(parsed?.route?.id, r.id).toBe(r.id);
      expect(parsed?.params, r.id).toEqual(params);
    }
  });

  it("keeps allowed query keys and drops unknown ones", () => {
    const href = studioHref(S, "dung/du-lieu/:bang", { bang: "customers" }, { truong: "email", tab: "x", q: "y" });
    expect(href).toBe(`/studio/du-an/${S}/dung/du-lieu/customers?truong=email`);
    const parsed = parseStudioPath(`${href}&view=z`);
    expect(parsed?.query).toEqual({ truong: "email" });
  });

  it("a table named app goes to the ?bang= form (static segment wins both ways)", () => {
    const href = studioHref(S, "dung/man-hinh/:bang", { bang: "app" });
    expect(href).toBe(`/studio/du-an/${S}/dung/man-hinh?bang=app`);
    expect(parseStudioPath(href)?.route?.id).toBe("dung/man-hinh");
    expect(parseStudioPath(`/studio/du-an/${S}/dung/man-hinh/app`)?.route?.id).toBe("dung/man-hinh/app");
    expect(parseStudioPath(`/studio/du-an/${S}/dung/man-hinh/customers`)?.route?.id).toBe("dung/man-hinh/:bang");
  });

  it("static segments are resolved before a table name", () => {
    expect(parseStudioPath(`/studio/du-an/${S}/dung/du-lieu/so-do`)?.route?.id).toBe("dung/du-lieu/so-do");
    expect(parseStudioPath(`/studio/du-an/${S}/dung/du-lieu/them-tu-goi`)?.route?.id).toBe("dung/du-lieu/them-tu-goi");
    expect(parseStudioPath(`/studio/du-an/${S}/van-hanh/du-lieu/customers/nhap`)?.route?.id).toBe("van-hanh/du-lieu/:bang/nhap");
    expect(parseStudioPath(`/studio/du-an/${S}/van-hanh/du-lieu/customers/r1`)?.route?.id).toBe("van-hanh/du-lieu/:bang/:id");
  });

  it("an empty path is the project root and an unknown path has no route", () => {
    expect(parseStudioPath(`/studio/du-an/${S}`)).toMatchObject({ slug: S, route: null, root: true });
    expect(parseStudioPath(`/studio/du-an/${S}/nope/nope`)).toMatchObject({ slug: S, route: null, root: false });
  });

  it("an out-of-pattern path parameter falls back to the project root (never enters the path)", () => {
    expect(studioHref(S, "dung/du-lieu/:bang", { bang: "Bad-Name" })).toBe(`/studio/du-an/${S}`);
    expect(parseStudioPath(`/studio/du-an/${S}/dung/du-lieu/Bad`)?.route).toBeNull();
  });
});

describe("slug and URL value validation (FR-028)", () => {
  const bad = ["A", "", "a b", "a/../b", "a".repeat(65), "%2e%2e", "ă", "-a"];
  it.each(bad)("slug %j is not a project", (slug) => {
    expect(parseStudioPath(`/studio/du-an/${slug}/dung/du-lieu`)).toBeNull();
    expect(currentProjectSlug(`/studio/du-an/${slug}/dung/du-lieu`)).toBeNull();
    expect(studioHref(slug, "dung/du-lieu")).toBe("/studio");
  });

  it("accepts the boundary slugs", () => {
    expect(currentProjectSlug("/studio/du-an/a0_-b/dung/du-lieu")).toBe("a0_-b");
    expect(currentProjectSlug(`/studio/du-an/${"a".repeat(64)}`)).toBe("a".repeat(64));
    expect(currentProjectSlug("/studio")).toBeNull();
  });

  it("drops ?bang=/?truong=/?app= outside the name pattern and ?nguoi=/?bc= outside the id pattern", () => {
    expect(studioHref(S, "dung/man-hinh", {}, { bang: "A b", app: "x/../y" })).toBe(`/studio/du-an/${S}/dung/man-hinh`);
    expect(studioHref(S, "dung/man-hinh", {}, { bang: "ok_1", app: "crm" })).toBe(`/studio/du-an/${S}/dung/man-hinh?app=crm&bang=ok_1`);
    expect(studioHref(S, "van-hanh/du-lieu/:bang/:id", { bang: "t", id: "a/b" })).toBe(`/studio/du-an/${S}`);
    expect(studioHref(S, "van-hanh/tu-dong/lan-chay", {}, { luat: "r 1" })).toBe(`/studio/du-an/${S}/van-hanh/tu-dong/lan-chay`);
  });

  it("?q= never reaches a project path", () => {
    for (const r of ROUTES) expect(r.query).not.toContain("q");
    expect(studioHref(S, "dung/bao-cao", {}, { q: "x" })).toBe(`/studio/du-an/${S}/dung/bao-cao`);
    // specs/074 T259: selection, table, app and the three run-filter keys are kept; anything else is dropped
    expect(studioHref(S, "dung/bao-cao", {}, { bc: "r1", bang: "t", app: "crm", dateFrom: "2026-01-01", dateTo: "2026-02-01", ownerId: "u1", q: "x" }))
      .toBe(`/studio/du-an/${S}/dung/bao-cao?bc=r1&bang=t&app=crm&dateFrom=2026-01-01&dateTo=2026-02-01&ownerId=u1`);
  });
});

describe("calendarsHref (T204)", () => {
  it("inside a project it is the Studio page; elsewhere the old admin screen", () => {
    expect(calendarsHref(`/studio/du-an/${S}/dung/tu-dong`)).toBe(`/studio/du-an/${S}/dung/tu-dong/lich-lam-viec`);
    expect(calendarsHref("/studio/workflows")).toBe("/admin/business-calendars");
    expect(calendarsHref(null)).toBe("/admin/business-calendars");
  });
});

describe("safeNext (FR-021)", () => {
  const ok = ["/studio", "/studio/moi", "/studio/tai-khoan", `/studio/du-an/${S}`, `/studio/du-an/${S}/dung/du-lieu?bang=x`, `/studio/du-an/${S}/bao-tri/kham`];
  it.each(ok)("accepts %s", (raw) => expect(safeNext(raw, S)).toBe(raw));

  const bad: unknown[] = [
    "//x", "https://x", "/login", "", undefined, null, 5, {},
    "/studio/du-an/other/dung/du-lieu", `/studio/du-an/${S}x`, `/studio/du-an/${S}x/y`,
    `/studio/du-an/${S}/../other/dung`, `/studio/du-an/${S}/../../login`,
    `/studio/du-an/${S}/%2e%2e/other`, `/studio/du-an/${S}/%2E%2E/other`, `/studio/du-an/${S}/%2F%2Fx`,
    `/studio/du-an/${S}/..\\x`, `/studio/du-an/${S}\\x`,
    `/studio/du-an/${S}/a\nb`, `/studio/du-an/${S}/a\tb`, `/studio/du-an/${S}/a\u0000b`,
    `/studio/du-an/${S}//evil`, "/studio/../login", "/studiox", "javascript:alert(1)",
  ];
  it.each(bad.map((b) => [JSON.stringify(b), b]))("drops %s to the first page of the project", (_l, raw) => {
    expect(safeNext(raw, S)).toBe(first);
  });

  it("an invalid S falls back to /studio", () => {
    expect(safeNext("/studio/moi", "A b")).toBe("/studio");
  });
});

describe("projectTokenMatches (FR-022, FR-026) — closed unless the server confirmed", () => {
  const home = "northwind";
  const operator = { isPlatformOperator: true };
  const inSession = (slug: string) => ({ isPlatformOperator: true, workspace: { tenantSlug: slug } });

  it("session W = S matches", () => expect(projectTokenMatches("acme", inSession("acme"), home)).toBe(true));
  it("session W != S does not", () => expect(projectTokenMatches("other", inSession("acme"), home)).toBe(false));
  it("S = H without a session matches (operator at home)", () => expect(projectTokenMatches("northwind", operator, home)).toBe(true));
  it("S = H while a session is open does not", () => expect(projectTokenMatches("northwind", inSession("acme"), home)).toBe(false));
  it("S != H without a session does not", () => expect(projectTokenMatches("acme", operator, home)).toBe(false));
  it("not confirmed as operator does not", () => expect(projectTokenMatches("northwind", {}, home)).toBe(false));
  it("no /auth/me answer does not", () => {
    expect(projectTokenMatches("northwind", null, home)).toBe(false);
    expect(projectTokenMatches("northwind", undefined, home)).toBe(false);
  });
  it("unreadable home slug does not", () => expect(projectTokenMatches("northwind", operator, null)).toBe(false));
});

describe("routeForPanel (kh6: AI Builder's way back to the page it was building)", () => {
  it("gives the mounted page that carries the assistant panel, never a pending or a parameterised one", () => {
    expect(routeForPanel("bang")).toBe("dung/du-lieu");
    expect(routeForPanel("menu")).toBe("dung/man-hinh/app");
    expect(routeForPanel("duyet")).toBe("dung/tu-dong/duyet");
    expect(routeForPanel("thuong-hieu")).toBe("dung/thuong-hieu");
    expect(routeForPanel("vai-tro")).toBe("dung/nguoi-dung-quyen");
    expect(routeForPanel("dashboard")).toBeNull(); // its page carries a path parameter and is not mounted
    expect(routeForPanel(undefined)).toBeNull();
    expect(routeForPanel(null)).toBeNull(); // not dung/man-hinh, whose aiPanel is also null
  });
});

describe("ma-man-moi (T600): four new assistant codes", () => {
  const golden = JSON.parse(
    fs.readFileSync(path.join(process.cwd(), "../nexus-platform/internal/ai/testdata/studio_panels.json"), "utf8"),
  ) as string[];
  it("every aiPanel is in the closed code list shared with the Go side", () => {
    for (const r of ROUTES) if (r.aiPanel !== null) expect(golden, r.id).toContain(r.aiPanel);
  });
  it("the four codes exist and are carried by the matching routes", () => {
    for (const c of ["man-hinh", "chay-thu", "van-hanh", "bao-tri"]) expect(golden).toContain(c);
    const by = (c: string) => ROUTES.filter((r) => r.aiPanel === c).map((r) => r.segments[0] + "/" + r.segments[1]);
    expect(by("man-hinh").every((x) => x === "dung/man-hinh")).toBe(true);
    expect(by("man-hinh").length).toBeGreaterThan(0);
    expect(by("chay-thu")).toEqual(["dung/chay-thu"]);
    expect(ROUTES.filter((r) => r.segments[0] === "van-hanh").every((r) => ["van-hanh", "nguoi-dung"].includes(r.aiPanel ?? ""))).toBe(true);
    expect(ROUTES.filter((r) => r.segments[0] === "bao-tri" && r.ctx === "phien").every((r) => r.aiPanel === "bao-tri")).toBe(true);
  });
});

describe("?nguoi= on van-hanh/nguoi-dung (specs/078 T036, FR-133)", () => {
  it("round-trips a person id", () => {
    const href = studioHref(S, "van-hanh/nguoi-dung", {}, { nguoi: "0b2f-uuid_1" });
    expect(href).toBe(`/studio/du-an/${S}/van-hanh/nguoi-dung?nguoi=0b2f-uuid_1`);
    expect(parseStudioPath(href)?.query.nguoi).toBe("0b2f-uuid_1");
  });
  it.each(["../roles/x", "a/b", "x y"])("rejects hostile %s in both directions", (bad) => {
    expect(studioHref(S, "van-hanh/nguoi-dung", {}, { nguoi: bad })).toBe(`/studio/du-an/${S}/van-hanh/nguoi-dung`);
    const parsed = parseStudioPath(`/studio/du-an/${S}/van-hanh/nguoi-dung?nguoi=${encodeURIComponent(bad)}`);
    expect(parsed?.query.nguoi).toBeUndefined();
  });
});
