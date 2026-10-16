"use client";

// ScreenBar (specs/075 FR-015, T020): the one row above the stage. Left: the status sentence (required fields still
// off the form, from `missingRequired`, or "Đã đủ"). Right, in order: screen picker · "Chưa lưu" · "Bỏ thay đổi" (only
// when dirty) · ONE brand button "Lưu thay đổi" · menu "…" ("Đưa màn về màn tự sinh" only when a saved screen exists).
// It owns no state: choosing another screen is the editor's call (it asks first when there are unsaved changes).
import { AlertTriangle, CheckCircle2, MoreHorizontal } from "lucide-react";
import { Button } from "@/components/slds/Button";
import { ButtonGroup } from "@/components/slds/ButtonGroup";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { Select } from "@/components/slds/Select";
import { StatusText } from "@/components/studio/StatusText";
import type { EntityMeta, FieldMeta } from "@/lib/api";
import { t, tf } from "@/lib/i18n";

export type ScreenMan = "form" | "ho-so" | "danh-sach";
const VIEWS = [
  ["form", "studio.build.screens.form"],
  ["ho-so", "studio.build.screens.detail"],
  ["danh-sach", "studio.build.screens.list"],
] as const;

export const screenLabel = (e: { name: string; label?: string }, man: ScreenMan): string =>
  tf(VIEWS.find((v) => v[0] === man)![1], { bang: e.label || e.name });

export interface ScreenBarProps {
  entities: EntityMeta[];
  bang: string;
  man: ScreenMan;
  /** Required fields not on the form; null while unknown. */
  missing: FieldMeta[] | null;
  dirty: boolean;
  saving: boolean;
  /** Writes are locked (maintenance): Lưu is off. */
  locked: boolean;
  canReset: boolean;
  /** The body below keeps its own save (the profile/list builders until F3 L2a/L2b replace them): only the picker shows. */
  bodyOwnsSave?: boolean;
  /** "Nháp" while the saved screen is a draft the customer has not seen (delivered project only); null = nothing. */
  draftLabel?: string | null;
  /** Delivered project: show the live/draft switch ("Xem bản đang chạy"); undefined hides it. */
  live?: boolean;
  onLive?: (v: boolean) => void;
  /** "Trên màn | Danh sách" switch (specs/075 L3); omitted = no switch. */
  mode?: "canvas" | "list";
  onMode?: (mode: "canvas" | "list") => void;
  onPick: (bang: string, man: ScreenMan) => void;
  onSave: () => void;
  onDiscard: () => void;
  onReset: () => void;
}

export function ScreenBar(p: ScreenBarProps) {
  const status =
    p.missing == null ? null : p.missing.length === 0 ? (
      <StatusText tone="success" className="inline-flex items-center gap-1">
        <CheckCircle2 aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
        {t("studio.build.screens.status.ok")}
      </StatusText>
    ) : (
      <StatusText tone="warn" className="inline-flex items-center gap-1">
        <AlertTriangle aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
        {tf("studio.build.screens.status.missing", { n: p.missing.length, names: p.missing.map((f) => f.label || f.name).join(", ") })}
      </StatusText>
    );
  return (
    <div role="group" aria-label={t("studio.build.screens.bar.label")} className="slds-grid slds-grid_vertical-align-center slds-wrap slds-m-bottom_small">
      <div className="slds-col slds-size_1-of-1 slds-large-size_1-of-2 slds-text-heading_small slds-m-bottom_x-small" aria-live="polite">{status}</div>
      <div className="slds-col slds-grid slds-grid_vertical-align-center slds-grid_align-end slds-wrap">
        <div className="slds-m-right_small">
          <Select
            label={t("studio.build.screens.pickerLabel")}
            labelHidden
            placeholder={false}
            value={`${p.bang}|${p.man}`}
            onChange={(e) => {
              const [b, m] = e.target.value.split("|");
              p.onPick(b, m as ScreenMan);
            }}
          >
            {p.entities.map((en) => (
              <optgroup key={en.name} label={en.label || en.name}>
                {VIEWS.map(([v]) => (
                  <option key={v} value={`${en.name}|${v}`}>{screenLabel(en, v)}</option>
                ))}
              </optgroup>
            ))}
          </Select>
        </div>
        {p.onLive && (
          <Button variant="neutral" className="slds-m-right_small" onClick={() => p.onLive!(!p.live)}>
            {p.live ? t("studio.build.screens.live.back") : t("studio.build.screens.live.view")}
          </Button>
        )}
        {p.onMode && (
          <ButtonGroup label={t("studio.build.screens.mode.label")} className="slds-m-right_small">
            {(["canvas", "list"] as const).map((m) => (
              <Button key={m} variant={p.mode === m ? "brand" : "neutral"} aria-pressed={p.mode === m} onClick={() => p.mode !== m && p.onMode?.(m)}>
                {t(m === "canvas" ? "studio.build.screens.mode.canvas" : "studio.build.screens.mode.list")}
              </Button>
            ))}
          </ButtonGroup>
        )}
        {p.draftLabel && <StatusText tone="warn" className="slds-m-right_small">{p.draftLabel}</StatusText>}
        {!p.bodyOwnsSave && p.dirty && <span className="slds-text-color_weak slds-m-right_small">{t("studio.build.screens.bar.unsaved")}</span>}
        {!p.bodyOwnsSave && p.dirty && (
          <Button variant="neutral" className="slds-m-right_x-small" disabled={p.saving} onClick={p.onDiscard}>{t("studio.build.screens.bar.discard")}</Button>
        )}
        {!p.bodyOwnsSave && (
          <Button variant="brand" disabled={!p.dirty || p.saving || p.locked} onClick={p.onSave}>
            {p.saving ? t("studio.build.screens.bar.saving") : t("studio.build.screens.bar.save")}
          </Button>
        )}
        {!p.bodyOwnsSave && p.canReset && (
          <Menu align="right" trigger={<ButtonIcon icon={MoreHorizontal} label={t("studio.build.screens.bar.more")} variant="border" />}>
            <MenuItem label={t("studio.build.screens.bar.reset")} onSelect={p.onReset} />
          </Menu>
        )}
      </div>
    </div>
  );
}
