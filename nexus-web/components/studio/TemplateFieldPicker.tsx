"use client";

// The email action's field pickers (specs/059, DAILY-QUEUE #271): "Chọn người nhận" lists the email
// fields of the record AND of every record it links to (the patient of an appointment), and "Chèn
// trường" does the same for the subject/body. Self-contained — it loads the linked entities' fields
// itself, each entity once, while the menu is open — so the Studio redesign can mount it wherever the
// email editor lands. SLDS `menus` (components/slds/Menu) with one group per record.
import { Braces } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { listFields, type FieldMeta } from "@/lib/api";
import { t } from "@/lib/i18n";
import { relationTargets, templateVarGroups } from "@/lib/workflowTemplateVars";
import { Button } from "@/components/slds/Button";
import { Menu, MenuGroup, MenuItem } from "@/components/slds/Menu";

export function TemplateFieldPicker({
  fields,
  emailOnly = false,
  label,
  onPick,
}: {
  /** Fields of the entity the rule runs on. */
  fields: FieldMeta[];
  /** Only addresses ("Gửi tới"). */
  emailOnly?: boolean;
  /** The trigger button's text — also names the menu. */
  label: string;
  /** Receives the chosen placeholder, e.g. "{{benh_nhan.email}}". */
  onPick: (token: string) => void;
}) {
  // Fields per linked entity, keyed by entity (absent = not loaded yet). A failed entity load degrades
  // to "no fields" for that entity. Keyed by ENTITY, not by rule: the builder keeps this picker mounted
  // across a switch to another rule, and the rule's own fields may arrive after the menu is open, so
  // every open (and every change of targets while open) loads whatever target is still missing.
  const [related, setRelated] = useState<Record<string, FieldMeta[]>>({});
  const [open, setOpen] = useState(false);
  const requested = useRef(new Set<string>());
  const targets = relationTargets(fields);
  const targetsKey = targets.join("\n");

  useEffect(() => {
    if (!open) return;
    const todo = targetsKey.split("\n").filter((e) => e && !requested.current.has(e));
    todo.forEach((e) => requested.current.add(e));
    for (const e of todo) {
      listFields(e).then((fs) => fs, () => [] as FieldMeta[]).then((fs) => setRelated((r) => ({ ...r, [e]: fs })));
    }
  }, [open, targetsKey]);

  const groups = templateVarGroups(fields, related, emailOnly);
  const loading = targets.some((e) => !(e in related));
  return (
    <Menu
      size="small"
      length={10}
      onOpenChange={setOpen}
      trigger={<Button iconLeft={Braces}>{label}</Button>}
    >
      {groups.map((g) => (
        <MenuGroup key={g.key} label={g.label}>
          {g.vars.map((v) => (
            <MenuItem key={v.token} label={v.label} onSelect={() => onPick(v.token)} />
          ))}
        </MenuGroup>
      ))}
      {loading && <MenuItem label={t("page.workflow.tpl.loading")} disabled />}
      {!loading && groups.length === 0 && (
        <MenuItem label={t(emailOnly ? "page.workflow.tpl.noEmail" : "page.workflow.tpl.noField")} disabled />
      )}
    </Menu>
  );
}
