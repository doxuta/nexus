"use client";

// Chạy thử › Vai: one row per role of the project, "Xem như" at the end of the row (specs/077 f5-trial-view-as).
// A plain data table in the working surface - no modal, no card. The click hands over to lib/studioViewAs.
import { Eye } from "lucide-react";
import Link from "next/link";
import { useState } from "react";
import { Button } from "@/components/slds/Button";
import { listAuditFiltered } from "@/lib/admin";
import { levelOf } from "@/lib/accessLevels";
import { getRole, listEntities, ApiError, type EntityMeta } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";
import { previewInWorkspace, viewAsRole } from "@/lib/studioViewAs";
import { inWorkspace } from "@/lib/workspace";
import { useQueryClient } from "@tanstack/react-query";
import { PUBLISH_STATE_KEY } from "../usePublishState";
import { QueryState } from "../QueryState";
import { useProjectQuery } from "../useProjectQuery";
import { useTrialRoles } from "./useTrialRoles";

export interface TrialApp {
  id: string;
  name: string;
  label?: string;
}

function failure(e: unknown): string {
  if (e instanceof ApiError && e.code === "PREVIEW_ROLE_NOT_FOUND") return t("studio.build.trial.roles.err.notFound");
  if (e instanceof ApiError && e.code === "WORKSPACE_ENDED") return t("studio.build.trial.roles.err.ended");
  if (e instanceof ApiError && e.status === 403) return t("studio.build.trial.roles.err.forbidden");
  return t("studio.build.trial.roles.err.generic");
}

const SHOWN = 3;
const names = (xs: string[]) =>
  (xs.length > SHOWN ? [...xs.slice(0, SHOWN), t("studio.build.trial.roles.sees.more").replace("{n}", String(xs.length - SHOWN))] : xs).join(", ");

// One role's column "Thấy": every table of the project (FR-024, not filtered by app), judged by levelOf on the raw grants.
// ponytail: one getRole per role in parallel (~10 roles); the key carries the project code (FR-003).
function SeesCell({ slug, roleId, tables }: { slug: string; roleId: string; tables: EntityMeta[] }) {
  const q = useProjectQuery(slug, ["role", roleId], () => getRole(roleId));
  if (q.isError) return <span className="slds-text-color_weak">{t("studio.build.trial.roles.err.sees")}</span>;
  if (!q.data) return <span className="slds-text-color_weak">…</span>;
  const grants = q.data.actions ?? [];
  const seen: string[] = [];
  const hidden: string[] = [];
  for (const tb of tables) (levelOf(grants, tb.name).tier === "none" ? hidden : seen).push(tb.label || tb.name);
  return (
    <div className="slds-text-body_small">
      {seen.length === 0 ? (
        <>
          {t("studio.build.trial.roles.sees.none")}.{" "}
          <Link href={studioHref(slug, "dung/nguoi-dung-quyen/vai/:id", { id: roleId })}>{t("studio.build.trial.roles.setAccess")}</Link>
        </>
      ) : (
        <>
          {t("studio.build.trial.roles.sees.yes")} {names(seen)}.
          {hidden.length > 0 && <> {t("studio.build.trial.roles.sees.no")} {names(hidden)}.</>}
        </>
      )}
    </div>
  );
}

