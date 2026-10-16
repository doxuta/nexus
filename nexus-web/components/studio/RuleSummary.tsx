// RuleSummary (specs/074 FR-021, lát f2-rule-list): one rule as a ruled list row — name, then three lines
// Khi / Nếu / Thì built from the stored config by ruleSentence(). Not a card: the list draws the dividers.
// State is coloured text (StatusText), never a Badge: "Đang tắt", draft overlay state, "Chạy khi lưu".
import Link from "next/link";
import { Button } from "@/components/slds/Button";
import { Toggle } from "@/components/slds/Toggle";
import { t, tf } from "@/lib/i18n";
import type { DraftState } from "@/lib/api";
import type { RuleSentence } from "@/lib/ruleSentence";
import { isBeforeTrigger } from "@/lib/workflowBefore";
import { draftStateLabel, StatusText, type StatusTone } from "./StatusText";

export interface RuleSummaryProps {
  name: string;
  sentence: RuleSentence;
  trigger?: string;
  /** false = the rule is switched off. */
  active?: boolean;
  draftState?: DraftState;
  /** Link target of the name; omit while the rule page is not mounted. */
  href?: string;
  /** Present = the row carries its on/off switch (the page owns the write). */
  onToggle?: (next: boolean) => void;
  toggling?: boolean;
  /** Newest run of this rule failed (lib/ruleLastRun); the mark opens that run. */
  failed?: boolean;
  onOpenFailedRun?: () => void;
  /** Result of the last switch, announced politely at the row. */
  message?: { tone: StatusTone; text: string };
}

export function RuleSummary({ name, sentence, trigger, active = true, draftState, href, onToggle, toggling, failed, onOpenFailedRun, message }: RuleSummaryProps) {
  const toggleName = tf("studio.build.automation.toggle.label", { name });
  const draft = draftStateLabel(draftState);
  const rows: Array<[string, string[]]> = [
    [t("studio.build.automation.list.when"), [sentence.when]],
    [t("studio.build.automation.list.if"), [sentence.cond]],
    [t("studio.build.automation.list.then"), sentence.then],
  ];
  return (
    <li className="slds-item">
      <p className="slds-text-heading_small">
        {href ? <Link href={href}>{name}</Link> : name}
        {!onToggle && !active && <StatusText tone="weak" className="slds-m-left_small">{t("studio.build.automation.list.off")}</StatusText>}
        {draft && <StatusText tone={draft.tone} className="slds-m-left_small">{draft.label}</StatusText>}
        {isBeforeTrigger(trigger) && <StatusText tone="warn" className="slds-m-left_small">{t("studio.build.automation.list.runsOnSave")}</StatusText>}
        {failed && (
          <Button variant="base" className="slds-text-color_error slds-m-left_small" onClick={onOpenFailedRun}>
            {t("studio.build.automation.toggle.lastRunFailed")}
          </Button>
        )}
      </p>
      {onToggle && (
        <Toggle
          label={<span className="slds-assistive-text">{toggleName}</span>}
          aria-label={toggleName}
          onText={t("studio.build.automation.toggle.on")}
          offText={t("studio.build.automation.toggle.off")}
          checked={active}
          disabled={toggling}
          onChange={(e) => onToggle(e.target.checked)}
          className="slds-m-top_xx-small"
        />
      )}
      {onToggle && (
        <div aria-live="polite" className="slds-m-top_xx-small">
          {message && <StatusText tone={message.tone}>{message.text}</StatusText>}
        </div>
      )}
      <dl className="slds-m-top_xx-small">
        {rows.map(([word, lines]) => (
          <div key={word} className="flex gap-3">
            <dt className="slds-text-body_small w-12 shrink-0"><strong>{word}</strong></dt>
            <dd className="min-w-0 flex-1">
              {lines.length === 0 ? "—" : lines.map((l, i) => <div key={i}>{l}</div>)}
            </dd>
          </div>
        ))}
      </dl>
    </li>
  );
}
