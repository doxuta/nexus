"use client";

// W7 §11.4 — read-only Team-Sets panel (shared by /admin/teams and /admin/sharing).
// Team sets are ENGINE-DERIVED (resolve-or-create from the creator's memberships when a record is
// written — A5). There is NO write surface; this is an inspect view: "which teams see records
// tagged with this set".
// SLDS (specs/038 T048): a card holding a data-tables/base table (set · team count · team badges,
// the primary team starred); illustration for the empty state, scoped notification for an error.

import { useEffect, useState } from "react";
import { Layers, Star } from "lucide-react";
import { listTeamSets, ensureAuth, ApiError, type AdminTeamSet } from "@/lib/admin";
import { t, tf } from "@/lib/i18n";
import { Badge } from "@/components/slds/Badge";
import { Card } from "@/components/slds/Card";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

function shortId(id: string): string {
  return id.replace(/-/g, "").slice(0, 8);
}

export function TeamSetsPanel({ className }: { className?: string }) {
  const [sets, setSets] = useState<AdminTeamSet[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    (async () => {
      try {
        await ensureAuth();
        const s = await listTeamSets();
        if (alive) setSets(s);
      } catch (e) {
        if (alive) setErr(e instanceof ApiError ? e.message : t("page.teamSets.loadError"));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => { alive = false; };
  }, []);

  let body;
  if (err) {
    body = (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{err}</ScopedNotification>
      </div>
    );
  } else if (sets.length === 0) {
    body = (
      <EmptyState
        className="slds-p-around_medium"
        icon={Layers}
        title={t("page.teamSets.empty")}
        description={t("page.teamSets.emptyDesc")}
      />
    );
  } else {
    body = (
      <table className="slds-table slds-table_cell-buffer slds-table_bordered" aria-label={t("page.teamSets.grid")}>
        <thead>
          <tr className="slds-line-height_reset">
            {(["set", "count", "teams"] as const).map((c) => (
              <th key={c} scope="col">
                <div className="slds-truncate" title={t(`page.teamSets.col.${c}`)}>{t(`page.teamSets.col.${c}`)}</div>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {sets.map((s) => (
            <tr key={s.id} className="slds-hint-parent">
              <th scope="row" data-label={t("page.teamSets.col.set")}>
                <div className="slds-truncate slds-text-font_monospace" title={s.id}>
                  {tf("page.teamSets.set", { id: shortId(s.id) })}
                </div>
              </th>
              <td data-label={t("page.teamSets.col.count")} className="tabular-nums">{s.teams.length}</td>
              <td data-label={t("page.teamSets.col.teams")} className="slds-cell-wrap">
                {s.teams.length === 0 ? (
                  <span className="slds-text-color_weak">{t("page.teamSets.emptySet")}</span>
                ) : (
                  <div className="flex flex-wrap gap-1">
                    {s.teams.map((team) =>
                      s.primaryTeamId === team.id ? (
                        <Badge key={team.id} variant="inverse" iconLeft={Star} iconTitle={t("page.teamSets.primary")}>
                          {team.name}
                          <span className="slds-assistive-text"> ({t("page.teamSets.primary")})</span>
                        </Badge>
                      ) : (
                        <Badge key={team.id}>{team.name}</Badge>
                      ),
                    )}
                  </div>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    );
  }

  return (
    <Card
      title={t("page.teamSets.title")}
      icon={{ glyph: Layers, name: "groups" }}
      count={loading || err ? undefined : sets.length}
      padded={false}
      loading={loading}
      className={className}
    >
      <p className="slds-p-horizontal_medium slds-p-bottom_small slds-text-color_weak">{t("page.teamSets.intro")}</p>
      {body}
    </Card>
  );
}
