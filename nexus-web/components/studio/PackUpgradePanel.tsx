"use client";

// PackUpgradePanel — "Xem nâng cấp" (../specs/069-pack-apply-upgrade): the 3-way plan for an installed
// pack against a newer version of it. Items are grouped by status (what needs a decision first), then
// by type; each shows where the pack and the builder changed it and, on demand, the three versions.
// L2a adds the apply step (L2b: "Luôn giữ" beside Giữ/Áp): pick an action per item (only among the ones the server offers, default =
// the server's), confirm, "Áp nâng cấp". The server recomputes the plan under its own lock, so a plan
// that moved comes back as 409 and the panel simply reloads it. The full "Bảo trì" screen is task
// FE-pack-apply-upgrade of the new Studio.
// SLDS: scoped-notifications · badges · divided lists · spinner · prompt.
import { useCallback, useEffect, useState } from "react";
import {
  ApiError, applyPackUpgrade, listPackUpgrades, planPackUpgrade,
  type PackSource, type UpgradeDecision, type UpgradeItem, type UpgradePlan, type UpgradeRun, type UpgradeStatus,
} from "@/lib/api";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { fill } from "@/components/slds/DataTable";
import { ModalClose, Prompt } from "@/components/slds/Modal";
import { Select } from "@/components/slds/Select";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { TYPE_LABELS } from "@/components/studio/PackDiffPanel";

type Theme = "success" | "warning" | "error" | undefined;

// Order = how urgently the builder must look: decisions first, settled states last (and unlisted).
const GROUPS: { status: UpgradeStatus; theme: Theme }[] = [
  { status: "conflict", theme: "warning" },
  { status: "conflict_no_base", theme: "warning" },
  { status: "conflict_deleted", theme: "warning" },
  { status: "update_safe", theme: "success" },
  { status: "added", theme: "success" },
  { status: "kept_custom", theme: undefined },
  { status: "protected", theme: undefined },
  { status: "deleted_by_builder", theme: undefined },
  { status: "removed_in_pack", theme: undefined },
  { status: "removed_in_pack_modified", theme: "warning" },
  { status: "undiffable", theme: undefined },
  { status: "ambiguous_name", theme: "error" },
  { status: "unsupported", theme: "error" },
  { status: "draft_pending", theme: undefined },
];

// "Settled" = nothing to decide (unchanged / in_sync): counted, not listed — hundreds of rows would bury the rest.
const SETTLED: UpgradeStatus[] = ["unchanged", "in_sync"];

const statusLabel = (s: string) => t(`page.packUpgrade.status.${s}` as I18nKey);
const KNOWN_REASONS = new Set([
  "ADAPTER_NOT_READY", "EXPORT_UNDIFFABLE", "AMBIGUOUS_NAME", "BASE_EXPECTED_ONLY", "FIELD_TYPE_CHANGE",
  "FIELD_UNIQUE_CHANGE", "NN_UPDATE_UNSUPPORTED", "ROLE_WILDCARD_REFUSED", "HARD_DELETE_KIND", "ROLE_NAME_RESERVED",
]);
// Server warnings the panel words itself (the server text stays the fallback for any other code).
// SHARED_WITH_PACK has its own line below, so it is not repeated here.
const KNOWN_WARNINGS = new Set(["GRANT_NARROWING", "FIELD_ACL_OPENING", "REQUIRED_ON_EXISTING_DATA", "NEW_RELATION_ON_EXISTING_TABLE", "NEW_HOOK_SECRET"]);

function Version({ title, value }: { title: string; value: unknown }) {
  return (
    <div className="min-w-0 flex-1">
      <h4 className="slds-text-title_caps">{title}</h4>
      {value === undefined || value === null
        ? <p className="slds-text-color_weak">{t("page.packUpgrade.absent")}</p>
        : <pre className="slds-text-body_small overflow-auto" style={{ maxHeight: "12rem" }}>{JSON.stringify(value, null, 2)}</pre>}
    </div>
  );
}

// keep_always writes only the flag on the baseline, but it is a change the builder confirms like the others.
const WRITE_ACTIONS = ["apply", "restore", "delete", "keep_always", "merge"];
const actionLabel = (a: string) => t(`page.packUpgrade.action.${a}` as I18nKey);

