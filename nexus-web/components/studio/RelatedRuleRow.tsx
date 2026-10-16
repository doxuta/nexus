"use client";

// One rule in the table page's related column: name (link to the rule page), its "Khi …" sentence and the on/off switch.
// The switch keeps its own result line (aria-live); a refused switch (409/422) says why at the row, with a link to the rule.
import Link from "next/link";
import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Toggle } from "@/components/slds/Toggle";
import { StatusText } from "@/components/studio/StatusText";
import { toggleRuleActive } from "@/lib/workflowToggle";
import { qk } from "@/lib/query/keys";
import { t, tf } from "@/lib/i18n";

export function RelatedRuleRow({ id, name, when, active, href }: { id: string; name: string; when: string; active: boolean; href?: string }) {
  const qc = useQueryClient();
  const [on, setOn] = useState(active);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ tone: "success" | "error"; text: string; open: boolean } | null>(null);
  const label = tf("studio.build.automation.toggle.label", { name });

  async function change(next: boolean) {
    setBusy(true);
    setMsg(null);
    const r = await toggleRuleActive(id, next);
    setBusy(false);
    if (r.ok) {
      setOn(r.isActive);
      setMsg({ tone: "success", text: tf(r.isActive ? "studio.build.automation.toggle.turnedOn" : "studio.build.automation.toggle.turnedOff", { name }), open: false });
      void qc.invalidateQueries({ queryKey: qk.adminWorkflows() });
    } else {
      setMsg({ tone: "error", text: r.message, open: r.needsOpen });
    }
  }

  return (
    <>
      <p>{href ? <Link href={href}>{name}</Link> : name}</p>
      <p className="slds-text-body_small slds-text-color_weak">{when}</p>
      <Toggle
        label={<span className="slds-assistive-text">{label}</span>}
        aria-label={label}
        onText={t("studio.build.automation.toggle.on")}
        offText={t("studio.build.automation.toggle.off")}
        checked={on}
        disabled={busy}
        onChange={(e) => void change(e.target.checked)}
        className="slds-m-top_xx-small"
      />
      <div aria-live="polite">
        {msg && (
          <StatusText tone={msg.tone}>
            {msg.text}
            {msg.open && href && <> <Link href={href}>{t("studio.build.data.related.rules.open")}</Link></>}
          </StatusText>
        )}
      </div>
    </>
  );
}
