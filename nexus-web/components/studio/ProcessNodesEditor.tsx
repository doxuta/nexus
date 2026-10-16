"use client";

// The nodes of a branching approval process (specs/043 FE-BPM-PROPS-b, T224/T227): approval/task nodes
// reuse ApprovalStepEditor; the machine nodes get their own card — gateway (branches + default),
// auto (fields written onto the record), wait (condition + SLA with a priority matrix) and end.
// Pure rules (ids, wire shape) live in lib/processModel; conditions are CondState (lib/approvalCriteria).
import { ArrowDown, ArrowUp, Plus, Trash2 } from "lucide-react";
import type { ApprovalGraphNode, ApprovalStep, FieldMeta } from "@/lib/api";
import { approvalOpsForType, approvalOpsWithEdge, noValueOp, type CondState } from "@/lib/approvalCriteria";
import { nodeStepView, nodeWithStep } from "@/lib/processModel";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { ExpressionBuilder } from "@/components/slds/ExpressionBuilder";
import { Input } from "@/components/slds/Input";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { ApprovalStepEditor, type HolderOptions } from "@/components/studio/ApprovalStepEditor";

const UNITS = ["minutes", "hours", "days", "business_hours"] as const;

/** One condition document: rows + (from two rows on) the VÀ/HOẶC choice. `edge` adds "vừa đổi (thành)". */
export function CondEditor({ value, onChange, fields, edge = false }: { value: CondState; onChange: (c: CondState) => void; fields: FieldMeta[]; edge?: boolean }) {
  return (
    <ExpressionBuilder
      title={null}
      fields={fields}
      value={value.rows}
      onChange={(rows) => onChange({ ...value, rows })}
      opsFor={edge ? approvalOpsWithEdge : approvalOpsForType}
      valueless={noValueOp}
      relativeDates={false}
      {...(value.rows.length > 1 && {
        match: value.match === "or" ? "any" : "all",
        onMatchChange: (m: string) => onChange({ ...value, match: m === "any" ? "or" : "and" }),
        matchOptions: ["all", "any"],
      })}
    />
  );
}

interface Props {
  nodes: ApprovalGraphNode[];
  onChange: (nodes: ApprovalGraphNode[]) => void;
  onMove: (idx: number, dir: -1 | 1) => void;
  onRemove: (idx: number) => void;
  holders: HolderOptions;
  fields: FieldMeta[];
  errorAt: { index: number; message: string } | null;
}

const nodeLabel = (n: ApprovalGraphNode, i: number) => `${i + 1} · ${n.name || t(`page.approvalBuilder.node.${n.kind}` as I18nKey)}`;