function ItemRow({ it, choice, onChoose, disabled }: { it: UpgradeItem; choice: string; onChoose: (a: string) => void; disabled: boolean }) {
  const [open, setOpen] = useState(false);
  const hasVersions = it.base !== undefined || it.theirs !== undefined || it.live !== undefined;
  const taken = it.theirsTakenPaths ?? [];
  const paths = it.theirsPaths.slice(0, 4).join(", ") + (it.theirsPaths.length > 4 ? "…" : "");
  return (
    <li className="slds-item">
      <div className="flex flex-wrap items-start gap-2">
        <span className="slds-text-color_weak">{TYPE_LABELS[it.itemType] ? t(TYPE_LABELS[it.itemType]) : it.itemType}</span>
        <div className="min-w-0 flex-1">
          <p className="break-all">
            {it.itemKey}
            {it.renamedTo && <span className="slds-text-color_weak">{` — ${fill(t("page.packUpgrade.renamedTo"), { name: it.renamedTo })}`}</span>}
          </p>
          {it.theirsPaths.length > 0 && (
            <p className="slds-text-body_small slds-text-color_weak slds-truncate" title={it.theirsPaths.join(", ")}>
              {fill(t("page.packUpgrade.pathsLabel"), { paths })}
            </p>
          )}
          {taken.length > 0 && (
            <p className="slds-text-body_small slds-text-color_weak slds-truncate" title={taken.join(", ")}>
              {fill(t("page.packUpgrade.takenPaths"), { paths: taken.slice(0, 4).join(", ") + (taken.length > 4 ? "…" : "") })}
            </p>
          )}
          {it.applyMode && it.actions.length > 1 && (
            <p className="slds-text-body_small slds-text-color_weak">{t(`page.packUpgrade.mode.${it.applyMode}` as I18nKey)}</p>
          )}
          {it.reason && (
            <p className="slds-text-body_small slds-text-color_weak">
              {KNOWN_REASONS.has(it.reason) ? t(`page.packUpgrade.reason.${it.reason}` as I18nKey) : it.reason}
            </p>
          )}
          {it.warnings.filter((w) => w.code !== "SHARED_WITH_PACK").map((w) => (
            <p key={w.code} className={`slds-text-body_small ${w.level === "confirm" ? "slds-text-color_error" : "slds-text-color_weak"}`}>
              {KNOWN_WARNINGS.has(w.code) ? t(`page.packUpgrade.warn.${w.code}` as I18nKey) : w.text}
            </p>
          ))}
          {it.sharedWith.length > 0 && (
            <p className="slds-text-body_small slds-text-color_weak">{fill(t("page.packUpgrade.sharedWith"), { slugs: it.sharedWith.join(", ") })}</p>
          )}
        </div>
        {it.actions.length > 1 && (
          <Select labelHidden label={`${t("page.packUpgrade.chooseFor")} ${it.itemKey}`} placeholder={false} value={choice}
            disabled={disabled} onChange={(e) => onChoose(e.target.value)}
            options={it.actions.map((a) => ({ value: a, label: actionLabel(a) }))} />
        )}
        {hasVersions && (
          <Button aria-expanded={open} onClick={() => setOpen((v) => !v)}>
            {t(open ? "page.packUpgrade.hideVersions" : "page.packUpgrade.showVersions")}
          </Button>
        )}
      </div>
      {open && hasVersions && (
        <div className="slds-box slds-box_x-small mt-2 flex flex-wrap gap-3">
          <Version title={t("page.packUpgrade.base")} value={it.base} />
          <Version title={t("page.packUpgrade.live")} value={it.live} />
          <Version title={t("page.packUpgrade.theirs")} value={it.theirs} />
          {it.merged !== undefined && <Version title={t("page.packUpgrade.merged")} value={it.merged} />}
        </div>
      )}
    </li>
  );
}

