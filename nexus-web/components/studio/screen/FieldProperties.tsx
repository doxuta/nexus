"use client";

// Right column of the form editor (specs/075 US1, L1a part): properties of the selected cell — the label the customer
// sees (this screen only), Lên/Xuống/Sang khung, Bỏ khỏi form — or, for a field that is not on the form yet
// (`?truong=<mã>`), the "Đặt lên form" button. Nothing selected = one hint line.
import { useState, type Ref } from "react";
import Link from "next/link";
import { Button } from "@/components/slds/Button";
import { Input } from "@/components/slds/Input";
import { Select } from "@/components/slds/Select";
import { Checkbox } from "@/components/slds/Checkbox";
import { Toggle } from "@/components/slds/Toggle";
import { Textarea } from "@/components/slds/Textarea";
import { StatusText, draftStateLabel } from "@/components/studio/StatusText";
import type { FieldMeta, VisibleWhen } from "@/lib/api";
import { HELP_TEXT_MAX, type DraftFieldCell, type DraftSection } from "@/lib/layoutDraft";
import { t, tf } from "@/lib/i18n";
import { typeLabelOf } from "./FormCanvas";
import { VisibleWhenRow } from "./VisibleWhenRow";

export interface FieldPropertiesProps {
  /** Selected cell on the form, with its field. */
  cell: DraftFieldCell | null;
  field: FieldMeta | null;
  /** Selected field that is NOT on the form. */
  unplaced: FieldMeta | null;
  sections: DraftSection[];
  /** Every field of the table (controller choices for "Chỉ hiện khi"). */
  fields: FieldMeta[];
  /** Fields whose condition reads the selected one (removing it clears them). */
  dependents: string[];
  /** Server sentence for LAYOUT_VISIBLE_WHEN_INVALID. */
  whenError?: string | null;
  sectionId: string | null;
  canUp: boolean;
  canDown: boolean;
  /** Set when the last "Bỏ khỏi form" was refused (required field). */
  removeBlocked: boolean;
  locked: boolean;
  /** Record page (not the form): no required/readonly notions, wording says "hồ sơ". */
  detail?: boolean;
  /** "Bắt buộc nhập" as it will be saved (the field's flag with the pending edit applied). */
  requiredOn?: boolean;
  tableHref: string;
  labelRef: Ref<HTMLInputElement>;
  onLabel: (label: string) => void;
  onUp: () => void;
  onDown: () => void;
  onToSection: (sectionId: string) => void;
  onRemove: () => void;
  onRequired?: (on: boolean) => void;
  onWhen: (vw: VisibleWhen | undefined) => void;
  onPlace: () => void;
  onDone: () => void;
  /** "Tuỳ chọn riêng của màn này": help text, width, read-only, required here. */
  onOption: (patch: Partial<Pick<DraftFieldCell, "helpText" | "readonly" | "required" | "colspan" | "fullWidth">>) => void;
  /** Discards the pending delete of the selected field ("Hoàn tác"). */
  onUndoDelete?: () => void;
}

/** Draft labels of one field — drawn only where the read carried them (never guessed). */
export function FieldDraftNote({ field, onUndo }: { field: FieldMeta; onUndo?: () => void }) {
  const state = field.draftState ? draftStateLabel(field.draftState) : null;
  const required = field.pendingRequired ? draftStateLabel("requiredOnPublish") : null;
  if (!state && !required) return null;
  return (
    <p className="slds-text-body_small slds-m-bottom_small">
      {state && field.draftState !== "edited" && <StatusText tone={state.tone}>{state.label}</StatusText>}
      {required && <StatusText tone={required.tone} className={field.draftState && field.draftState !== "edited" ? "slds-m-left_small" : undefined}>{required.label}</StatusText>}
      {field.draftState === "deleted" && onUndo && (
        <Button variant="base" className="slds-m-left_small" onClick={onUndo}>{t("studio.build.screens.draft.undo")}</Button>
      )}
    </p>
  );
}

