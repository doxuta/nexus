"use client";

// AccessMatrix (specs/074 FR-030..FR-032, lát f2-access-matrix): one row per role, one column per table, each cell one of
// Không thấy / Chỉ xem / Xem và sửa (blueprint data-tables, edited in the cell; the role column is pinned while the
// table scrolls sideways inside its own area, also at 390px). A cell that cannot be told in three words (Tuỳ chỉnh, or
// anything that follows the "every table" row) is a link to the role page instead of a quick edit.
// Each editable cell is a real native <select> in the Tab order from the start (FR-071): hovering adds nothing the
// keyboard or a touch screen would miss.
// Writes: the changed cells are kept as deltas (table, tier). Quick changes of ONE role merge for 300ms, then the newest
// role is read and the deltas are applied on it before the PUT (which replaces the whole set), so a grant the page never
// saw — or an explicit "none" of another table — leaves the PUT exactly as it came in (lib/accessLevels works on the raw array).
import clsx from "clsx";
import Link from "next/link";
import { useEffect, useRef, useState } from "react";
import { getRole, updateRolePermissions } from "@/lib/admin";
import type { RoleDetail, RoleGrant } from "@/lib/api";
import { applyLevel, levelOf, type MatrixTier } from "@/lib/accessLevels";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { Button } from "@/components/slds/Button";
import { Select } from "@/components/slds/Select";
import { useDirectApply } from "@/components/shell/useDirectApply";
import { useProposedChanges, type ProposedChange } from "@/components/studio/assistant/ProposalOverlay";
import { StatusText } from "./StatusText";

interface Delta { table: string; tier: MatrixTier }
interface RoleOps { wait: Delta[]; busy: boolean; timer: ReturnType<typeof setTimeout> | null }

const TIERS = ["none", "view", "edit"] as const;
const TIER_KEY = { none: "studio.build.access.level.none", view: "studio.build.access.level.view", edit: "studio.build.access.level.edit", custom: "studio.build.access.level.custom" } as const;
const MERGE_MS = 300;

// An assistant access card (propose_access): a "role" change, docKey = role id, before/after = { grants, labels } of the
// tables it touches (plus the role's "*" rows). The note says the level a cell would get and the one it has.
type AccessSide = { grants?: RoleGrant[]; labels?: Record<string, string> };
const side = (v: unknown): AccessSide => (typeof v === "object" && v !== null ? (v as AccessSide) : {});
const tierText = (v: unknown, table: string): string | null => {
  const grants = side(v).grants;
  return Array.isArray(grants) ? t(TIER_KEY[levelOf(grants, table).tier]) : null;
};
const accessTablesOf = (c: ProposedChange | null): string[] => Object.keys(side(c?.after).labels ?? {});

/**
 * What the access card being previewed draws on the page: its role's cells (`proposed`) and, as read-only columns, the
 * tables it names that are not built yet (the table card is still to be applied).
 */
export function accessGhost(change: ProposedChange | null, roles: readonly RoleDetail[], built: readonly { name: string }[]) {
  const tables = accessTablesOf(change);
  const role = change ? roles.find((r) => r.id === change.docKey || r.name === change.target.name) : undefined;
  const labels = side(change?.after).labels ?? {};
  return {
    proposed: role ? new Set(tables.map((n) => `${role.id}|${n}`)) : undefined,
    pending: tables.filter((n) => !built.some((e) => e.name === n)).map((n) => ({ name: n, label: labels[n] || n, pending: true })),
  };
}
function GhostNote({ change, table }: { change: ProposedChange; table: string }) {
  const now = tierText(change.after, table);
  const was = tierText(change.before, table);
  return (
    <div className="slds-text-body_small">
      {now && <StatusText tone="proposed">{tf("studio.build.ai.access.cell", { level: now })}</StatusText>}
      {was && <div className="slds-text-color_weak">{tf("studio.build.ai.beforeNamed", { before: was })}</div>}
    </div>
  );
}

export interface AccessMatrixProps {
  roles: RoleDetail[];
  /** `pending`: a table an assistant card proposes that does not exist yet — drawn, never editable. */
  tables: { name: string; label: string; pending?: boolean }[];
  roleHref: (roleId: string) => string;
  /** `"<roleId>|<table>"` of the cells an assistant proposal touches (ghost tint). */
  proposed?: ReadonlySet<string>;
  /** A role was written: the page puts the saved role into its cache. */
  onSaved?: (role: RoleDetail) => void;
  /** "Xem như" per role (from useRoleViewAs); absent where a preview cannot start. */
  viewAs?: { appLabel: string; open: (role: { id: string; name: string; label?: string }) => Promise<void> };
}