export function TrialRolesTable({ slug, app, draft }: { slug: string; app: TrialApp | null; draft?: boolean }) {
  const qc = useQueryClient();
  const roles = useTrialRoles(slug);
  // ponytail: 100 newest preview rows, enough for a small project; the note is a courtesy, so a failed read just hides it.
  const seenQ = useProjectQuery(slug, ["studio", "trial", "seen"], () => listAuditFiltered({ entityName: "__preview__", action: "create", limit: 100 }));
  const entities = useProjectQuery(slug, ["studio", "trial", "entities"], listEntities);
  const tables = (entities.data ?? []).filter((e) => e.draftState !== "deleted");
  const [busy, setBusy] = useState<string | null>(null);
  const [problem, setProblem] = useState<{ roleId: string; text: string; retry: boolean } | null>(null);

  async function open(roleId: string) {
    if (!app || busy) return;
    setBusy(roleId);
    setProblem(null);
    try {
      await viewAsRole({ roleId, appId: app.id, appSlug: app.name, draft });
      // The tab navigates away; stay "busy" so a second click cannot start a second session.
    } catch (e) {
      if (e instanceof ApiError && e.code === "PREVIEW_DRAFT_UNAVAILABLE") {
        // The draft vanished (published elsewhere): re-read the state so the toggle drops out; not an error to show.
        void qc.invalidateQueries({ queryKey: ["project", slug, ...PUBLISH_STATE_KEY] });
        setBusy(null);
        return;
      }
      const gone = e instanceof ApiError && e.code === "PREVIEW_ROLE_NOT_FOUND";
      setProblem({ roleId, text: failure(e), retry: !gone && !(e instanceof ApiError && (e.status === 403 || e.code === "WORKSPACE_ENDED")) });
      if (gone) void roles.refetch();
      setBusy(null);
    }
  }

  // Latest "Xem như" per role, matched by roleId (stable) and dropped when the role is no longer in the project.
  const seen = new Map<string, string>();
  for (const e of seenQ.data?.entries ?? []) {
    const id = (e.diff?.after as { roleId?: unknown } | undefined)?.roleId;
    if (typeof id === "string" && (!seen.has(id) || e.at > seen.get(id)!)) seen.set(id, e.at);
  }
  const seenRoles = (roles.data ?? []).filter((r) => seen.has(r.id)).sort((a, b) => seen.get(b.id)!.localeCompare(seen.get(a.id)!));
  const blocked = inWorkspace() && !previewInWorkspace;
  const note = !app ? t("studio.build.trial.roles.noApp") : null;

  return (
    <section className="slds-m-top_medium" aria-labelledby="trial-roles-h">
      <h2 id="trial-roles-h" className="slds-text-heading_medium">{t("studio.build.trial.roles.title")}</h2>
      <p className="slds-text-color_weak slds-m-bottom_small">{t("studio.build.trial.roles.hint")}</p>
      {seenRoles.length > 0 && (
        <p className="slds-m-bottom_small">{tf("studio.build.trial.roles.seenNote", { names: seenRoles.map((r) => r.label || r.name).join(", ") })}</p>
      )}
      {note && <p className="slds-text-color_weak">{note}</p>}
      <QueryState
        query={roles}
        empty={
          <div>
            <p>{t("studio.build.trial.roles.empty")}</p>
            <Link href={studioHref(slug, "dung/nguoi-dung-quyen")}>{t("studio.build.trial.roles.openAccess")}</Link>
          </div>
        }
      >
        <table className="slds-table slds-table_bordered slds-table_cell-buffer slds-max-medium-table_stacked-horizontal">
          <thead>
            <tr className="slds-line-height_reset">
              <th scope="col"><div className="slds-truncate">{t("studio.build.trial.roles.col.role")}</div></th>
              <th scope="col"><div className="slds-truncate">{t("studio.build.trial.roles.col.code")}</div></th>
              {seenRoles.length > 0 && <th scope="col"><div className="slds-truncate">{t("studio.build.trial.roles.lastSeen")}</div></th>}
              <th scope="col"><div className="slds-truncate">{t("studio.build.trial.roles.col.sees")}</div></th>
              <th scope="col"><span className="slds-assistive-text">{t("studio.build.trial.roles.viewAs")}</span></th>
            </tr>
          </thead>
          <tbody>
            {(roles.data ?? []).map((r) => {
              const label = r.label || r.name;
              return (
                <tr key={r.id} className="slds-hint-parent">
                  <th scope="row" data-label={t("studio.build.trial.roles.col.role")}><div className="slds-truncate" title={label}>{label}</div>
                    {r.name === "tenant_owner" && <p className="slds-text-body_small slds-text-color_weak">{t("studio.build.trial.roles.ownerNote")}</p>}
                  </th>
                  <td data-label={t("studio.build.trial.roles.col.code")}><div className="slds-truncate" title={r.name}>{r.name}</div></td>
                  {seenRoles.length > 0 && (
                    <td data-label={t("studio.build.trial.roles.lastSeen")}>{seen.has(r.id) ? formatDateTime(seen.get(r.id)!) : ""}</td>
                  )}
                  <td data-label={t("studio.build.trial.roles.col.sees")}><SeesCell slug={slug} roleId={r.id} tables={tables} /></td>
                  <td className="slds-text-align_right">
                    {!note && !blocked && (
                      <Button
                        variant="neutral"
                        size="small"
                        iconLeft={Eye}
                        className="whitespace-nowrap"
                        aria-label={`${t("studio.build.trial.roles.viewAs")} ${label}`}
                        disabled={busy !== null}
                        onClick={() => void open(r.id)}
                      >
                        {busy === r.id ? t("studio.build.trial.roles.opening") : t("studio.build.trial.roles.viewAs")}
                      </Button>
                    )}
                    {problem?.roleId === r.id && (
                      <p role="alert" className="slds-text-color_error slds-text-body_small">
                        {problem.text}{" "}
                        {problem.retry && (
                          <Button variant="neutral" size="small" onClick={() => void open(r.id)}>{t("studio.query.retry")}</Button>
                        )}
                      </p>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
        <p className="slds-text-color_weak slds-m-top_small">{t("studio.build.trial.roles.note")}</p>
      </QueryState>
    </section>
  );
}
