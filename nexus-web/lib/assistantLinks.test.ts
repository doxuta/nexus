// specs/076 ai3 T052: a link the model wrote becomes a Studio link, or plain text.
import { describe, expect, it } from "vitest";
import { resolveAssistantLink } from "./assistantLinks";

const S = "acme";
describe("resolveAssistantLink", () => {
  it.each([
    ["/studio/layout?entity=khach_hang&tab=form", "/studio/du-an/acme/dung/man-hinh/khach_hang?man=form"],
    ["/studio/verticals", "/studio/du-an/acme/dung/du-lieu/them-tu-goi"],
    ["/studio/approvals", "/studio/du-an/acme/dung/tu-dong/duyet"],
    ["/studio/workqueues", "/studio/du-an/acme/dung/tu-dong/hang-doi"],
    ["/studio/workflows", "/studio/du-an/acme/dung/tu-dong"],
    ["/reports", "/studio/du-an/acme/dung/bao-cao"],
    ["/dashboards", "/studio/du-an/acme/dung/bao-cao"],
    ["/admin/users", "/studio/du-an/acme/van-hanh/nguoi-dung"],
    ["/admin/roles", "/studio/du-an/acme/dung/nguoi-dung-quyen"],
  ])("%s", (href, want) => expect(resolveAssistantLink(href, S)).toBe(want));
  it.each([
    "/studio/khong-co", "//evil.com", "/\\evil.com", "https://evil.com", "/unknown/page", "",
    "/constructor", "/studio/layout?entity=Bad-Name&tab=form", "/studio/layout?entity=../x",
  ])("null for %s", (href) => expect(resolveAssistantLink(href, S)).toBeNull());
  it("null for an invalid slug", () => expect(resolveAssistantLink("/reports", "A/B")).toBeNull());
});
