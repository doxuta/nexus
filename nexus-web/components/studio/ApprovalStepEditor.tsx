"use client";

// One step of an approval process (specs/043 FE-BPM-PROPS-a, T222/T223/T225): who holds it (a person, a
// role, a team with a chair strategy, a work queue, the person in a record field, the submitter, or
// several people with a completion rule), its deadline clock, and where a rejection goes. A step is a
// SLDS `card` inside the process's `progress-indicator/vertical` list; the pure rules (kinds, wire shape,
// validation) live in lib/processModel.
import { ArrowDown, ArrowUp, Plus, Trash2, User, Users } from "lucide-react";
import type { ApprovalStep } from "@/lib/api";
import { OWNER_FIELD, holderKind, rejectTargets, setHolderKind, type HolderKind } from "@/lib/processModel";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Combobox, type ComboboxOption } from "@/components/slds/Combobox";
import { Input } from "@/components/slds/Input";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";

const KINDS: HolderKind[] = ["user", "role", "team", "queue", "field", "submitter", "many"];
const STRATEGIES = ["pool", "each_member", "round_robin", "load_based"] as const;

export interface HolderOptions {
  users: ComboboxOption[];
  roles: ComboboxOption[];
  teams: ComboboxOption[];
  queues: ComboboxOption[];
  /** user-type fields of the process's table (+ the record owner) */
  fields: { value: string; label: string }[];
}

interface Props {
  step: ApprovalStep;
  index: number;
  steps: ApprovalStep[];
  options: HolderOptions;
  /** A 422 that points at this step — shown in the card itself. */
  error?: string | null;
  onChange: (next: ApprovalStep) => void;
  onMove: (dir: -1 | 1) => void;
  onRemove: (() => void) | null;
}

export function ApprovalStepEditor({ step, index, steps, options, error, onChange, onMove, onRemove }: Props) {
  const n = index + 1;
  const kind = holderKind(step);
  const patch = (p: Partial<ApprovalStep>) => onChange({ ...step, ...p });
  const setAssign = (p: Partial<NonNullable<ApprovalStep["assignment"]>>) =>
    onChange({ ...step, assignment: { ...(step.assignment ?? { type: "team" as const }), ...p } });
  const rej = step.onReject;
  const rejectValue = !rej || rej.to === "@end" ? "@end" : rej.to;
  const targets = rejectTargets(steps, index);
  const approverOpts = (type: "user" | "role") => (type === "user" ? options.users : options.roles);

  return (
    <div className="grow">
      <p className="slds-text-title_bold slds-m-bottom_x-small">
        {tf("page.approvalBuilder.stepN", { n })}
        {step.name && <span className="slds-text-color_weak"> · {step.name}</span>}
      </p>
      {error && <ScopedNotification theme="error" className="slds-m-bottom_x-small">{error}</ScopedNotification>}

      <div className="grid gap-3 md:grid-cols-[minmax(0,1fr)_minmax(0,2fr)_9rem] md:items-start">
        <Select
          label={t("page.approvalBuilder.holder")}
          placeholder={false}
          value={kind}
          onChange={(e) => onChange(setHolderKind(step, e.target.value as HolderKind))}
          options={KINDS.map((k) => ({ value: k, label: t(`page.approvalBuilder.holder.${k}` as I18nKey) }))}
        />

        {(kind === "user" || kind === "role") && (
          <Combobox
            variant="autocomplete"
            label={kind === "user" ? t("page.approvalBuilder.approver") : t("page.approvalBuilder.approverRoleField")}
            placeholder={kind === "user" ? t("page.approvalBuilder.approverPh") : t("page.approvalBuilder.rolePh")}
            options={approverOpts(kind)}
            value={step.approverId || null}
            onChange={(v) => patch({ approverId: v ?? "" })}
          />
        )}
        {(kind === "team" || kind === "queue") && (
          <Combobox
            variant="autocomplete"
            label={kind === "team" ? t("page.approvalBuilder.team") : t("page.approvalBuilder.queue")}
            placeholder={kind === "team" ? t("page.approvalBuilder.teamPh") : t("page.approvalBuilder.queuePh")}
            options={kind === "team" ? options.teams : options.queues}
            value={step.assignment?.id || null}
            onChange={(v) => setAssign({ type: kind, id: v ?? "" })}
          />
        )}
        {kind === "field" && (
          <Select
            label={t("page.approvalBuilder.field")}
            placeholder={false}
            value={step.assignment?.field ?? OWNER_FIELD}
            onChange={(e) => setAssign({ type: "field", field: e.target.value })}
            options={options.fields}
          />
        )}
        {(kind === "submitter" || kind === "many") && <span />}

        <Select
          label={t("page.approvalBuilder.slaClock")}
          placeholder={false}
          value={step.slaClock ?? "business"}
          onChange={(e) => patch({ slaClock: e.target.value as "business" | "wall" })}
          options={(["business", "wall"] as const).map((k) => ({ value: k, label: t(`page.approvalBuilder.slaClock.${k}` as I18nKey) }))}
        />
      </div>

      {kind === "team" && (
        <Select
          className="slds-m-top_small"
          label={t("page.approvalBuilder.strategy")}
          placeholder={false}
          value={step.assignment?.strategy ?? "pool"}
          onChange={(e) => setAssign({ strategy: e.target.value as (typeof STRATEGIES)[number] })}
          options={STRATEGIES.map((k) => ({ value: k, label: t(`page.approvalBuilder.strategy.${k}` as I18nKey) }))}
        />
      )}

      {kind === "many" && <ManyApprovers step={step} options={options} onChange={onChange} />}

      <div className="slds-m-top_small grid gap-3 md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_9rem] md:items-start">
        <Select
          label={t("page.approvalBuilder.onReject")}
          placeholder={false}
          value={rejectValue}
          onChange={(e) => {
            const to = e.target.value;
            patch({ onReject: to === "@end" ? undefined : to === "@submitter" ? { to, resumeAt: "rejector" } : { to } });
          }}
          options={[
            { value: "@end", label: t("page.approvalBuilder.onReject.end") },
            { value: "@submitter", label: t("page.approvalBuilder.onReject.submitter") },
            ...targets.map((x) => ({ value: x.id, label: tf("page.approvalBuilder.onReject.back", { n: x.index + 1 }) })),
          ]}
        />
        {rej?.to === "@submitter" ? (
          <RadioButtonGroup
            label={t("page.approvalBuilder.resumeAt")}
            value={rej.resumeAt ?? "rejector"}
            onValueChange={(v) => patch({ onReject: { to: "@submitter", resumeAt: v as "rejector" | "start" } })}
            options={(["rejector", "start"] as const).map((k) => ({ value: k, label: t(`page.approvalBuilder.resumeAt.${k}` as I18nKey) }))}
          />
        ) : <span />}
        <Input
          type="number"
          min={1}
          label={t("page.approvalBuilder.sla")}
          fixedTextRight={t("page.approvalBuilder.slaUnit")}
          value={(step.slaSeconds ?? 3600) / 3600}
          onChange={(e) => patch({ slaSeconds: parseInt(e.target.value) * 3600 })}
        />
      </div>

      <div className="slds-m-top_small flex gap-1">
        <ButtonIcon icon={ArrowUp} variant="border-filled" label={tf("page.approvalBuilder.moveUp", { n })} disabled={index === 0} onClick={() => onMove(-1)} />
        <ButtonIcon icon={ArrowDown} variant="border-filled" label={tf("page.approvalBuilder.moveDown", { n })} disabled={index === steps.length - 1} onClick={() => onMove(1)} />
        {onRemove && <ButtonIcon icon={Trash2} variant="border-filled" label={tf("page.approvalBuilder.removeStep", { n })} onClick={onRemove} />}
      </div>
    </div>
  );
}

