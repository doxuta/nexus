"use client";

// SampleGenerateForm — in-place sample generator for the Chạy thử step (specs/077 T094; replaces the old modal:
// a dialog to create is a UI-ARCH 5.7 no). A `slds-theme_shade` strip: tables (Checkbox group), source, rows per
// table (5/10/20), Sinh · Huỷ. One mutation, so a double click sends one request. The report is read in an
// aria-live region. `initial` = the tables to tick (one row's menu) — omitted ⇒ every table.
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { RadioGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Select } from "@/components/slds/Select";
import { generateSampleData, getPublishState, listPackInstalls, type SampleGenerateResult, type SampleSource } from "@/lib/api";
import { t } from "@/lib/i18n";
import { useProjectQuery } from "../useProjectQuery";
import { SampleResult } from "./SampleResult";
import { invalidateSampleData } from "./useSampleSummary";

const PER_ENTITY = ["5", "10", "20"];
const SOURCES: SampleSource[] = ["auto", "fields"];

export function SampleGenerateForm({ slug, entityNames, labels, initial, onClose }: {
  slug: string;
  entityNames: string[];
  labels: Map<string, string>;
  initial?: string[];
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [picked, setPicked] = useState<string[]>(initial ?? entityNames);
  const [per, setPer] = useState(PER_ENTITY[0]);
  const [source, setSource] = useState<SampleSource>("auto");
  const [result, setResult] = useState<SampleGenerateResult | null>(null);
  const first = useRef<HTMLDivElement>(null);
  const label = (e: string) => labels.get(e) ?? e;
  // Delivered ⇒ samples land in the app the customer is using. Unknown (loading / error) ⇒ say nothing.
  const publish = useProjectQuery(slug, ["publishState"], getPublishState);
  const delivered = publish.data?.delivered === true;

  // "Từ gói" only when the ledger lists an install (never probed by a trial generate: that writes real data).
  const installs = useProjectQuery(slug, ["packInstalls"], listPackInstalls);
  const hasPack = (installs.data?.length ?? 0) > 0;
  const sources = hasPack ? [...SOURCES, "pack" as SampleSource] : SOURCES;

  useEffect(() => first.current?.querySelector("input")?.focus(), []);

  const run = useMutation({
    mutationFn: () => generateSampleData(entityNames.filter((e) => picked.includes(e)), Number(per), source),
    onSuccess: (r) => {
      setResult(r);
      void invalidateSampleData(qc, slug);
      void qc.invalidateQueries({ queryKey: ["project", slug, "studio", "handoff"] });
    },
  });
  const toggle = (e: string) => setPicked((p) => (p.includes(e) ? p.filter((x) => x !== e) : [...p, e]));
  const allPicked = picked.length === entityNames.length;

  return (
    <div
      className="slds-theme_shade slds-p-around_medium slds-m-bottom_small"
      onKeyDown={(e) => { if (e.key === "Escape") onClose(); }}
    >
      <p className="slds-text-color_weak slds-m-bottom_small">{t("studio.build.trial.generate.help")}</p>
      {delivered && <ScopedNotification theme="warning">{t("studio.build.trial.generate.deliveredWarn")}</ScopedNotification>}
      <fieldset className="slds-form-element slds-m-bottom_small">
        <legend className="slds-form-element__legend slds-form-element__label slds-grid slds-grid_vertical-align-center">
          <span className="slds-col">{t("studio.build.trial.generate.tables")}</span>
          <Button variant="base" onClick={() => setPicked(allPicked ? [] : entityNames)}>
            {t(allPicked ? "studio.build.trial.generate.selectNone" : "studio.build.trial.generate.selectAll")}
          </Button>
        </legend>
        <div ref={first} className="slds-scrollable_y" style={{ maxHeight: "14rem", columnWidth: "14rem" }}>
          {entityNames.map((e) => (
            <Checkbox key={e} label={label(e)} checked={picked.includes(e)} onChange={() => toggle(e)} />
          ))}
        </div>
      </fieldset>
      <RadioGroup
        label={t("studio.build.trial.generate.source")}
        value={source}
        onValueChange={(v) => setSource(v as SampleSource)}
        options={sources.map((v) => ({ value: v, label: t(`studio.build.trial.generate.source.${v}` as const) }))}
      />
      <Select
        label={t("studio.build.trial.generate.perEntity")}
        value={per}
        onChange={(ev) => setPer(ev.target.value)}
        options={PER_ENTITY.map((n) => ({ value: n, label: n }))}
        placeholder={false}
        disabled={source === "pack"}
      />
      <div className="slds-m-top_small">
        <Button variant="neutral" onClick={onClose}>
          {t(result ? "studio.build.trial.generate.close" : "studio.build.trial.generate.cancel")}
        </Button>
        <Button variant="brand" className="slds-m-left_x-small" disabled={picked.length === 0 || run.isPending} onClick={() => run.mutate()}>
          {run.isPending
            ? t("studio.build.trial.generate.running")
            : t(delivered ? "studio.build.trial.generate.runLive" : "studio.build.trial.generate.run")}
        </Button>
      </div>
      <div aria-live="polite">
        {run.isError && (
          <ScopedNotification theme="error">
            {run.error instanceof Error && run.error.message ? run.error.message : t("studio.build.trial.generate.error")}
          </ScopedNotification>
        )}
        {result && <SampleResult result={result} label={label} />}
      </div>
    </div>
  );
}
