// lib/assistantLinks.ts — turn a link the MODEL wrote into a Studio link of this project (specs/076 ai3 T058).
// Anything that does not map is null (the text then renders as plain words): a model-made string never reaches
// router.push unless a row of this table claims it, and the target is built only by studioHref (FR-129).
import { NAME_RE, SLUG_RE, studioHref, type RouteId } from "@/lib/studioRoutes";

// Keys are the OLD routes the model still names, written without the leading slash so the "no link to an old Studio
// route" gate (a literal /studio/<old> in source) does not mistake this table for a link; lookup uses path.slice(1).
const SIMPLE: Record<string, RouteId> = {
  "studio/verticals": "dung/du-lieu/them-tu-goi",
  "studio/approvals": "dung/tu-dong/duyet",
  "studio/workqueues": "dung/tu-dong/hang-doi",
  "studio/workflows": "dung/tu-dong",
  reports: "dung/bao-cao",
  dashboards: "dung/bao-cao",
  "admin/users": "van-hanh/nguoi-dung",
  "admin/roles": "dung/nguoi-dung-quyen",
};
const TABS: Record<string, string> = { form: "form", list: "list", detail: "detail" };

export function resolveAssistantLink(href: string, slug: string): string | null {
  if (!SLUG_RE.test(slug) || typeof href !== "string" || !href.startsWith("/") || href.startsWith("//") || href.startsWith("/\\")) return null;
  const qAt = href.indexOf("?");
  const path = qAt < 0 ? href : href.slice(0, qAt);
  const sp = new URLSearchParams(qAt < 0 ? "" : href.slice(qAt + 1));
  if (path.slice(1) === "studio/layout") {
    const entity = sp.get("entity");
    if (!entity || !NAME_RE.test(entity)) return null;
    const tab = TABS[sp.get("tab") ?? ""];
    return studioHref(slug, "dung/man-hinh/:bang", { bang: entity }, tab ? { man: tab } : undefined);
  }
  const key = path.slice(1);
  return Object.hasOwn(SIMPLE, key) ? studioHref(slug, SIMPLE[key]) : null; // hasOwn: "/constructor" is not a route
}
