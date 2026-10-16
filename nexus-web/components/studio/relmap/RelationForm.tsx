"use client";

// RelationForm (specs/075 T173, FR-088): the 4th mode of the relation map's right column — create a link in place, no dialog.
// 1-N / 1-1 add a `relation` field on the DETAIL table (the same POST /admin/fields the table page uses); N-N registers a
// link through POST /admin/relationships. Suggested names are derived while rendering (a typed value wins), so there is no
// state-sync effect. Server refusals are shown through strictErrorMessage only: never the engine's own words.
import { useState } from "react";
import { ArrowLeftRight } from "lucide-react";
import { ApiError, addField, createRelationship, type FieldMeta } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { strictErrorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { isValidIdent, suggestFieldName, type RelType } from "@/lib/relCanvas";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Combobox } from "@/components/slds/Combobox";
import { Input } from "@/components/slds/Input";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";

export interface FormEntity { name: string; label: string; fields: FieldMeta[] | ReadonlyArray<{ name: string }> }
export interface CreatedLink { parent: string; child: string; relType: RelType; fieldName?: string; relName?: string }

const KINDS = [
  { value: "1-N", label: "studio.build.relMap.kind.oneMany" },
  { value: "N-N", label: "studio.build.relMap.kind.manyMany" },
  { value: "1-1", label: "studio.build.relMap.kind.oneOne" },
] as const;

const slug = (s: string) => s.replace(/[^a-z0-9_]/gi, "_").toLowerCase();

