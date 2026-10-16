// specs/079 f7-khoi-phuc-ke-hoach T111/T113: bao-tri/sao-luu/khoi-phuc/<backupId>[?lan=<restoreId>].
import { describe, expect, it } from "vitest";
import { NHA_MOUNT_ALLOWLIST, ROUTES, parseStudioPath, studioHref } from "@/lib/studioRoutes";

const S = "acme";
const UUID = "0b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d";
const ID = "bao-tri/sao-luu/khoi-phuc/:id";

describe("restore route", () => {
  it("is a home-token (nha) page, no assistant, and its component is on the nha allow-list", () => {
    const r = ROUTES.find((x) => x.id === ID)!;
    expect(r.ctx).toBe("nha");
    expect(r.aiPanel).toBeNull();
    expect(r.mounted).toBe(true);
    expect(NHA_MOUNT_ALLOWLIST).toContain(r.mounts);
  });

  it("builds and parses the href with the backup id and ?lan=", () => {
    const href = studioHref(S, ID, { id: UUID }, { lan: UUID });
    expect(href).toBe(`/studio/du-an/${S}/bao-tri/sao-luu/khoi-phuc/${UUID}?lan=${UUID}`);
    const p = parseStudioPath(href)!;
    expect(p.route?.id).toBe(ID);
    expect(p.params.id).toBe(UUID);
    expect(p.query.lan).toBe(UUID);
  });

  it("an unknown query key is dropped; a ?lan= that is not a UUID is dropped", () => {
    expect(studioHref(S, ID, { id: UUID }, { lan: "nope", x: "1" })).toBe(`/studio/du-an/${S}/bao-tri/sao-luu/khoi-phuc/${UUID}`);
    expect(parseStudioPath(`/studio/du-an/${S}/bao-tri/sao-luu/khoi-phuc/${UUID}?lan=nope&x=1`)!.query).toEqual({});
  });

  it("a backup id that is not a UUID is no page of the project", () => {
    expect(parseStudioPath(`/studio/du-an/${S}/bao-tri/sao-luu/khoi-phuc/ab12`)!.route).toBeNull();
    expect(studioHref(S, ID, { id: "ab12" })).toBe(`/studio/du-an/${S}`);
  });
});