export function PackUpgradePanel({ slug, source, onRun }: { slug: string; source: PackSource; onRun?: (r: UpgradeRun) => void }) {
  // Result is tagged with the key it was fetched for, so a slug/source change shows the spinner
  // again without calling setState synchronously inside the effect.
  const sourceKey = JSON.stringify(source);
  const [nonce, setNonce] = useState(0);
  const key = `${slug}|${sourceKey}|${nonce}`;
  const [res, setRes] = useState<{ key: string; plan?: UpgradePlan; err?: string } | null>(null);
  const [choices, setChoices] = useState<{ key: string; byItem: Record<string, string> }>({ key: "", byItem: {} });
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [tick, setTick] = useState(false); // "Áp ngay cho khách" (L7)
  const [applying, setApplying] = useState(false);
  const [run, setRun] = useState<UpgradeRun | null>(null);
  const [note, setNote] = useState<{ theme: "warning" | "error" | "success"; text: string } | null>(null);
  useEffect(() => {
    let alive = true;
    planPackUpgrade(slug, JSON.parse(sourceKey) as PackSource)
      .then((p) => { if (alive) setRes({ key, plan: p }); })
      .catch((e) => { if (alive) setRes({ key, err: e instanceof ApiError ? e.message : t("page.packUpgrade.failed") }); });
    return () => { alive = false; };
  }, [slug, sourceKey, key]);
  const cur = res?.key === key ? res : null;
  const plan = cur?.plan ?? null;
  const err = cur?.err ?? "";
  // Choices are per plan: a reloaded plan starts from the server's defaults again.
  const byItem = choices.key === plan?.planHash ? choices.byItem : {};
  const chosen = (it: UpgradeItem) => byItem[it.itemType + "/" + it.itemKey] ?? it.defaultAction;

  const reload = useCallback(() => { setRes(null); setNonce((n) => n + 1); }, []);

  if (err) return <ScopedNotification theme="error">{err}</ScopedNotification>;
  if (!plan) {
    return (
      <div className="flex items-center gap-2">
        <Spinner size="x-small" inline />
        <span className="slds-text-color_weak">{t("page.packUpgrade.loading")}</span>
      </div>
    );
  }

  const writes = plan.items.filter((i) => WRITE_ACTIONS.includes(chosen(i)));
  const adoptable = plan.items.filter((i) => i.adopt).length;
  const canApply = !applying && writes.length + adoptable > 0;
  // L7: a change that reaches a delivered customer at once, with no trace, needs the builder's tick.
  const needsTick = plan.delivered && writes.some((i) => i.applyMode === "immediate_untraced" && chosen(i) !== "keep_always");
  const confirmWarnings = writes.filter((i) => chosen(i) !== "keep_always").flatMap((i) => i.warnings.filter((w) => w.level === "confirm").map((w) => `${i.itemKey}: ${w.text}`));

  async function doApply() {
    if (!plan) return;
    setConfirmOpen(false);
    setApplying(true);
    setNote(null);
    // Explicit decision for every item that offered a choice, so what the builder saw is what runs.
    const decisions: UpgradeDecision[] = plan.items
      .filter((i) => i.actions.length > 0)
      .map((i) => ({ itemType: i.itemType, itemKey: i.itemKey, action: chosen(i) }));
    try {
      const r = await (needsTick
        ? applyPackUpgrade(slug, source, plan.planHash, decisions, { confirmImmediate: tick })
        : applyPackUpgrade(slug, source, plan.planHash, decisions));
      setRun(r);
      onRun?.(r);
      const drafted = r.counts?.drafted ?? 0;
      setNote({
        theme: r.status === "done" ? "success" : "warning",
        text: drafted > 0 ? fill(t("page.packUpgrade.drafted"), { n: drafted }) : t(`page.packUpgrade.run.${r.status}` as I18nKey),
      });
      setTick(false);
      reload();
    } catch (e) {
      if (e instanceof ApiError && e.code === "PACK_UPGRADE_PLAN_STALE") {
        setNote({ theme: "warning", text: t("page.packUpgrade.stale") });
        reload();
      } else if (e instanceof ApiError && e.code === "PACK_UPGRADE_IN_PROGRESS") {
        setNote({ theme: "warning", text: fill(t("page.packUpgrade.inProgress"), { at: String(e.details.startedAt ?? "") }) });
      } else if (e instanceof ApiError && e.code === "PACK_UPGRADE_OPEN_WORKSPACE") {
        setNote({ theme: "warning", text: t("page.packUpgrade.openWorkspace") });
      } else if (e instanceof ApiError && e.code === "PACK_UPGRADE_NOTHING_TO_APPLY") {
        setNote({ theme: "warning", text: t("page.packUpgrade.nothingToApply") });
        reload();
      } else if (e instanceof ApiError) {
        setNote({ theme: "error", text: e.message });
      } else {
        // The connection dropped but the server keeps running: the history is the source of truth.
        try {
          const [last] = await listPackUpgrades(slug, 1);
          if (last) setRun(last);
          setNote({ theme: "warning", text: t("page.packUpgrade.networkLost") });
        } catch {
          setNote({ theme: "error", text: t("page.packUpgrade.failed") });
        }
        reload();
      }
    } finally {
      setApplying(false);
    }
  }

  const settled = plan.items.filter((i) => SETTLED.includes(i.status)).length;
  const shown = GROUPS.map((g) => ({ ...g, items: plan.items.filter((i) => i.status === g.status) })).filter((g) => g.items.length > 0);
  return (
    <div className="flex flex-col gap-3">
      <p className="font-semibold">{fill(t("page.packUpgrade.range"), { from: plan.fromVersions.join(", v"), to: plan.toVersion })}</p>
      {plan.untracked && <ScopedNotification theme="warning">{t("page.packUpgrade.untracked")}</ScopedNotification>}
      {plan.warnings.map((w) => <ScopedNotification key={w} theme="warning">{w}</ScopedNotification>)}
      {note && <ScopedNotification theme={note.theme}>{note.text}</ScopedNotification>}
      {run && (
        <section aria-label={t("page.packUpgrade.runResult")}>
          <h3 className="slds-text-title_caps">{t("page.packUpgrade.runResult")}</h3>
          <ul className="slds-has-dividers_bottom-space">
            {run.results.map((r) => (
              <li key={r.itemType + "/" + r.itemKey} className="slds-item">
                <Badge theme={r.outcome === "failed" || r.outcome === "not_run" ? "error" : r.outcome === "blocked" ? "warning" : "success"}>{t(`page.packUpgrade.outcome.${r.outcome}` as I18nKey)}</Badge>{" "}
                <span className="break-all">{r.itemKey}</span>
                {r.message && <span className="slds-text-body_small slds-text-color_weak">{` — ${r.message}`}</span>}
              </li>
            ))}
          </ul>
        </section>
      )}
      {shown.length === 0 && <p>{t("page.packUpgrade.nothingToDecide")}</p>}
      {shown.map((g) => {
        const byType = new Map<string, UpgradeItem[]>();
        for (const it of g.items) byType.set(it.itemType, [...(byType.get(it.itemType) ?? []), it]);
        return (
          <section key={g.status} aria-label={statusLabel(g.status)}>
            <h3 className="slds-text-title_caps flex items-center gap-2">
              <Badge theme={g.theme}>{statusLabel(g.status)}</Badge>
              <span>{g.items.length}</span>
            </h3>
            {[...byType.entries()].map(([type, items]) => (
              <ul key={type} className="slds-has-dividers_bottom-space">
                {items.map((it) => (
                  <ItemRow key={type + "/" + it.itemKey} it={it} choice={chosen(it)} disabled={applying}
                    onChoose={(a) => setChoices({ key: plan.planHash, byItem: { ...byItem, [it.itemType + "/" + it.itemKey]: a } })} />
                ))}
              </ul>
            ))}
          </section>
        );
      })}
      {settled > 0 && <p className="slds-text-body_small slds-text-color_weak">{fill(t("page.packUpgrade.settled"), { n: settled })}</p>}
      <div className="flex flex-wrap items-center gap-3">
        <Button variant="brand" disabled={!canApply} onClick={() => setConfirmOpen(true)}>
          {fill(t("page.packUpgrade.apply"), { n: writes.length })}
        </Button>
        {adoptable > 0 && <span className="slds-text-body_small slds-text-color_weak">{fill(t("page.packUpgrade.adoptable"), { n: adoptable })}</span>}
        {applying && <Spinner size="x-small" inline />}
      </div>
      {confirmOpen && (
        <Prompt open onOpenChange={(o) => { if (!o) setConfirmOpen(false); }} title={t("page.packUpgrade.confirmTitle")}
          footer={<>
            <ModalClose asChild><Button>{t("page.packUpgrade.cancel")}</Button></ModalClose>
            <Button variant="brand" disabled={needsTick && !tick} onClick={doApply}>{t("page.packUpgrade.confirmGo")}</Button>
          </>}>
          <p>{fill(t("page.packUpgrade.confirmBody"), { n: writes.length })}</p>
          <ul className="slds-list_dotted">
            {writes.map((i) => <li key={i.itemType + "/" + i.itemKey}>{`${actionLabel(chosen(i))}: ${i.itemKey}`}</li>)}
          </ul>
          {needsTick && (
            <Checkbox label={t("page.packUpgrade.immediateTick")} checked={tick} onChange={(e) => setTick(e.target.checked)}
              help={t("page.packUpgrade.immediateHint")} />
          )}
          {confirmWarnings.length > 0 && (
            <ScopedNotification theme="warning">
              {confirmWarnings.map((w) => <p key={w}>{w}</p>)}
            </ScopedNotification>
          )}
        </Prompt>
      )}
    </div>
  );
}
