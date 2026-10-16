"use client";

// The "Chặn và báo lỗi" action of a before rule (specs/048 FE-before-save-hooks-2), on SLDS:
// `textarea` for the sentence the user reads when refused (1–500 characters, counted like the server
// — trimmed runes), `combobox` for the optional field to pin it under, `form-element` help text for
// the two authoring hints (never name a field the user's role cannot see; prefer "vừa đổi thành" so
// an old record that already breaks the rule is not stuck). Owns no state.
import { Combobox } from "@/components/slds/Combobox";
import { Textarea } from "@/components/slds/Textarea";
import type { FieldMeta } from "@/lib/api";
import { MAX_VETO_MESSAGE, vetoMessageLength } from "@/lib/workflowBefore";
import { t, tf } from "@/lib/i18n";

export function VetoActionEditor({
  trigger,
  message,
  field,
  fields,
  onChange,
  messageError,
  fieldError,
}: {
  /** The rule's trigger; a before_delete rule has no anchor to pick (see below). */
  trigger?: string;
  message: string;
  /** Anchor field name; "" = not pinned (the form shows the message at the top). */
  field: string;
  /** The rule entity's fields. */
  fields: FieldMeta[];
  onChange: (key: "message" | "field", value: string) => void;
  /** Server refusal of the message (WORKFLOW_VETO_MESSAGE_INVALID). */
  messageError?: string;
  /** Server refusal of the anchor (RECORD_UNKNOWN_FIELD). */
  fieldError?: string;
}) {
  const n = vetoMessageLength(message);
  const tooLong = n > MAX_VETO_MESSAGE ? tf("workflow.before.veto.messageTooLong", { max: MAX_VETO_MESSAGE, n }) : undefined;
  const options = [
    { value: "", label: t("workflow.before.veto.fieldNone") },
    ...fields.map((f) => ({ value: f.name, label: f.label || f.name })),
    // A saved anchor the entity no longer has stays visible: opening the rule and saving must not
    // silently drop it (same rule as the other field pickers of this panel).
    ...(field && !fields.some((f) => f.name === field) ? [{ value: field, label: field }] : []),
  ];
  // A delete has no form: its refusal is read in the delete dialog and the mass-delete strip, never
  // under an input, and "vừa đổi thành" (the help's advice) is not offered on a delete rule. So no
  // anchor picker there — unless one is already set (API, pack, a switched trigger): it stays, so it
  // can be cleared and a refusal of it is seen.
  const anchorable = trigger !== "before_delete" || field !== "";
  return (
    <div className="flex flex-col gap-3">
      <Textarea
        label={t("workflow.before.veto.message")}
        required
        rows={2}
        value={message}
        error={messageError ?? tooLong}
        help={tf("workflow.before.veto.messageHelp", { n, max: MAX_VETO_MESSAGE })}
        onChange={(e) => onChange("message", e.target.value)}
      />
      {anchorable && (
        <Combobox
          label={t("workflow.before.veto.field")}
          options={options}
          value={field}
          error={fieldError}
          help={t("workflow.before.veto.fieldHelp")}
          onChange={(v) => onChange("field", v ?? "")}
        />
      )}
    </div>
  );
}
