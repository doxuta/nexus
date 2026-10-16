"use client";

// Chạy thử › Dữ liệu mẫu theo bảng (specs/077 f5-trial-sample-view): read-only. One row per table of the project
// (zeros included), the whole project whatever app is picked. Generate = in-place form (header button / row menu).
// Wipe (specs/077 f5-sample-wipe): header button + "Xoá mẫu của bảng này" in the row menu, both open the in-page SampleWipeConfirm.
import { useQueryClient } from "@tanstack/react-query";
import { Ellipsis, Plus } from "lucide-react";
import { useRef, useState } from "react";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { ApiError, type SampleWipeResult } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { sampleSourceText } from "@/lib/sampleLabels";
import { QueryState } from "../QueryState";
import { SampleGenerateForm } from "./SampleGenerateForm";
import { SampleKeptList } from "./SampleKeptList";
import { SampleWipeConfirm } from "./SampleWipeConfirm";
import { invalidateSampleData, useSampleSummary } from "./useSampleSummary";

const fill = (s: string, v: Record<string, string | number>) => s.replace(/\{(\w+)\}/g, (m, k: string) => (k in v ? String(v[k]) : m));

export function SampleDataByTable({ slug }: { slug: string }) {
  const s = useSampleSummary(slug);
  const forbidden = s.error instanceof ApiError && s.error.status === 403;
  const tables = s.rows.filter((r) => r.count > 0).length;
  // null = form closed; initial undefined = every table (header button), [name] = one row's "Sinh thêm".
  const [gen, setGen] = useState<{ initial?: string[]; key: number } | null>(null);
  const opener = useRef<HTMLElement | null>(null);
  const names = s.rows.map((r) => r.entity);
  const labels = new Map(s.rows.map((r) => [r.entity, r.ambiguous ? `${r.label} (${r.entity})` : r.label]));
  const open = (initial?: string[]) => { opener.current = document.activeElement as HTMLElement; setGen({ initial, key: Date.now() }); };
  const close = () => { setGen(null); setTimeout(() => opener.current?.focus(), 0); };
  const qc = useQueryClient();
  // scope: undefined = whole project; string = one table. `result` = what the last wipe did.
  const [wipe, setWipe] = useState<{ scope?: string } | null>(null);
  const [result, setResult] = useState<SampleWipeResult | null>(null);
  const rowsTotal = s.rows.reduce((n, r) => n + r.count, 0); // same source the box counts from
  const label = (e: string) => labels.get(e) ?? e;
  // The opener (button / menu item) leaves the DOM while the box is open, so focus returns to the section heading.
  const headRef = useRef<HTMLHeadingElement>(null);
  const closeWipe = () => { setWipe(null); requestAnimationFrame(() => headRef.current?.focus()); };
  const openWipe = (scope?: string) => { setResult(null); setWipe({ scope }); };
  const done = (r: SampleWipeResult) => {
    setResult(r);
    closeWipe();
    void invalidateSampleData(qc, slug);
    void qc.invalidateQueries({ queryKey: ["project", slug, "studio", "handoff"] });
  };
  return (
    <section className="slds-m-top_large" aria-labelledby="trial-sample-h">
      <div className="slds-grid slds-grid_vertical-align-center">
        <h2 id="trial-sample-h" ref={headRef} tabIndex={-1} className="slds-text-heading_medium slds-col">{t("studio.build.trial.sample.title")}</h2>
        {!forbidden && names.length > 0 && !gen && (
          <Button variant="neutral" iconLeft={Plus} onClick={() => open()}>{t("studio.build.trial.generate.open")}</Button>
        )}
        {!forbidden && rowsTotal > 0 && !wipe && (
          <Button variant="text-destructive" onClick={() => openWipe()}>{t("studio.build.trial.wipe.open")}</Button>
        )}
      </div>
      {gen && (
        <div className="slds-m-top_small">
          <SampleGenerateForm key={gen.key} slug={slug} entityNames={names} labels={labels} initial={gen.initial} onClose={close} />
        </div>
      )}
      {forbidden ? (
        <p role="alert" className="slds-text-color_error">{t("studio.build.trial.sample.forbidden")}</p>
      ) : (
        <QueryState
          query={{ isPending: s.isPending, isError: !!s.error, error: s.error, data: s.rows, refetch: s.refetch }}
          empty={<p className="slds-text-color_weak">{t("studio.build.trial.sample.empty")}</p>}
        >
          {wipe && (
            <SampleWipeConfirm
              key={wipe.scope ?? "*"} // a new scope = a new box = a new snapshot (never confirm table A's numbers on table B)
              summary={{ total: s.total, entities: s.rows }}
              entityNames={s.rows.map((r) => r.entity)}
              scope={wipe.scope}
              onDone={done}
              onCancel={closeWipe}
            />
          )}
          {result && (
            <div className="slds-m-bottom_small" role="status">
              <p>{tf("studio.build.trial.wipe.done", { n: result.deleted.reduce((n, d) => n + d.count, 0) })}</p>
              <SampleKeptList kept={result.kept} label={label} />
            </div>
          )}
          <p className="slds-m-bottom_x-small">
            {s.total === 0 ? t("studio.build.trial.sample.noEntities") : fill(t("studio.build.trial.sample.count"), { n: s.total, k: tables })}
          </p>
          <p className="slds-text-color_weak slds-m-bottom_small">{t("studio.build.trial.sample.legend")}</p>
          <table className="slds-table slds-table_bordered slds-table_cell-buffer slds-max-medium-table_stacked">
            <thead>
              <tr className="slds-line-height_reset">
                <th scope="col"><div className="slds-truncate">{t("studio.build.trial.sample.col.table")}</div></th>
                <th scope="col" className="slds-text-align_right"><div className="slds-truncate">{t("studio.build.trial.sample.col.count")}</div></th>
                <th scope="col" className="slds-text-align_right"><div className="slds-truncate">{t("studio.build.trial.sample.col.bin")}</div></th>
                <th scope="col"><div className="slds-truncate">{t("studio.build.trial.sample.col.source")}</div></th>
                <th scope="col"><span className="slds-assistive-text">{t("studio.build.trial.sample.col.actions")}</span></th>
              </tr>
            </thead>
            <tbody>
              {s.rows.map((r) => {
                const name = r.ambiguous ? `${r.label} (${r.entity})` : r.label;
                return (
                  <tr key={r.entity} className="slds-hint-parent">
                    <th scope="row" data-label={t("studio.build.trial.sample.col.table")}><div className="slds-truncate" title={name}>{name}</div></th>
                    <td data-label={t("studio.build.trial.sample.col.count")} className="slds-text-align_right"><span className="tabular-nums">{r.count}</span></td>
                    <td data-label={t("studio.build.trial.sample.col.bin")} className="slds-text-align_right"><span className="tabular-nums">{r.inBin}</span></td>
                    <td data-label={t("studio.build.trial.sample.col.source")}>{sampleSourceText(r.bySource)}</td>
                    <td data-label={t("studio.build.trial.sample.col.actions")} className="slds-text-align_right">
                      <Menu
                        align="right"
                        trigger={<ButtonIcon icon={Ellipsis} variant="border" size="x-small" more label={tf("studio.build.trial.sample.rowMenu", { table: r.label })} />}
                      >
                        <MenuItem label={t("studio.build.trial.sample.generateMore")} onSelect={() => open([r.entity])} />
                        {r.count > 0 && <MenuItem label={t("studio.build.trial.wipe.openOne")} onSelect={() => openWipe(r.entity)} />}
                      </Menu>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </QueryState>
      )}
    </section>
  );
}
