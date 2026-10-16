"use client";

// UpgradePage — "Nâng cấp một gói" (bao-tri/nang-cap/:id; specs/079 f7-nang-cap-ke-hoach/-ap/-giu-tep/-gop/-nhap,
// specs/069 FE-pack-apply-upgrade). Reads the 3-way plan of one installed pack against the pack version shipped
// with Nexus, shows what needs a decision first, lets the builder choose per item among the actions the server
// offers (Giữ · Luôn giữ · Áp · Khôi phục · Xoá · Gộp), then applies it. For a delivered project the same plan says
// per item whether a change goes into the draft or reaches the customer at once (applyMode); an immediate,
// untraced change needs a tick. The plan is never cached (it must be recomputed on every visit); the server
// recomputes it under its own lock on apply, so a plan that moved comes back 409 and the page reloads it.
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import {
  ApiError, applyPackUpgrade, listPackUpgrades, planPackUpgrade,
  type PackSource, type UpgradePlan, type UpgradeRun,
} from "@/lib/api";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { fill } from "@/components/slds/DataTable";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { isEngineMissing } from "@/lib/engineGap";
import { maintainErrorText } from "@/lib/maintainErrors";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import {
  actionLabelKey, chosenAction, countWrites, decisionsFrom, groupUpgradeItems, itemKeyOf,
  needsImmediateTick, outcomeLabelKey, warningLabelKey, type UpgradeGroup,
} from "@/lib/packUpgrade";
import { InlineConfirm } from "../InlineConfirm";
import { StatusText, type StatusTone } from "../StatusText";
import { useProjectQuery } from "../useProjectQuery";
import { UpgradeApplyBar } from "./UpgradeApplyBar";
import { UpgradeItemList } from "./UpgradeItemList";

// maintainErrorText fills {startedAt} and falls back to errorMessage when there is no studio.maintain.err.<CODE> key.
const sentence = maintainErrorText;
const OUTCOME_TONE: Record<string, StatusTone> = { failed: "error", not_run: "error", blocked: "warn" };
const RUN_TONE: Record<string, StatusTone> = { done: "success", partial: "warn", failed: "error", interrupted: "warn", running: "weak" };
function RunResults({ run }: { run: UpgradeRun }) {
  return (
    <ul className="slds-has-dividers_bottom">
      {run.results.map((r) => (
        <li key={r.itemType + "/" + r.itemKey} className="slds-item">
          <StatusText tone={OUTCOME_TONE[r.outcome] ?? "success"}>{t(outcomeLabelKey(r.outcome))}</StatusText>{" "}
          <span className="break-all">{r.itemKey}</span>
          {r.message && <span className="slds-text-body_small slds-text-color_weak">{` · ${r.message}`}</span>}
        </li>
      ))}
    </ul>
  );
}

function History({ project, pack, refresh }: { project: string; pack: string; refresh: number }) {
  const runs = useProjectQuery(project, ["upgrades", pack, refresh], () => listPackUpgrades(pack, 10));
  const [open, setOpen] = useState<string | null>(null);
  return (
    <section aria-label={t("studio.maintain.upgrade.history")} className="slds-m-top_large">
      <h3 className="slds-text-heading_small">{t("studio.maintain.upgrade.history")}</h3>
      {runs.isPending ? <Spinner size="x-small" inline />
        : runs.isError ? <p className="slds-text-color_weak">{t("studio.maintain.upgrade.historyFailed")}</p>
        : (runs.data ?? []).length === 0 ? <p className="slds-text-color_weak">{t("studio.maintain.upgrade.historyNone")}</p>
        : (
          <ul className="slds-has-dividers_bottom">
            {(runs.data ?? []).map((r) => (
              <li key={r.id} className="slds-item">
                <div className="flex flex-wrap items-center gap-3">
                  <span className="font-semibold">{fill(t("studio.maintain.upgrade.historyLine"), { from: r.fromVersions, to: r.toVersion })}</span>
                  <StatusText tone={RUN_TONE[r.status] ?? "weak"}>{t(`studio.maintain.upgrade.runStatus.${r.status}` as I18nKey)}</StatusText>
                  <span className="slds-text-color_weak">{new Date(r.startedAt).toLocaleString("vi-VN")}</span>
                  <Button className="ml-auto" variant="neutral" aria-expanded={open === r.id} onClick={() => setOpen(open === r.id ? null : r.id)}>
                    {t(open === r.id ? "studio.maintain.upgrade.historyHide" : "studio.maintain.upgrade.historyShow")}
                  </Button>
                </div>
                {open === r.id && <div className="slds-m-top_small"><RunResults run={r} /></div>}
              </li>
            ))}
          </ul>
        )}
    </section>
  );
}