export function RelationForm({ entities, initialParent, initialChild, onCreated, onCancel }: {
  entities: FormEntity[];
  initialParent?: string;
  initialChild?: string;
  onCreated: (c: CreatedLink) => void;
  onCancel: () => void;
}) {
  const first = entities[0]?.name ?? "";
  const startParent = initialParent ?? first;
  const [kind, setKind] = useState<RelType>("1-N");
  const [parent, setParent] = useState(startParent);
  const [child, setChild] = useState(initialChild ?? entities.find((e) => e.name !== startParent)?.name ?? "");
  const [typed, setTyped] = useState<{ name?: string; label?: string; nn?: string }>({});
  const [inverse, setInverse] = useState("");
  const [busy, setBusy] = useState(false);
  const [serverErr, setServerErr] = useState<{ on: "name" | "inverse" | "general"; msg: string } | null>(null);

  const labelOf = (n: string) => entities.find((e) => e.name === n)?.label ?? n;
  const childFields = entities.find((e) => e.name === child)?.fields ?? [];
  const fieldName = typed.name ?? (parent && child ? suggestFieldName(parent, child, childFields) : "");
  const fieldLabel = typed.label ?? (parent ? labelOf(parent) : "");
  const nnName = typed.nn ?? slug(`${parent}_${child}`);

  const sameTable = !!parent && parent === child;
  const taken = kind !== "N-N" && childFields.some((f) => f.name === fieldName);
  const nameBad = kind === "N-N" ? !isValidIdent(nnName) : !isValidIdent(fieldName);
  const inverseBad = inverse !== "" && !isValidIdent(inverse);
  const canSubmit = !busy && !!parent && !!child && !sameTable && !nameBad && !taken && !inverseBad && (kind === "N-N" || fieldLabel.trim() !== "");

  const swap = () => { setParent(child); setChild(parent); setTyped((v) => ({ ...v, name: undefined, nn: undefined })); };
  const pick = (set: (v: string) => void) => (v: string | null) => { if (v) set(v); setTyped((v) => ({ ...v, name: undefined, nn: undefined })); };
  const options = entities.map((e) => ({ value: e.name, label: e.label }));

  const submit = async () => {
    if (!canSubmit) return;
    setBusy(true);
    setServerErr(null);
    try {
      if (kind === "N-N") {
        const r = await createRelationship({ name: nnName.trim(), entityA: parent, entityB: child });
        emitDraftsChanged("relation-create"); // a new relation is a pending change on a delivered workspace: the header recounts
        toast.success(tf("studio.build.relMap.form.created", { name: `${labelOf(parent)} → ${labelOf(child)}` }));
        onCreated({ parent, child, relType: kind, relName: r.name });
      } else {
        const opts: Record<string, unknown> = { relatedEntity: parent, cardinality: kind };
        if (inverse.trim()) opts.inverseField = inverse.trim();
        await addField(child, { name: fieldName.trim(), label: fieldLabel.trim(), fieldType: "relation", options: opts });
        emitDraftsChanged("relation-create");
        toast.success(tf("studio.build.relMap.form.created", { name: `${labelOf(parent)} → ${labelOf(child)}` }));
        onCreated({ parent, child, relType: kind, fieldName: fieldName.trim(), relName: undefined });
      }
    } catch (e) {
      const code = e instanceof ApiError ? e.code : "";
      setServerErr({
        on: code.startsWith("FIELD_RELATION_INVERSE") ? "inverse" : code === "FIELD_ALREADY_EXISTS" ? "name" : "general",
        msg: strictErrorMessage(e),
      });
    } finally {
      setBusy(false);
    }
  };

  const identMsg = t("studio.build.relMap.form.identBad");
  const nameErr = serverErr?.on === "name" ? serverErr.msg
    : sameTable ? t("studio.build.relMap.form.sameTable")
    : taken ? tf("studio.build.relMap.form.taken", { bang: labelOf(child) })
    : nameBad && (kind === "N-N" ? nnName : fieldName) !== "" ? identMsg : undefined;

  return (
    <form className="slds-theme_shade slds-p-around_small flex flex-col gap-3" onSubmit={(e) => { e.preventDefault(); void submit(); }}>
      <h2 className="slds-text-heading_small">{t("studio.build.relMap.form.title")}</h2>
      <RadioButtonGroup
        label={t("studio.build.relMap.form.kind")}
        options={KINDS.map((k) => ({ value: k.value, label: t(k.label) }))}
        value={kind}
        onValueChange={(v) => setKind(v as RelType)}
      />
      <Combobox label={t("studio.build.relMap.form.parent")} options={options} value={parent} placeholder={t("studio.build.relMap.form.pickTable")} onChange={pick(setParent)} />
      <div>
        <ButtonIcon variant="border-filled" icon={ArrowLeftRight} label={t("studio.build.relMap.form.swap")} onClick={swap} />
      </div>
      <Combobox label={t("studio.build.relMap.form.child")} options={options} value={child} placeholder={t("studio.build.relMap.form.pickTable")} onChange={pick(setChild)} />

      {kind === "N-N" ? (
        <Input label={t("studio.build.relMap.form.nnName")} value={nnName} error={nameErr} onChange={(e) => setTyped((v) => ({ ...v, nn: e.target.value }))} />
      ) : (
        <>
          <Input label={t("studio.build.relMap.form.fieldName")} value={fieldName} error={nameErr} onChange={(e) => setTyped((v) => ({ ...v, name: e.target.value }))} />
          <Input label={t("studio.build.relMap.panel.fieldLabel")} value={fieldLabel} required onChange={(e) => setTyped((v) => ({ ...v, label: e.target.value }))} />
          <Input
            label={t("studio.build.relMap.form.inverse")}
            value={inverse}
            help={t("studio.build.relMap.form.inverseHelp")}
            error={inverseBad ? identMsg : serverErr?.on === "inverse" ? serverErr.msg : undefined}
            onChange={(e) => setInverse(e.target.value)}
          />
        </>
      )}

      {serverErr?.on === "general" && <ScopedNotification theme="error">{serverErr.msg}</ScopedNotification>}
      <div className="flex gap-2">
        <Button type="submit" variant="brand" disabled={!canSubmit}>{busy ? t("studio.build.relMap.form.creating") : t("studio.build.relMap.form.create")}</Button>
        <Button variant="neutral" onClick={onCancel} disabled={busy}>{t("studio.build.relMap.form.cancel")}</Button>
      </div>
    </form>
  );
}
