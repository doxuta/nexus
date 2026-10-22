"use client";

// The preview contract between the assistant and the editors (specs/076 f4-proposal-overlay-hook, D-AI-8).
// F4 owns this file; F2/F3 PAINT the ghost: an editor calls useProposedChange(target) and draws `.nx-proposed` when it
// gets a change, and adds one row to PAINTED. PAINTED is EMPTY on purpose: a kind nobody paints gets no "Xem trước"
// button (the card keeps only its ChangeList) instead of a button that opens a screen with nothing to see.
// The overlay holds at most ONE change and lives inside the project frame's `key={slug}`, so a project change drops it.
import { usePathname, useRouter } from "next/navigation";
import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { skipNextRouteFocus } from "@/components/studio/useStudioRouteFocus";
import type { AiDraftItem } from "@/lib/aiBuilder";
import { t, tf } from "@/lib/i18n";
import { parseStudioPath, routeForChange, ROUTES, zoneOfRoute } from "@/lib/studioRoutes";
import { stepOfRoute } from "@/lib/buildSteps";
import type { I18nKey } from "@/lib/i18n/vi";

export type ProposalOp = "create" | "update" | "delete";
export interface ProposalTarget { docType: string; docKey?: string; entity?: string; field?: string; view?: string; name?: string }
export interface ProposedChange {
  proposalKey: string; docType: string; docKey?: string; target: ProposalTarget; op: ProposalOp;
  label: string; before?: unknown; after?: unknown; changedPaths?: string[]; moreChanged?: number; appName?: string;
}

/** Kinds an editor really draws (each row needs a useProposedChange caller). f2-ai-preview-data-rules: table list + table page
 *  (entity create/delete, field create/update), rule list + rule page (workflow create, update, delete).
 *  Lát 34: menu, ma trận quyền, báo cáo, thương hiệu. */
export const PAINTED: ReadonlyArray<{ docType: string; op: ProposalOp }> = [
  { docType: "entity", op: "create" }, { docType: "entity", op: "delete" },
  { docType: "field", op: "create" }, { docType: "field", op: "update" },
  { docType: "layout", op: "update" }, // f3 screen-editor-ai-preview: form + record page (list screens are not drawn yet)
  { docType: "workflow", op: "create" }, { docType: "workflow", op: "update" }, { docType: "workflow", op: "delete" },
  { docType: "app", op: "update" },
  { docType: "role", op: "update" }, // an access card (propose_access): the role's cells on the access matrix
  { docType: "report", op: "create" },
  { docType: "report", op: "update" },
  { docType: "report", op: "delete" },
  { docType: "branding", op: "update" },
];

// The server names a change in Vietnamese (internal/ai/change.go: Bảng "x", Trường "y"); a table or a field is named again
// here from its code, so the page draws it in the UI language. Other kinds keep the server's label.
function docLabel(c: NonNullable<AiDraftItem["change"]>): string {
  if (c.docType === "entity" && c.target?.entity) return tf("studio.build.ai.doc.entity", { name: c.target.entity });
  if (c.docType === "field" && c.target?.field) return tf("studio.build.ai.doc.field", { name: c.target.field });
  return c.label;
}

/** The card's change from the draft the server sent; null when the server sent no `change` (older draft) or an op we don't know. */
export function proposedFromItem(item: AiDraftItem, proposalKey: string): ProposedChange | null {
  const c = item.change;
  if (!c || (c.op !== "create" && c.op !== "update" && c.op !== "delete")) return null;
  return {
    proposalKey, docType: c.docType, docKey: c.docKey, op: c.op as ProposalOp, label: docLabel(c), before: c.before, after: c.after,
    changedPaths: c.changedPaths, moreChanged: c.moreChanged, appName: item.entity?.appName,
    target: { docType: c.docType, docKey: c.docKey, ...c.target },
  };
}

export interface ActivePreview {
  change: ProposedChange;
  /** "Khu › Bước" of the page the assistant opened; absent when the page already drew the kind. */
  opened?: string;
  /** Label of the target screen, for "Đang tô sáng trên …". */
  screen: string;
}
export interface ProposalOverlayApi {
  active: ActivePreview | null;
  canPreview: (c: ProposedChange) => boolean;
  toggle: (c: ProposedChange) => void;
  clear: () => void;
}

const Ctx = createContext<ProposalOverlayApi | null>(null);

/** null outside a project frame ⇒ no preview button anywhere. */
export const useProposalOverlay = (): ProposalOverlayApi | null => useContext(Ctx);

const matches = (c: ProposedChange, target: Partial<ProposalTarget>): boolean =>
  (Object.keys(target) as (keyof ProposalTarget)[]).every((k) => target[k] === undefined || (k === "docKey" ? c.docKey : c.target[k]) === target[k]);

export function useProposedChange(target: ProposalTarget): ProposedChange | null {
  const a = useContext(Ctx)?.active;
  return a && matches(a.change, target) ? a.change : null;
}
export function useProposedChanges(docType: string, filter?: Partial<ProposalTarget>): ProposedChange[] {
  const a = useContext(Ctx)?.active;
  return a && a.change.docType === docType && matches(a.change, filter ?? {}) ? [a.change] : [];
}

const whereLabel = (path: string): string => {
  const r = parseStudioPath(path)?.route ?? null;
  const step = stepOfRoute(r);
  const zone = zoneOfRoute(r);
  const zoneKey = (zone === "dung" ? "studio.zone.build" : zone === "van-hanh" ? "studio.zone.operate" : "studio.zone.maintain") as I18nKey;
  const leaf = step ? t(`studio.step.${step}.label` as I18nKey) : r ? t(r.labelKey) : "";
  return leaf ? `${t(zoneKey)} › ${leaf}` : t(zoneKey);
};

interface Props { slug: string; children: ReactNode; /** Painters F2/F3 added; default = the module table. Only tests and /dev/kit pass it. */ painted?: ReadonlyArray<{ docType: string; op: ProposalOp }>; /** false = never router.push (the /dev/kit probe has no project to open). */ navigate?: boolean }

export function ProposalOverlayProvider({ slug, children, painted = PAINTED, navigate = true }: Props) {
  const router = useRouter();
  const pathname = usePathname() ?? "";
  const [active, setActive] = useState<ActivePreview | null>(null);

  const dest = useCallback(
    (c: ProposedChange) => (painted.some((p) => p.docType === c.docType && p.op === c.op) ? routeForChange(c, parseStudioPath(pathname), slug) : null),
    [painted, pathname, slug],
  );
  const canPreview = useCallback((c: ProposedChange) => dest(c) !== null, [dest]);
  const clear = useCallback(() => setActive(null), []);
  const toggle = useCallback((c: ProposedChange) => {
    if (active?.change.proposalKey === c.proposalKey) return setActive(null);
    const d = dest(c);
    if (!d) return;
    const next: ActivePreview = { change: c, screen: t(ROUTES.find((r) => r.id === d.route)?.labelKey ?? "studio.route.dataList") };
    if (!d.stay && navigate) {
      skipNextRouteFocus(); // focus stays in the assistant's card
      router.push(d.href, { scroll: false });
      next.opened = whereLabel(d.href);
    }
    setActive(next);
  }, [active, dest, router, navigate]);

  const api = useMemo(() => ({ active, canPreview, toggle, clear }), [active, canPreview, toggle, clear]);
  return <Ctx.Provider value={api}>{children}</Ctx.Provider>;
}