/** `source` defaults to the pack shipped with Nexus; "Nâng cấp bằng tệp" passes the parsed file (kept in memory only). */
export function UpgradePage({ project, pack, source = { builtin: pack } }: { project: string; pack: string; source?: PackSource }) {
  const qc = useQueryClient();
  const plan = useProjectQuery<UpgradePlan>(project, ["upgrade-plan", pack, "builtin" in source ? "builtin" : "file"], () => planPackUpgrade(pack, source), { fresh: true, refetchOnWindowFocus: false });
  const [choices, setChoices] = useState<{ hash: string; byItem: Record<string, string> }>({ hash: "", byItem: {} });
  const [confirmOpen, setConfirmOpen] = useState(false);
  // Focus returns to the apply button once the inline confirm closes (it would fall to <body> otherwise).
  const wasOpen = useRef(false);
  useEffect(() => {
    if (wasOpen.current && !confirmOpen) document.querySelector<HTMLElement>('[data-testid="upgrade-apply-bar"] button:last-of-type')?.focus();
    wasOpen.current = confirmOpen;
  }, [confirmOpen]);
  const [tick, setTick] = useState(false);
  const [applying, setApplying] = useState(false);
  const [run, setRun] = useState<UpgradeRun | null>(null);
  const [note, setNote] = useState<{ theme: "warning" | "error" | "success"; text: string } | null>(null);
  const [histNonce, setHistNonce] = useState(0);

  const p = plan.data;
  // Choices belong to one plan: a reloaded plan starts from the server's defaults again.
  const byItem = p && choices.hash === p.planHash ? choices.byItem : {};

  if (plan.isPending) {
    return (
      <div className="slds-p-around_medium flex items-center gap-2">
        <Spinner size="x-small" inline />
        <span className="slds-text-color_weak">{t("studio.maintain.upgrade.loading")}</span>
      </div>
    );
  }
  if (plan.isError && isEngineMissing(plan.error)) {
    return <div className="slds-p-around_medium"><ScopedNotification theme="info">{t("studio.maintain.upgrade.engineMissing")}</ScopedNotification></div>;
  }
  if (plan.isError || !p) {
    return (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{plan.error ? sentence(plan.error) : t("studio.maintain.upgrade.loadFailed")}</ScopedNotification>
        <div className="slds-m-top_small">
          <Button variant="neutral" onClick={() => void plan.refetch()}>{t("studio.maintain.upgrade.retry")}</Button>
        </div>
      </div>
    );
  }

  const groups = groupUpgradeItems(p.items);
  const settled = groups.find((g) => g.group === "settled")?.items.length ?? 0;
  const count = (g: UpgradeGroup) => groups.find((x) => x.group === g)?.items.length ?? 0;
  const { writes, adoptable } = countWrites(p.items, byItem);
  const needsTick = needsImmediateTick(p.items, byItem, p.delivered);
  const written = p.items.filter((i) => countWrites([i], byItem).writes > 0);
  const confirmWarnings = written.filter((i) => chosenAction(i, byItem) !== "keep_always")
    .flatMap((i) => i.warnings.filter((w) => w.level === "confirm").map((w) => `${i.label || i.itemKey}: ${(warningLabelKey(w.code) ? t(warningLabelKey(w.code)!) : w.text)}`));

  const reload = () => { void plan.refetch(); setHistNonce((n) => n + 1); };

  async function doApply() {
    if (!p) return;
    setApplying(true);
    setNote(null);
    try {
      const r = await applyPackUpgrade(pack, source, p.planHash, decisionsFrom(p.items, byItem), needsTick ? { confirmImmediate: tick } : {});
      setRun(r);
      setConfirmOpen(false);
      setTick(false);
      const drafted = r.counts?.drafted ?? 0;
      const lag = r.replicaLagSeconds ? ` ${fill(t("studio.maintain.upgrade.lag"), { n: String(r.replicaLagSeconds) })}` : "";
      setNote({
        theme: r.status === "done" ? "success" : "warning",
        text: (drafted > 0 ? fill(t("studio.maintain.upgrade.drafted"), { n: String(drafted) }) : t(`studio.maintain.upgrade.run.${r.status}` as I18nKey)) + lag,
      });
      reload();
      void qc.invalidateQueries();
    } catch (e) {
      setConfirmOpen(false);
      if (e instanceof ApiError) {
        setNote({ theme: "warning", text: sentence(e) });
        if (e.code === "PACK_UPGRADE_PLAN_STALE" || e.code === "PACK_UPGRADE_NOTHING_TO_APPLY") reload();
      } else {
        // The connection dropped but the server keeps running: the history is the source of truth.
        try {
          const [last] = await listPackUpgrades(pack, 1);
          if (last) setRun(last);
          setNote({ theme: "warning", text: t("studio.maintain.upgrade.networkLost") });
        } catch {
          setNote({ theme: "error", text: t("studio.maintain.upgrade.loadFailed") });
        }
        reload();
      }
    } finally {
      setApplying(false);
    }
  }

  return (
    <div className="slds-p-around_medium">
      <h2 className="slds-text-heading_medium">
        {fill(t("studio.maintain.upgrade.title"), { name: p.slug, from: `v${p.fromVersions.join(", v")}`, to: `v${p.toVersion}` })}
      </h2>
      {p.items.length > 0 && groups.length === 1 && groups[0].group === "settled" ? (
        <p className="nx-lede slds-m-top_x-small">{t("studio.maintain.upgrade.nothing")}</p>
      ) : (
        <p className="nx-lede slds-m-top_x-small">
          {fill(t("studio.maintain.upgrade.lede"), { decide: String(count("decide")), safe: String(count("safe")), custom: String(count("custom")), settled: String(settled) })}
        </p>
      )}
      {p.untracked && <ScopedNotification theme="warning">{t("studio.maintain.upgrade.untracked")}</ScopedNotification>}
      {p.warnings.map((w) => <ScopedNotification key={w} theme="warning">{w}</ScopedNotification>)}
      {note && <ScopedNotification theme={note.theme}>{note.text}</ScopedNotification>}
      {run && (
        <section aria-label={t("studio.maintain.upgrade.result")} className="slds-m-top_medium">
          <h3 className="slds-text-heading_small">{t("studio.maintain.upgrade.result")}</h3>
          <RunResults run={run} />
        </section>
      )}
      <UpgradeItemList items={p.items} choices={byItem} disabled={applying}
        onChoose={(key, a) => { setTick(false); setChoices({ hash: p.planHash, byItem: { ...byItem, [key]: a } }); }} />
      {settled > 0 && <p className="slds-text-body_small slds-text-color_weak slds-m-top_medium">{fill(t("studio.maintain.upgrade.settled"), { n: String(settled) })}</p>}

      {confirmOpen && (
        <div className="slds-m-top_medium">
          <InlineConfirm tone="warning" message={fill(t("studio.maintain.upgrade.confirmMsg"), { name: p.slug, n: String(writes) })}
            confirmLabel={t("studio.maintain.upgrade.confirmGo")} busy={applying} confirmDisabled={needsTick && !tick}
            onConfirm={() => void doApply()} onCancel={() => { setConfirmOpen(false); setTick(false); }}>
            <ul className="slds-list_dotted">
              {written.map((i) => <li key={itemKeyOf(i)}>{`${t(actionLabelKey(chosenAction(i, byItem), i.status))}: ${i.label || i.itemKey}`}</li>)}
            </ul>
            {needsTick && <Checkbox label={t("studio.maintain.upgrade.tick")} help={t("studio.maintain.upgrade.tickHint")} checked={tick} onChange={(e) => setTick(e.target.checked)} />}
            {confirmWarnings.map((w) => <p key={w} className="slds-text-color_error slds-text-body_small">{w}</p>)}
          </InlineConfirm>
        </div>
      )}
      {!confirmOpen && (
        <UpgradeApplyBar writes={writes} adoptable={adoptable} applying={applying} onApply={() => setConfirmOpen(true)}
          onReset={() => { setChoices({ hash: "", byItem: {} }); setTick(false); }} />
      )}
      <History project={project} pack={pack} refresh={histNonce} />
    </div>
  );
}