export function AccessMatrix({ roles, tables, roleHref, proposed, onSaved, viewAs }: AccessMatrixProps) {
  const [viewing, setViewing] = useState<string | null>(null);
  // `ops` is the write machinery (timers, in-flight flag); it is only touched in handlers and effects.
  // `shown` is what the grid draws: the deltas not yet confirmed (oldest first) and the last saved copy of a role.
  const ops = useRef(new Map<string, RoleOps>());
  const [shown, setShown] = useState<Record<string, Delta[]>>({});
  const [saved, setSaved] = useState<Record<string, { base: RoleDetail; actions: RoleGrant[] }>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [live, setLive] = useState("");
  // Delivered project: a role grant has no draft, so the change is asked in place before it is queued (lát f2-direct-apply-access).
  const { guard, dialog, notice } = useDirectApply({ inline: true });
  const ghost = useProposedChanges("role")[0] ?? null;
  const latest = useRef({ roles, onSaved });
  useEffect(() => { latest.current = { roles, onSaved }; });

  const opsOf = (id: string): RoleOps => {
    let q = ops.current.get(id);
    if (!q) ops.current.set(id, (q = { wait: [], busy: false, timer: null }));
    return q;
  };
  const settle = (id: string, n: number) => setShown((m) => ({ ...m, [id]: (m[id] ?? []).slice(n) }));

  async function flush(role: RoleDetail) {
    const q = opsOf(role.id);
    if (q.busy || q.wait.length === 0) return;
    q.busy = true;
    const batch = q.wait;
    q.wait = [];
    const label = role.label || role.name;
    try {
      const fresh = await getRole(role.id);
      const actions = batch.reduce((acc, d) => applyLevel(acc, d.table, d.tier), fresh.actions ?? []);
      const out = await updateRolePermissions(role.id, { name: fresh.name, label: fresh.label, actions });
      const next = { ...fresh, ...out, actions: out?.actions ?? actions };
      setSaved((m) => ({ ...m, [role.id]: { base: role, actions: next.actions } }));
      settle(role.id, batch.length);
      latest.current.onSaved?.(next);
      setLive(tf("studio.build.access.saved", { role: label }));
    } catch (e) {
      const why = errorMessage(e);
      const pinned = Object.fromEntries(batch.map((d) => [`${role.id}|${d.table}`, why]));
      setErrors((prev) => ({ ...prev, ...pinned }));
      settle(role.id, batch.length); // the cells go back to what the role really has
      setLive(tf("studio.build.access.saveFailed", { role: label, why }));
    } finally {
      q.busy = false;
      if (q.wait.length > 0) void flush(role);
    }
  }

  function change(role: RoleDetail, table: string, tier: MatrixTier) {
    const q = opsOf(role.id);
    const key = `${role.id}|${table}`;
    setErrors((prev) => (key in prev ? Object.fromEntries(Object.entries(prev).filter(([k]) => k !== key)) : prev));
    q.wait.push({ table, tier });
    setShown((m) => ({ ...m, [role.id]: [...(m[role.id] ?? []), { table, tier }] }));
    if (q.timer) clearTimeout(q.timer);
    q.timer = setTimeout(() => { q.timer = null; void flush(role); }, MERGE_MS);
  }

  // Leaving the page inside the 300ms window must not drop the change.
  useEffect(() => {
    const all = ops.current;
    return () => {
      for (const [id, q] of all) {
        if (!q.timer) continue;
        clearTimeout(q.timer);
        q.timer = null;
        const r = latest.current.roles.find((x) => x.id === id);
        if (r) void flush(r);
      }
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- flush only touches refs and setState
  }, []);

  async function viewAsOf(role: RoleDetail) {
    if (!viewAs || viewing) return;
    setViewing(role.id);
    try {
      await viewAs.open({ id: role.id, name: role.name, label: role.label }); // the tab navigates away on success: stay locked
    } catch (e) {
      setViewing(null);
      setLive(errorMessage(e));
    }
  }

  const grantsOf = (role: RoleDetail): RoleGrant[] => {
    const s = saved[role.id];
    const base = s && s.base === role ? s.actions : role.actions ?? [];
    return (shown[role.id] ?? []).reduce((acc, d) => applyLevel(acc, d.table, d.tier), base);
  };

  return (
    <>
      {notice && !dialog && <p className="slds-p-horizontal_medium slds-p-bottom_x-small">{notice}</p>}
      {dialog && <div className="slds-p-horizontal_medium slds-p-bottom_small">{dialog}</div>}
      <p role="status" aria-live="polite" className="slds-text-body_small slds-text-color_weak slds-p-horizontal_medium">
        {live}
      </p>
      <div className="overflow-x-auto">
        <table className="slds-table slds-table_bordered" aria-label={t("studio.build.access.matrix.label")}>
          <thead>
            <tr className="slds-line-height_reset">
              <th scope="col" className="sticky left-0 z-10 min-w-36 max-w-60 slds-theme_default">
                <div className="slds-truncate">{t("studio.build.access.matrix.role")}</div>
              </th>
              {tables.map((tb) => (
                <th key={tb.name} scope="col" className={tb.pending ? "nx-proposed" : undefined}>
                  <div className="slds-truncate" title={tb.name}>{tb.label}</div>
                  {tb.pending && <StatusText tone="proposed">{t("studio.assistant.card.lead")}</StatusText>}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {roles.map((role) => {
              const roleText = role.label || role.name;
              const grants = grantsOf(role);
              return (
                <tr key={role.id} className="slds-hint-parent">
                  <th scope="row" className="sticky left-0 z-10 min-w-36 max-w-60 slds-theme_default">
                    <div className="slds-grid slds-grid_vertical-align-center slds-grid_align-spread">
                      <div className="slds-truncate" title={role.name}>
                        <Link href={roleHref(role.id)}>{roleText}</Link>
                      </div>
                      {viewAs && (
                        <Button
                          variant="base"
                          className="slds-m-left_x-small"
                          disabled={viewing !== null}
                          aria-label={tf("studio.build.access.viewAs.rowFor", { role: roleText })}
                          title={tf("studio.build.access.viewAs.why", { app: viewAs.appLabel })}
                          onClick={() => void viewAsOf(role)}
                        >
                          {t("studio.build.access.viewAs.row")}
                        </Button>
                      )}
                    </div>
                  </th>
                  {tables.map((tb) => {
                    const { tier, inherited } = levelOf(grants, tb.name);
                    const key = `${role.id}|${tb.name}`;
                    const tint = proposed?.has(key) ? "nx-proposed" : undefined;
                    const gc = tint && ghost && (ghost.docKey === role.id || ghost.target.name === role.name) && accessTablesOf(ghost).includes(tb.name) ? ghost : null;
                    const mark = gc ? { "data-proposal-key": gc.proposalKey } : undefined;
                    if (tb.pending) {
                      // Nothing to grant on yet: the cell says what the role has (nothing, or what "*" gives) and what the card adds.
                      return (
                        <td key={tb.name} className={clsx("min-w-36", tint)} {...mark}>
                          <span className="slds-text-color_weak">{t(TIER_KEY[tier])}</span>
                          {gc && <GhostNote change={gc} table={tb.name} />}
                        </td>
                      );
                    }
                    if (inherited || tier === "custom") {
                      const shown = t(inherited ? "studio.build.access.level.inherited" : "studio.build.access.level.custom");
                      const label = tf("studio.build.access.matrix.cell", {
                        role: roleText,
                        table: tb.label,
                        level: t(inherited ? "studio.build.access.level.inheritedName" : "studio.build.access.level.customName"),
                      });
                      return (
                        <td key={tb.name} className={clsx("min-w-36", tint)} {...mark}>
                          <Link href={roleHref(role.id)} aria-label={label}>
                            {inherited ? `${t(TIER_KEY[tier])}, ${shown}` : shown}
                          </Link>
                          {gc && <GhostNote change={gc} table={tb.name} />}
                        </td>
                      );
                    }
                    const name = tf("studio.build.access.matrix.cell", { role: roleText, table: tb.label, level: t(TIER_KEY[tier]) });
                    return (
                      <td key={tb.name} className={clsx("min-w-36", tint)} {...mark}>
                        <Select
                          label={name}
                          labelHidden
                          aria-label={name}
                          placeholder={false}
                          value={tier}
                          className="pointer-coarse:min-h-11"
                          error={errors[key]}
                          options={TIERS.map((v) => ({ value: v, label: t(TIER_KEY[v]) }))}
                          onChange={(e) => {
                            const next = e.target.value as MatrixTier;
                            void guard(tf("studio.build.access.applyNow.matrix", { role: roleText, table: tb.label, level: t(TIER_KEY[next]) }), () => change(role, tb.name, next));
                          }}
                        />
                        {gc && <GhostNote change={gc} table={tb.name} />}
                      </td>
                    );
                  })}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </>
  );
}