export function FieldProperties(props: FieldPropertiesProps) {
  // the ref prop stays out of `p` (React Compiler lint reads any object holding a ref as a ref)
  const { labelRef, ...p } = props;
  const [confirming, setConfirming] = useState<string | null>(null); // name of the cell whose removal awaits a yes
  const target = p.field ?? p.unplaced;
  if (!target) return <p className="slds-text-color_weak">{t(p.detail ? "studio.build.screens.record.none" : "studio.build.screens.field.none")}</p>;
  const name = target.label || target.name;
  return (
    <section aria-labelledby="nx-field-title">
      <h3 id="nx-field-title" className="slds-text-heading_small slds-m-bottom_x-small">{name}</h3>
      <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_small">
        {t("studio.build.screens.field.kind")}: {typeLabelOf(target)}
        {(p.requiredOn ?? target.isRequired) && !p.detail ? ` · ${t("studio.build.screens.field.required")}` : ""}
      </p>
      <FieldDraftNote field={target} onUndo={p.onUndoDelete} />
      {p.unplaced && !p.cell ? (
        <>
          <p className="slds-m-bottom_small">{t(p.detail ? "studio.build.screens.record.unplacedNote" : "studio.build.screens.field.unplacedNote")}</p>
          <Button variant="brand" disabled={p.locked} onClick={p.onPlace}>{t(p.detail ? "studio.build.screens.record.place" : "studio.build.screens.field.place")}</Button>
          {!p.detail && p.onRequired && (
            <Toggle
              className="slds-m-top_small"
              label={t("studio.build.screens.field.requiredSwitch")}
              checked={!!p.requiredOn}
              disabled={p.locked}
              help={t("studio.build.screens.field.requiredHint")}
              onChange={(e) => p.onRequired?.(e.target.checked)}
            />
          )}
        </>
      ) : (
        p.cell && (
          <>
            <Input
              ref={labelRef}
              label={t("studio.build.screens.field.label")}
              value={p.cell.labelOverride ?? ""}
              placeholder={target.label || target.name}
              onChange={(e) => p.onLabel(e.target.value)}
              help={t("studio.build.screens.field.labelNote")}
            />
            <p className="slds-text-body_small slds-m-bottom_small">
              <Link className="slds-text-link" href={p.tableHref}>{t("studio.build.screens.field.labelLink")}</Link>
            </p>
            {p.field && !p.detail && p.onRequired && (
              <Toggle
                className="slds-m-top_small"
                label={t("studio.build.screens.field.requiredSwitch")}
                checked={!!p.requiredOn}
                disabled={p.locked || (!p.requiredOn && !!p.cell.visibleWhen)}
                help={!p.requiredOn && p.cell.visibleWhen ? t("studio.build.screens.field.requiredLocked") : t("studio.build.screens.field.requiredHint")}
                onChange={(e) => p.onRequired?.(e.target.checked)}
              />
            )}
            {p.field && !p.detail && (
              <VisibleWhenRow
                key={p.cell.id}
                cell={p.cell}
                field={p.field}
                fields={p.fields}
                draft={p.sections}
                disabled={p.locked}
                error={p.whenError}
                onChange={p.onWhen}
              />
            )}
            <div className="slds-button-group slds-m-bottom_small" role="group">
              <Button variant="neutral" disabled={!p.canUp} onClick={p.onUp}>{t("studio.build.screens.field.up")}</Button>
              <Button variant="neutral" disabled={!p.canDown} onClick={p.onDown}>{t("studio.build.screens.field.down")}</Button>
            </div>
            {p.sections.length > 1 && p.sectionId && (
              <Select
                label={t("studio.build.screens.field.toSection")}
                placeholder={false}
                value={p.sectionId}
                onChange={(e) => p.onToSection(e.target.value)}
              >
                {p.sections.map((s) => (
                  <option key={s.id} value={s.id}>{s.title.trim() || t("studio.build.screens.stage.sectionUntitled")}</option>
                ))}
              </Select>
            )}
            {!p.detail && <CellOptions cell={p.cell} columns={p.sections.find((x) => x.id === p.sectionId)?.columns ?? 1} onOption={p.onOption} />}
            <div className="slds-m-top_small">
              <Button variant="text-destructive" onClick={() => (p.dependents.length > 0 ? setConfirming(p.cell!.field) : p.onRemove())}>
                {t(p.detail ? "studio.build.screens.record.remove" : "studio.build.screens.field.remove")}
              </Button>
            </div>
            {confirming === p.cell.field && p.dependents.length > 0 && (
              <div role="alert" className="slds-m-top_x-small">
                <p className="slds-m-bottom_x-small">{tf("studio.build.screens.visibleWhen.dependents", { n: p.dependents.length, names: p.dependents.join(", ") })}</p>
                <Button variant="destructive" onClick={() => { setConfirming(null); p.onRemove(); }}>{t("studio.build.screens.visibleWhen.confirmRemove")}</Button>{" "}
                <Button variant="neutral" onClick={() => setConfirming(null)}>{t("studio.build.screens.visibleWhen.cancel")}</Button>
              </div>
            )}
            {p.removeBlocked && (
              <p role="alert" className="slds-text-color_error slds-m-top_x-small">{t("studio.build.screens.field.requiredStay")}</p>
            )}
          </>
        )
      )}
      <div className="slds-m-top_medium">
        <Button variant="base" onClick={p.onDone}>{t("studio.build.screens.field.done")}</Button>
      </div>
    </section>
  );
}

function CellOptions({ cell, columns, onOption }: { cell: DraftFieldCell; columns: number; onOption: FieldPropertiesProps["onOption"] }) {
  const width = cell.fullWidth ? "full" : cell.colspan ? String(Math.min(cell.colspan, columns)) : "auto";
  return (
    <fieldset className="slds-m-top_medium">
      <legend className="slds-text-title_caps slds-m-bottom_x-small">{t("studio.build.screens.field.options")}</legend>
      <Textarea
        label={t("studio.build.screens.field.help")}
        maxLength={HELP_TEXT_MAX}
        value={cell.helpText ?? ""}
        onChange={(e) => onOption({ helpText: e.target.value })}
        help={tf("studio.build.screens.field.helpCount", { n: (cell.helpText ?? "").length, max: HELP_TEXT_MAX })}
      />
      <Select
        label={t("studio.build.screens.field.width")}
        placeholder={false}
        value={width}
        onChange={(e) => {
          const v = e.target.value;
          onOption(v === "full" ? { fullWidth: true, colspan: undefined } : v === "auto" ? { fullWidth: undefined, colspan: undefined } : { colspan: Number(v), fullWidth: undefined });
        }}
      >
        <option value="auto">{t("studio.build.screens.field.widthAuto")}</option>
        {Array.from({ length: columns }, (_, i) => i + 1).map((n) => (
          <option key={n} value={n}>{tf("studio.build.screens.field.widthCols", { n })}</option>
        ))}
        <option value="full">{t("studio.build.screens.field.widthFull")}</option>
      </Select>
      <Checkbox label={t("studio.build.screens.field.readonly")} checked={!!cell.readonly} onChange={(e) => onOption({ readonly: e.target.checked || undefined })} />
      <Checkbox
        label={t("studio.build.screens.field.requiredHere")}
        help={t("studio.build.screens.field.requiredHereNote")}
        checked={!!cell.required}
        onChange={(e) => onOption({ required: e.target.checked || undefined })}
      />
    </fieldset>
  );
}
