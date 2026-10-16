"use client";

// RecordSlaCard — SLA clocks of the tracking lane on the record page (specs/043 T282/T283).
// GET /:entity/:id/approvals?lane=independent. Renders NOTHING when the record has no such item
// (and while loading / on error: a missing card is honest, a spinner on every record is noise).
import { useQuery } from "@tanstack/react-query";
import { Clock } from "lucide-react";
import { ensureAuth, listRecordSLA } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import { slaBasisText, slaState, type RecordSlaItem } from "@/lib/recordSla";
import { Badge } from "@/components/slds/Badge";
import { Card } from "@/components/slds/Card";

function left(sec: number): string {
  const h = Math.floor(sec / 3600);
  return h >= 1 ? t("page.record.sla.hours").replace("{n}", String(h)) : t("page.record.sla.minutes").replace("{n}", String(Math.max(1, Math.round(sec / 60))));
}

function Row({ it }: { it: RecordSlaItem }) {
  const st = slaState(it);
  const basis = slaBasisText(it.sla.basis);
  const badge =
    st.kind === "met" ? (basis ? t("page.record.sla.metBasis").replace("{basis}", basis) : t("page.record.sla.met"))
    : st.kind === "overdue" ? t("page.record.sla.overdue")
    : st.kind === "paused" ? t("page.record.sla.paused")
    : t("page.record.sla.running");
  let line = "";
  if (st.kind === "paused") {
    line = it.sla.remainingSeconds != null ? t("page.record.sla.pausedLeft").replace("{left}", left(it.sla.remainingSeconds)) : "";
  } else if (it.sla.dueAt && st.kind !== "met") {
    const when = formatDateTime(it.sla.dueAt);
    line = basis ? t("page.record.sla.due").replace("{when}", when).replace("{basis}", basis) : t("page.record.sla.dueNoBasis").replace("{when}", when);
  }
  return (
    <li className="slds-m-bottom_x-small">
      <div className="slds-grid slds-grid_align-spread slds-grid_vertical-align-center">
        <span className="slds-text-title_bold">{it.stepName}</span>
        <Badge theme={st.theme}>{badge}</Badge>
      </div>
      {line && <p className="slds-text-body_small slds-text-color_weak">{line}</p>}
    </li>
  );
}

export function RecordSlaCard({ entity, recordId }: { entity: string; recordId: string }) {
  const { data } = useQuery({
    queryKey: ["record-sla", entity, recordId],
    queryFn: async () => { await ensureAuth(); return listRecordSLA(entity, recordId); },
  });
  if (!data || data.length === 0) return null;
  return (
    <Card title={t("page.record.sla.title")} icon={{ glyph: Clock, name: "service_report" }} boundary className="slds-m-bottom_small">
      <ul>{data.map((it) => <Row key={it.id} it={it} />)}</ul>
    </Card>
  );
}
