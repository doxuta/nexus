"use client";

// UpgradeItemList — the grouped items of one pack upgrade plan (specs/079 f7-nang-cap-ke-hoach/-ap): each group is a
// divider list; a row says what the item is and what the server found, offers a choice ONLY among the server's
// actions, and opens the three columns (old pack | yours | new pack).
import { useState } from "react";
import type { UpgradeItem } from "@/lib/api";
import { Button } from "@/components/slds/Button";
import { fill } from "@/components/slds/DataTable";
import { Select } from "@/components/slds/Select";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import {
  actionLabelKey, applyModeLabelKey, chosenAction, groupUpgradeItems, itemKeyOf, itemTypeLabelKey, mergePreview, offeredActions, reasonLabelKey, statusMeta, warningLabelKey,
} from "@/lib/packUpgrade";
import { StatusText } from "../StatusText";
import { CompareTable } from "./CompareTable";

const shortPaths = (p: string[]) => p.slice(0, 4).join(", ") + (p.length > 4 ? "…" : "");

function ItemRow({ it, choice, onChoose, disabled }: { it: UpgradeItem; choice: string; onChoose: (a: string) => void; disabled: boolean }) {
  const [open, setOpen] = useState(false);
  const meta = statusMeta(it.status);
  const typeKey = itemTypeLabelKey(it.itemType);
  const reasonKey = reasonLabelKey(it.reason);
  const modeKey = applyModeLabelKey(it.applyMode);
  const hasVersions = it.base !== undefined || it.theirs !== undefined || it.live !== undefined;
  const choices = offeredActions(it).filter((a) => a !== "none");
  const preview = mergePreview(it);
  return (
    <li className="slds-item">
      <div className="flex flex-wrap items-start gap-3">
        <div className="min-w-0 flex-1">
          <p className="break-all">
            <span className="slds-text-color_weak">{typeKey ? t(typeKey) : it.itemType}</span>{" "}
            <span className="font-semibold">{it.label || it.itemKey}</span>{" "}
            <StatusText tone={meta.tone}>{t(meta.labelKey)}</StatusText>
          </p>
          {it.renamedTo && <p className="slds-text-body_small slds-text-color_weak">{fill(t("studio.maintain.upgrade.renamed"), { name: it.renamedTo })}</p>}
          {it.theirsPaths.length > 0 && <p className="slds-text-body_small slds-text-color_weak slds-truncate" title={it.theirsPaths.join(", ")}>{shortPaths(it.theirsPaths)}</p>}
          {preview && (
            <p className="slds-text-body_small slds-text-color_weak">
              {fill(t("studio.maintain.upgrade.mergePreview"), { pack: shortPaths(preview.fromPack) || "-", mine: shortPaths(preview.mine) || "-" })}
            </p>
          )}
          {reasonKey && <p className="slds-text-body_small slds-text-color_weak">{t(reasonKey)}</p>}
          {modeKey && choices.length > 1 && <p className="slds-text-body_small slds-text-color_weak">{t(modeKey)}</p>}
          {it.warnings.filter((w) => w.code !== "SHARED_WITH_PACK").map((w) => {
            const k = warningLabelKey(w.code);
            return <p key={w.code} className={`slds-text-body_small ${w.level === "confirm" ? "slds-text-color_error" : "slds-text-color_weak"}`}>{k ? t(k) : w.text}</p>;
          })}
          {it.sharedWith.length > 0 && <p className="slds-text-body_small slds-text-color_weak">{fill(t("studio.maintain.upgrade.sharedWith"), { slugs: it.sharedWith.join(", ") })}</p>}
        </div>
        {choices.length > 1 ? (
          <Select labelHidden label={fill(t("studio.maintain.upgrade.choose"), { name: it.label || it.itemKey })} placeholder={false} value={choice}
            disabled={disabled} onChange={(e) => onChoose(e.target.value)}
            options={choices.map((a) => ({ value: a, label: t(actionLabelKey(a, it.status)) }))} />
        ) : choices.length === 1 ? (
          <span className="slds-text-color_weak">{t(actionLabelKey(choices[0], it.status))}</span>
        ) : null}
        {hasVersions && (
          <Button variant="neutral" aria-expanded={open} onClick={() => setOpen((v) => !v)}>
            {t(open ? "studio.maintain.upgrade.hideThree" : "studio.maintain.upgrade.show")}
          </Button>
        )}
      </div>
      {open && hasVersions && <div className="slds-m-top_small"><CompareTable item={it} /></div>}
    </li>
  );
}

/** Every group but the settled one (settled items are counted by the page, not listed). */
export function UpgradeItemList({ items, choices, disabled, onChoose }: {
  items: UpgradeItem[]; choices: Record<string, string>; disabled: boolean; onChoose: (key: string, action: string) => void;
}) {
  return (
    <>
      {groupUpgradeItems(items).filter((g) => g.group !== "settled").map((g) => (
        <section key={g.group} aria-label={t(`studio.maintain.upgrade.group.${g.group}` as I18nKey)} className="slds-m-top_medium">
          <h3 className="slds-text-heading_small">{`${t(`studio.maintain.upgrade.group.${g.group}` as I18nKey)} (${g.items.length})`}</h3>
          <ul className="slds-has-dividers_bottom">
            {g.items.map((it) => (
              <ItemRow key={itemKeyOf(it)} it={it} choice={chosenAction(it, choices)} disabled={disabled} onChoose={(a) => onChoose(itemKeyOf(it), a)} />
            ))}
          </ul>
        </section>
      ))}
    </>
  );
}