export function ProcessNodesEditor({ nodes, onChange, onMove, onRemove, holders, fields, errorAt }: Props) {
  const patch = (i: number, p: Partial<ApprovalGraphNode>) => onChange(nodes.map((n, j) => (j === i ? { ...n, ...p } : n)));
  const targets = (i: number) => nodes.flatMap((n, j) => (j === i || !n.id ? [] : [{ value: n.id, label: nodeLabel(n, j) }]));
  const pick = (i: number) => [{ value: "", label: t("page.approvalBuilder.nodePick") }, ...targets(i)];
  // reject targets of a human node: earlier approval nodes only, named by id
  const asSteps: ApprovalStep[] = nodes.map((n) => (n.kind === "approval" ? { ...nodeStepView(n) } : { approverType: "user", approverId: "" }));

  return (
    <ol className="slds-progress__list" aria-label={t("page.approvalBuilder.steps")}>
      {nodes.map((n, i) => {
        const err = errorAt?.index === i ? errorAt.message : null;
        const human = n.kind === "approval" || n.kind === "task";
        const nextSelect = n.kind !== "gateway" && n.kind !== "end" && (
          <Select
            className="slds-m-top_small"
            label={t("page.approvalBuilder.nodeNext")}
            placeholder={false}
            value={n.next ?? ""}
            onChange={(e) => patch(i, e.target.value ? { next: e.target.value } : { next: undefined })}
            options={[{ value: "", label: t("page.approvalBuilder.nodeNextDefault") }, ...targets(i)]}
          />
        );
        return (
          <li key={n.id ?? i} className="slds-progress__item">
            <div className="slds-progress__marker" />
            <div className="slds-progress__item_content slds-grid slds-p-bottom_medium">
              {human ? (
                <div className="grow">
                  <ApprovalStepEditor
                    step={nodeStepView(n)}
                    index={i}
                    steps={asSteps}
                    options={holders}
                    error={err}
                    onChange={(s) => onChange(nodes.map((x, j) => (j === i ? nodeWithStep(x, s) : x)))}
                    onMove={(d) => onMove(i, d)}
                    onRemove={nodes.length > 1 ? () => onRemove(i) : null}
                  />
                  {nextSelect}
                </div>
              ) : (
                <div className="grow">
                  <p className="slds-text-title_bold slds-m-bottom_x-small">
                    {tf("page.approvalBuilder.nodeN", { n: i + 1, kind: t(`page.approvalBuilder.node.${n.kind}` as I18nKey) })}
                  </p>
                  {err && <ScopedNotification theme="error" className="slds-m-bottom_x-small">{err}</ScopedNotification>}
                  <Input label={t("page.approvalBuilder.nodeName")} value={n.name ?? ""} onChange={(e) => patch(i, { name: e.target.value })} />
                  {n.kind === "gateway" && <Gateway node={n} fields={fields} pick={pick(i)} onPatch={(p) => patch(i, p)} />}
                  {n.kind === "auto" && <Auto node={n} fields={fields} onPatch={(p) => patch(i, p)} />}
                  {n.kind === "end" && (
                    <Select
                      className="slds-m-top_small"
                      label={t("page.approvalBuilder.endResult")}
                      placeholder={false}
                      value={n.result === "rejected" ? "rejected" : "approved"}
                      onChange={(e) => patch(i, { result: e.target.value })}
                      options={(["approved", "rejected"] as const).map((k) => ({ value: k, label: t(`page.approvalBuilder.endResult.${k}` as I18nKey) }))}
                    />
                  )}
                  {n.kind === "wait" && <Wait node={n} fields={fields} holders={holders} onPatch={(p) => patch(i, p)} />}
                  {nextSelect}
                  <div className="slds-m-top_small flex gap-1">
                    <ButtonIcon icon={ArrowUp} variant="border-filled" label={tf("page.approvalBuilder.moveUp", { n: i + 1 })} disabled={i === 0} onClick={() => onMove(i, -1)} />
                    <ButtonIcon icon={ArrowDown} variant="border-filled" label={tf("page.approvalBuilder.moveDown", { n: i + 1 })} disabled={i === nodes.length - 1} onClick={() => onMove(i, 1)} />
                    <ButtonIcon icon={Trash2} variant="border-filled" label={tf("page.approvalBuilder.removeNode", { n: i + 1 })} onClick={() => onRemove(i)} />
                  </div>
                </div>
              )}
            </div>
          </li>
        );
      })}
    </ol>
  );
}

type Branch = { name: string; when: CondState; to: string };