function ManyApprovers({ step, options, onChange }: { step: ApprovalStep; options: HolderOptions; onChange: (s: ApprovalStep) => void }) {
  const list = step.approvers ?? [];
  const completion = step.completion ?? { mode: "all" as const };
  const setList = (approvers: NonNullable<ApprovalStep["approvers"]>) => onChange({ ...step, approvers });
  return (
    <fieldset className="slds-m-top_small">
      <legend className="slds-form-element__legend slds-form-element__label">{t("page.approvalBuilder.approvers")}</legend>
      {list.map((a, i) => (
        <div key={i} className="grid items-end gap-2 md:grid-cols-[auto_minmax(0,1fr)_auto] slds-m-bottom_x-small">
          <RadioButtonGroup
            label={t("page.approvalBuilder.approverType")}
            value={a.type}
            onValueChange={(v) => setList(list.map((x, j) => (j === i ? { type: v as "user" | "role", id: "" } : x)))}
            options={[
              { value: "user", label: t("page.approvalBuilder.approverUser"), icon: User },
              { value: "role", label: t("page.approvalBuilder.approverRole"), icon: Users },
            ]}
          />
          <Combobox
            variant="autocomplete"
            label={a.type === "user" ? t("page.approvalBuilder.approver") : t("page.approvalBuilder.approverRoleField")}
            placeholder={a.type === "user" ? t("page.approvalBuilder.approverPh") : t("page.approvalBuilder.rolePh")}
            options={a.type === "user" ? options.users : options.roles}
            value={a.id || null}
            onChange={(v) => setList(list.map((x, j) => (j === i ? { ...x, id: v ?? "" } : x)))}
          />
          <ButtonIcon
            icon={Trash2}
            variant="border-filled"
            label={tf("page.approvalBuilder.removeApprover", { n: i + 1 })}
            disabled={list.length <= 1}
            onClick={() => setList(list.filter((_, j) => j !== i))}
          />
        </div>
      ))}
      <Button iconLeft={Plus} onClick={() => setList([...list, { type: "user", id: "" }])}>{t("page.approvalBuilder.addApprover")}</Button>
      <div className="slds-m-top_small grid gap-3 md:grid-cols-[minmax(0,2fr)_9rem] md:items-start">
        <Select
          label={t("page.approvalBuilder.completion")}
          placeholder={false}
          value={completion.mode}
          onChange={(e) => {
            const mode = e.target.value as "all" | "any" | "quorum";
            onChange({ ...step, completion: mode === "quorum" ? { mode, percent: completion.percent ?? 50 } : { mode } });
          }}
          options={(["all", "any", "quorum"] as const).map((k) => ({ value: k, label: t(`page.approvalBuilder.completion.${k}` as I18nKey) }))}
        />
        {completion.mode === "quorum" && (
          <Input
            type="number"
            min={1}
            max={100}
            label={t("page.approvalBuilder.percent")}
            fixedTextRight="%"
            value={completion.percent ?? ""}
            onChange={(e) => onChange({ ...step, completion: { mode: "quorum", percent: e.target.value === "" ? undefined : Number(e.target.value) } })}
          />
        )}
      </div>
    </fieldset>
  );
}
