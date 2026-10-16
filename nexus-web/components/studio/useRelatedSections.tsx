"use client";

// The read-only related lists of a table page (specs/074 lát 4: Màn hình, Liên kết, Báo cáo, Dữ liệu) + Luật / Quyền
// links kept from the old side nav (no list to show for them yet). Each list has its own query, so one failing request
// marks only its own section.
import Link from "next/link";
import { useQueries, useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { getLayout, getRole, listAllRelationships, listApprovalProcesses, listRoles, listWorkflows, type WorkflowDetail, listEntities, listAdminReports, listFields, cellField, type EntityMeta } from "@/lib/api";
import { qk } from "@/lib/query/keys";
import { t, tf } from "@/lib/i18n";
import { errorMessage } from "@/lib/errorMessages";
import { isMounted, studioHref, type RouteId } from "@/lib/studioRoutes";
import type { RelatedSection } from "@/components/studio/RelatedLists";
import { RelatedRuleRow } from "@/components/studio/RelatedRuleRow";
import { levelOf } from "@/lib/accessLevels";
import { ruleSentence } from "@/lib/ruleSentence";

const TIER_KEY = { none: "studio.build.access.level.none", view: "studio.build.access.level.view", edit: "studio.build.access.level.edit", custom: "studio.build.access.level.custom" } as const;

export function useRelatedSections(slug: string, entity: EntityMeta, label: string): RelatedSection[] {
  const name = entity.name;
  const fieldsQ = useQuery({ queryKey: qk.fields(name), queryFn: () => listFields(name), staleTime: 60_000 });
  const layoutQ = useQuery({ queryKey: qk.layout(name), queryFn: () => getLayout(name), staleTime: 60_000 });
  const linksQ = useQuery({ queryKey: qk.entityLinks(name), queryFn: () => listAllRelationships(name), staleTime: 60_000 });
  const entitiesQ = useQuery({ queryKey: ["studio", "count", "entities"], queryFn: listEntities, staleTime: 60_000 });
  const reportsQ = useQuery({ queryKey: qk.adminReports(), queryFn: listAdminReports, staleTime: 60_000 });

  const rulesQ = useQuery({ queryKey: qk.adminWorkflows(), queryFn: listWorkflows, staleTime: 60_000 });
  const approvalsQ = useQuery({ queryKey: ["studio", "count", "approvals"], queryFn: listApprovalProcesses, staleTime: 60_000 });
  const rolesQ = useQuery({ queryKey: qk.roles(), queryFn: listRoles, staleTime: 60_000 });
  // Same cache key as the access matrix (qk.role): both read one copy of each role's grants.
  const roleQs = useQueries({ queries: (rolesQ.data ?? []).map((r) => ({ queryKey: qk.role(r.id), queryFn: () => getRole(r.id), staleTime: 60_000 })) });

  const state = (...qs: { isPending: boolean; isError: boolean }[]): RelatedSection["state"] =>
    qs.some((q) => q.isError) ? "error" : qs.some((q) => q.isPending) ? "loading" : "ready";
  const err = (...qs: { isError: boolean; error: unknown }[]) => {
    const q = qs.find((x) => x.isError);
    return q ? `${t("studio.build.data.related.loadFail")} ${errorMessage(q.error)}` : undefined;
  };
  const retry = (...qs: { refetch: () => unknown }[]) => () => qs.forEach((q) => void q.refetch());
  const linkTo = (route: RouteId, text: string, params: Record<string, string> = {}, query: Record<string, string> = {}) => (
    <Link key={text} href={studioHref(slug, route, params, query)}>{text}</Link>
  );

  // Screens: the form reads "a of b fields" = distinct field cells on the edit view / all fields of the table.
  const screenItems: ReactNode[] = [];
  const layout = layoutQ.data;
  if (layout && isMounted("dung/man-hinh/:bang")) {
    const placed = new Set<string>();
    for (const sec of layout.views?.edit?.sections ?? []) for (const c of sec.fields) { const f = cellField(c); if (f) placed.add(f.field); }
    screenItems.push(linkTo("dung/man-hinh/:bang", tf("studio.build.data.related.screenForm", { table: label, a: placed.size, b: fieldsQ.data?.length ?? 0 }), { bang: name }, { man: "form" }));
    screenItems.push(linkTo("dung/man-hinh/:bang", tf("studio.build.data.related.screenList", { table: label, n: layout.views?.list?.columns?.length ?? 0 }), { bang: name }, { man: "danh-sach" }));
  }

  // Links: same direction rule as the table list's "Liên kết tới" (the child holds the relation; N-N goes both ways).
  const labelOf = new Map((entitiesQ.data ?? []).map((e) => [e.name, e.label || e.name]));
  const targets = [...new Set((linksQ.data ?? []).flatMap((r) => {
    const out: string[] = [];
    if (r.childEntityName === name) out.push(r.parentEntityName);
    if (r.relationshipType === "N-N" && r.parentEntityName === name) out.push(r.childEntityName);
    return out.filter((n) => n !== name);
  }))].sort();
  const linkItems = targets.map((n) =>
    isMounted("dung/du-lieu/:bang") ? linkTo("dung/du-lieu/:bang", labelOf.get(n) ?? n, { bang: n }) : <span key={n}>{labelOf.get(n) ?? n}</span>);

  const reports = (reportsQ.data ?? []).filter((r) => r.entityName === name);
  const sections: RelatedSection[] = [
    {
      id: "screens", title: t("studio.build.data.related.screens"), state: state(layoutQ, fieldsQ), items: screenItems,
      empty: t("studio.build.data.related.screensEmpty"), error: err(layoutQ, fieldsQ), onRetry: retry(layoutQ, fieldsQ),
    },
    {
      id: "links", title: t("studio.build.data.related.links"), state: state(linksQ), count: targets.length, items: linkItems,
      empty: t("studio.build.data.related.linksEmpty"), error: err(linksQ), onRetry: retry(linksQ),
      action: isMounted("dung/du-lieu/so-do") ? linkTo("dung/du-lieu/so-do", t("studio.build.data.related.diagram"), {}, { bang: name }) : undefined,
    },
    {
      id: "reports", title: t("studio.build.data.related.reports"), state: state(reportsQ), count: reports.length,
      items: reports.map((r) => (isMounted("dung/bao-cao") ? linkTo("dung/bao-cao", r.name, {}, { bc: r.id }) : <span key={r.id}>{r.name}</span>)),
      empty: t("studio.build.data.related.reportsEmpty"), error: err(reportsQ), onRetry: retry(reportsQ),
      action: isMounted("dung/bao-cao") ? linkTo("dung/bao-cao", t("studio.build.data.related.reportsAll"), {}, { bang: name }) : undefined,
    },
    {
      id: "data", title: t("studio.build.data.related.data"), state: "ready", empty: "",
      items: [<Link key="d" href={`/${name}`}>{t("studio.build.data.related.dataOpen")}</Link>],
    },
  ];
  const rules = (rulesQ.data ?? []).filter((w) => w.entityName === name);
  const nApprovals = (approvalsQ.data ?? []).filter((a) => a.entityName === name).length;
  const ruleItems: ReactNode[] = rules.map((w) => (
    <RelatedRuleRow
      key={w.id} id={w.id} name={w.name} active={!!w.isActive}
      when={ruleSentence(w as WorkflowDetail, entitiesQ.data ?? [], fieldsQ.data ?? []).when}
      href={isMounted("dung/tu-dong/luat/:id") ? studioHref(slug, "dung/tu-dong/luat/:id", { id: w.id }) : undefined}
    />
  ));
  if (nApprovals > 0) ruleItems.push(<span key="appr">{tf("studio.build.data.related.rules.approvals", { n: nApprovals })}</span>);
  if (isMounted("dung/tu-dong")) {
    sections.push({
      id: "rules", title: t("studio.entity.link.rules"), state: state(rulesQ, entitiesQ, fieldsQ), count: rules.length, items: ruleItems,
      empty: t("studio.build.data.related.rules.empty"), error: err(rulesQ, entitiesQ, fieldsQ), onRetry: retry(rulesQ, entitiesQ, fieldsQ),
      action: linkTo("dung/tu-dong", t("studio.build.data.related.rules.all"), {}, { bang: name }),
    });
  }
  if (isMounted("dung/nguoi-dung-quyen")) {
    const roleLevels = (rolesQ.data ?? []).map((r, i) => ({ r, d: roleQs[i]?.data }));
    sections.push({
      id: "access", title: t("studio.entity.link.access"), state: state(rolesQ, ...roleQs), count: roleLevels.length,
      items: roleLevels.map(({ r, d }) => (
        <span key={r.id}>{tf("studio.build.data.related.access.row", { role: r.label || r.name, level: d ? t(TIER_KEY[levelOf(d.actions ?? [], name).tier]) : "—" })}</span>
      )),
      empty: t("studio.build.data.related.access.empty"), error: err(rolesQ, ...roleQs), onRetry: retry(rolesQ, ...roleQs),
      action: linkTo("dung/nguoi-dung-quyen", t("studio.build.data.related.access.open"), {}, { bang: name }),
    });
  }
  return sections;
}