function Gateway({ node, fields, pick, onPatch }: { node: ApprovalGraphNode; fields: FieldMeta[]; pick: { value: string; label: string }[]; onPatch: (p: Partial<ApprovalGraphNode>) => void }) {
  const branches = (node.branches ?? []) as Branch[];
  const set = (next: Branch[]) => onPatch({ branches: next });
  return (
    <div className="slds-m-top_small">
      <p className="slds-form-element__label">{t("page.approvalBuilder.branches")}</p>
      {branches.map((b, j) => (
        <fieldset key={j} className="slds-box slds-m-bottom_x-small">
          <div className="grid items-end gap-2 md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]">
            <Input label={t("page.approvalBuilder.branchName")} value={b.name} onChange={(e) => set(branches.map((x, k) => (k === j ? { ...x, name: e.target.value } : x)))} />
            <Select label={t("page.approvalBuilder.branchTo")} placeholder={false} value={b.to} onChange={(e) => set(branches.map((x, k) => (k === j ? { ...x, to: e.target.value } : x)))} options={pick} />
            <ButtonIcon icon={Trash2} variant="border-filled" label={tf("page.approvalBuilder.branchRemove", { n: j + 1 })} onClick={() => set(branches.filter((_, k) => k !== j))} />
          </div>
          {b.when.rows.length === 0 && <p className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">{t("page.approvalBuilder.branchNoRows")}</p>}
          <CondEditor value={b.when} fields={fields} onChange={(c) => set(branches.map((x, k) => (k === j ? { ...x, when: c } : x)))} />
        </fieldset>
      ))}
      <Button iconLeft={Plus} onClick={() => set([...branches, { name: "", when: { match: "and", rows: [] }, to: "" }])}>{t("page.approvalBuilder.branchAdd")}</Button>
      <Select className="slds-m-top_small" label={t("page.approvalBuilder.branchDefault")} placeholder={false} value={String(node.default ?? "")} onChange={(e) => onPatch({ default: e.target.value })} options={pick} />
    </div>
  );
}

function Auto({ node, fields, onPatch }: { node: ApprovalGraphNode; fields: FieldMeta[]; onPatch: (p: Partial<ApprovalGraphNode>) => void }) {
  const set = (node.set ?? []) as { field: string; value: unknown }[];
  const put = (next: typeof set) => onPatch({ set: next });
  const writable = fields.filter((f) => !["formula", "autonumber", "rollup"].includes(f.fieldType));
  return (
    <div className="slds-m-top_small">
      <p className="slds-form-element__label">{t("page.approvalBuilder.setTitle")}</p>
      {set.map((s, j) => (
        <div key={j} className="grid items-end gap-2 slds-m-bottom_x-small md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]">
          <Select label={t("page.approvalBuilder.setField")} placeholder={t("page.approvalBuilder.nodePick")} value={s.field} onChange={(e) => put(set.map((x, k) => (k === j ? { ...x, field: e.target.value } : x)))} options={writable.map((f) => ({ value: f.name, label: f.label || f.name }))} />
          <Input label={t("page.approvalBuilder.setValue")} value={String(s.value ?? "")} onChange={(e) => put(set.map((x, k) => (k === j ? { ...x, value: e.target.value } : x)))} />
          <ButtonIcon icon={Trash2} variant="border-filled" label={tf("page.approvalBuilder.setRemove", { n: j + 1 })} disabled={set.length <= 1} onClick={() => put(set.filter((_, k) => k !== j))} />
        </div>
      ))}
      <Button iconLeft={Plus} onClick={() => put([...set, { field: "", value: "" }])}>{t("page.approvalBuilder.setAdd")}</Button>
    </div>
  );
}

type Row = { when: CondState; amount: number; unit: string };
type Sla = { amount?: number; unit?: string; matrix?: Row[]; writeDueTo?: string; onChange?: string; notify?: { type: string; id?: string; field?: string } };

function Wait({ node, fields, holders, onPatch }: { node: ApprovalGraphNode; fields: FieldMeta[]; holders: HolderOptions; onPatch: (p: Partial<ApprovalGraphNode>) => void }) {
  const until = (node.until ?? { match: "and", rows: [] }) as CondState;
  const sla = node.sla as Sla | undefined;
  const setSla = (p: Partial<Sla>) => onPatch({ sla: { ...sla, ...p } });
  const unitOpts = UNITS.map((u) => ({ value: u, label: t(`page.approvalBuilder.slaUnit.${u}` as I18nKey) }));
  const matrix = sla?.matrix ?? [];
  const setRow = (j: number, p: Partial<Row>) => setSla({ matrix: matrix.map((r, k) => (k === j ? { ...r, ...p } : r)) });
  const notify = sla?.notify;
  const notifyKind = notify?.type ?? "";
  const dateFields = fields.filter((f) => f.fieldType === "datetime" || f.fieldType === "date");
  const notifyOpts = notifyKind === "user" ? holders.users : notifyKind === "role" ? holders.roles : notifyKind === "team" ? holders.teams : [];
  return (
    <div className="slds-m-top_small">
      <p className="slds-form-element__label">{t("page.approvalBuilder.waitUntil")}</p>
      <CondEditor value={until} fields={fields} onChange={(c) => onPatch({ until: c })} />
      {!sla ? (
        <Button className="slds-m-top_small" iconLeft={Plus} onClick={() => onPatch({ sla: { amount: 4, unit: "business_hours", notify: { type: "submitter" } } })}>{t("page.approvalBuilder.waitSla")}</Button>
      ) : (
        <fieldset className="slds-box slds-m-top_small">
          <div className="flex items-center justify-between">
            <legend className="slds-form-element__legend slds-form-element__label">{t("page.approvalBuilder.waitSla")}</legend>
            <Button variant="text-destructive" onClick={() => onPatch({ sla: undefined })}>{t("page.approvalBuilder.waitSlaNone")}</Button>
          </div>
          <div className="grid gap-2 md:grid-cols-[8rem_minmax(0,1fr)_minmax(0,1fr)_minmax(0,1fr)]">
            <Input type="number" min={1} label={t("page.approvalBuilder.slaAmount")} value={sla.amount ?? ""} onChange={(e) => setSla({ amount: e.target.value === "" ? undefined : Number(e.target.value) })} />
            <Select label={t("page.approvalBuilder.slaUnitField")} placeholder={false} value={sla.unit ?? "business_hours"} onChange={(e) => setSla({ unit: e.target.value })} options={unitOpts} />
            <Select label={t("page.approvalBuilder.slaWriteDueTo")} placeholder={false} value={sla.writeDueTo ?? ""} onChange={(e) => setSla({ writeDueTo: e.target.value })} options={[{ value: "", label: t("page.approvalBuilder.slaWriteDueToNone") }, ...dateFields.map((f) => ({ value: f.name, label: f.label || f.name }))]} />
            <Select label={t("page.approvalBuilder.slaOnChange")} placeholder={false} value={sla.onChange || "keep"} onChange={(e) => setSla({ onChange: e.target.value })} options={(["keep", "tighten_only"] as const).map((k) => ({ value: k, label: t(`page.approvalBuilder.slaOnChange.${k}` as I18nKey) }))} />
          </div>

          <p className="slds-form-element__label slds-m-top_small">{t("page.approvalBuilder.slaMatrix")}</p>
          {matrix.map((r, j) => (
            <div key={j} className="slds-box slds-m-bottom_x-small">
              <div className="grid items-end gap-2 md:grid-cols-[8rem_minmax(0,1fr)_auto]">
                <Input type="number" min={1} label={t("page.approvalBuilder.slaAmount")} value={String(r.amount)} onChange={(e) => setRow(j, { amount: Number(e.target.value) })} />
                <Select label={t("page.approvalBuilder.slaUnitField")} placeholder={false} value={r.unit} onChange={(e) => setRow(j, { unit: e.target.value })} options={unitOpts} />
                <ButtonIcon icon={Trash2} variant="border-filled" label={tf("page.approvalBuilder.slaMatrixRemove", { n: j + 1 })} onClick={() => setSla({ matrix: matrix.filter((_, k) => k !== j) })} />
              </div>
              <CondEditor value={r.when} fields={fields} onChange={(c) => setRow(j, { when: c })} />
            </div>
          ))}
          <Button iconLeft={Plus} onClick={() => setSla({ matrix: [...matrix, { when: { match: "and", rows: [] }, amount: 1, unit: "hours" }] })}>{t("page.approvalBuilder.slaMatrixAdd")}</Button>

          <div className="slds-m-top_small grid gap-2 md:grid-cols-2">
            <Select
              label={t("page.approvalBuilder.slaNotify")}
              placeholder={t("page.approvalBuilder.nodePick")}
              value={notifyKind}
              onChange={(e) => setSla({ notify: e.target.value ? (e.target.value === "field" ? { type: "field", field: "owner_id" } : { type: e.target.value }) : undefined })}
              options={[
                { value: "submitter", label: t("page.approvalBuilder.notifySubmitter") },
                { value: "user", label: t("page.approvalBuilder.holder.user") },
                { value: "role", label: t("page.approvalBuilder.holder.role") },
                { value: "team", label: t("page.approvalBuilder.holder.team") },
                { value: "field", label: t("page.approvalBuilder.holder.field") },
              ]}
              help={t("page.approvalBuilder.slaNotifyHelp")}
            />
            {(notifyKind === "user" || notifyKind === "role" || notifyKind === "team") && (
              <Select label={t("page.approvalBuilder.nodePick")} placeholder={t("page.approvalBuilder.nodePick")} value={notify?.id ?? ""} onChange={(e) => setSla({ notify: { type: notifyKind, id: e.target.value } })} options={notifyOpts.map((o) => ({ value: o.value, label: o.label }))} />
            )}
            {notifyKind === "field" && (
              <Select label={t("page.approvalBuilder.field")} placeholder={false} value={notify?.field ?? "owner_id"} onChange={(e) => setSla({ notify: { type: "field", field: e.target.value } })} options={holders.fields} />
            )}
          </div>
        </fieldset>
      )}
    </div>
  );
}
