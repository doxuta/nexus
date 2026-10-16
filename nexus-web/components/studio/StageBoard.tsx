"use client";

// The stage board of the project page (specs/073 FR-091, architecture 3.2): ONE raised surface, four columns separated by
// rules (no grey column, no card per project). A project is one line you can click: name (18px, heavier than the 16px column
// title so size carries the hierarchy), industry, then ONE calm line — build progress + "Kế tiếp", or today's use. Problems
// never sit here; they belong to the navy band. The stage is the server's word (`row.stage`), never computed.
// Presentational: the page owns the data and what each callback does.
import { MoreHorizontal } from "lucide-react";
import { useState, type FormEvent, type KeyboardEvent } from "react";
import { BuildMeter } from "@/components/studio/BuildMeter";
import { Button } from "@/components/slds/Button";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Input } from "@/components/slds/Input";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { Tooltip } from "@/components/slds/Tooltip";
import type { ProjectRow, ProjectStage } from "@/lib/api";
import type { CheckState } from "@/lib/projectCheck";
import { buildSteps, nextStep, type BuildStepKey } from "@/lib/buildSteps";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

const STAGES: { key: ProjectStage; name: I18nKey; desc: I18nKey }[] = [
  { key: "building", name: "studio.stage.building.name", desc: "studio.stage.building.desc" },
  { key: "trial", name: "studio.stage.trial.name", desc: "studio.stage.trial.desc" },
  { key: "live", name: "studio.stage.live.name", desc: "studio.stage.live.desc" },
  { key: "needs_attention", name: "studio.stage.needsAttention.name", desc: "studio.stage.needsAttention.desc" },
];

export interface StageBoardProps {
  rows: ProjectRow[];
  /** Filter text (the page keeps it in `?q=`): matches name, industry and code. */
  query: string;
  onOpen: (row: ProjectRow) => void;
  onOpenStep: (row: ProjectRow, step: BuildStepKey) => void;
  /** `true` = "Bắt đầu chạy thử", `false` = "Về Đang dựng". */
  onTrial: (row: ProjectRow, on: boolean) => void;
  /** `null` clears the industry. */
  onIndustry: (row: ProjectRow, industry: string | null) => void;
  onHealthCheck: (row: ProjectRow) => void;
  onSettings: (row: ProjectRow) => void;
  /** Result of "Khám lại" per tenantId, shown on the row (T453). */
  checks?: Record<string, CheckState>;
}

const matches = (r: ProjectRow, q: string) => !q || `${r.name} ${r.industry ?? ""} ${r.slug}`.toLowerCase().includes(q);
const stepLabel = (k: BuildStepKey) => t(`studio.step.${k}.label` as I18nKey);

export function StageBoard(props: StageBoardProps) {
  const { rows, query } = props;
  const q = query.trim().toLowerCase();
  const shown = rows.filter((r) => matches(r, q));
  return (
    <article className="slds-card overflow-hidden" aria-label={t("studio.projects.boardLabel")}>
      <div className="-mr-px -mb-px grid grid-cols-4 max-lg:grid-cols-2 max-md:grid-cols-1">
        {STAGES.map((st) => {
          const list = shown.filter((r) => r.stage === st.key);
          return (
            <section key={st.key} aria-labelledby={`stage-${st.key}`} className="slds-border_right slds-border_bottom slds-p-around_medium min-w-0">
              <h2 id={`stage-${st.key}`} className="slds-text-heading_small slds-m-bottom_xx-small">
                <Tooltip trigger={<button type="button" className="slds-button_reset hover:underline">{t(st.name)}</button>}>{t(st.desc)}</Tooltip>{" "}
                <span className="slds-text-color_weak">{list.length}</span>
              </h2>
              {list.length === 0 ? (
                <p className="slds-text-color_weak slds-p-vertical_x-small">{q ? tf("studio.projects.noMatch", { q: query.trim() }) : t("studio.projects.empty")}</p>
              ) : (
                <ul>
                  {list.map((r) => (
                    <ProjectLine key={r.tenantId} row={r} {...props} />
                  ))}
                </ul>
              )}
            </section>
          );
        })}
      </div>
    </article>
  );
}

function ProjectLine({ row, onOpen, onOpenStep, onTrial, onIndustry, onHealthCheck, onSettings, checks }: StageBoardProps & { row: ProjectRow }) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const building = row.stage === "building" || row.stage === "trial";
  const steps = buildSteps(row, { home: false });
  const next = nextStep(steps);
  const nextState = steps.find((s) => s.key === next);

  const startEdit = () => {
    setDraft(row.industry ?? "");
    setEditing(true);
  };
  const save = (e?: FormEvent) => {
    e?.preventDefault();
    onIndustry(row, draft.trim() === "" ? null : draft.trim());
    setEditing(false);
  };
  const keys = (e: KeyboardEvent) => {
    if (e.key === "Escape") setEditing(false);
  };

  return (
    <li
      className="slds-p-vertical_small cursor-pointer"
      onClick={(e) => {
        // The name, the step link, the form and the menu have their own meaning; only the free part of the line opens the project.
        if (!(e.target as HTMLElement).closest("button, a, input, form, [role=menu], [role=menuitem]")) onOpen(row);
      }}
    >
      <div className="slds-grid slds-grid_align-spread">
        <div className="min-w-0">
          <button
            type="button"
            className="slds-button_reset slds-text-align_left hover:underline"
            style={{ fontSize: "var(--slds-g-font-scale-3)", fontWeight: "var(--slds-g-font-weight-7)" }}
            onClick={() => onOpen(row)}
          >
            {row.name}
          </button>
          {editing ? (
            <form className="slds-grid slds-grid_vertical-align-end slds-m-top_xx-small" onSubmit={save} onKeyDown={keys}>
              <Input
                label={tf("studio.projects.industry.label", { name: row.name })}
                labelHidden
                autoFocus
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
              />
              <Button variant="brand" type="submit" className="slds-m-left_x-small">{t("studio.projects.industry.save")}</Button>
              <Button variant="neutral" onClick={() => setEditing(false)} className="slds-m-left_xx-small">{t("studio.projects.industry.cancel")}</Button>
            </form>
          ) : (
            row.industry && <p className="slds-text-body_small slds-text-color_weak">{row.industry}</p>
          )}
        </div>
        <Menu align="right" trigger={<ButtonIcon icon={MoreHorizontal} label={tf("studio.projects.menu", { name: row.name })} variant="border-filled" more />}>
          {row.stage === "building" && <MenuItem label={t("studio.projects.action.startTrial")} onSelect={() => onTrial(row, true)} />}
          {row.stage === "trial" && <MenuItem label={t("studio.projects.action.backToBuilding")} onSelect={() => onTrial(row, false)} />}
          <MenuItem label={t("studio.projects.action.editIndustry")} onSelect={startEdit} />
          <MenuItem label={t("studio.projects.action.recheck")} onSelect={() => onHealthCheck(row)} />
          <MenuItem label={t("studio.projects.action.settings")} onSelect={() => onSettings(row)} />
        </Menu>
      </div>
      {checks?.[row.tenantId] && <CheckLine state={checks[row.tenantId]} />}
      {row.usableOwnerCount !== undefined && (
        <p className={`slds-text-body_small slds-m-top_xx-small ${row.usableOwnerCount === 0 ? "" : "slds-text-color_weak"}`}>
          {row.usableOwnerCount === 0 ? t("studio.attention.noOwner") : tf("studio.projects.owners", { n: row.usableOwnerCount })}
        </p>
      )}
      {building ? (
        <div className="slds-m-top_x-small">
          <BuildMeter steps={steps} />
          {next && nextState && (
            <p className="slds-m-top_x-small">
              {t("studio.projects.next")}{" "}
              <button type="button" className="slds-button slds-p-horizontal_none" onClick={() => onOpenStep(row, next)}>{stepLabel(next)}</button>
              {" · "}
              {nextState.sublabel}
            </p>
          )}
        </div>
      ) : (
        <p className="slds-m-top_x-small slds-text-color_weak">
          {row.lastActiveAt
            ? tf("studio.projects.usage", { n: row.counts.staffUsers, when: new Date(row.lastActiveAt).toLocaleDateString("vi-VN") })
            : tf("studio.projects.usageNever", { n: row.counts.staffUsers })}
        </p>
      )}
    </li>
  );
}

function CheckLine({ state }: { state: CheckState }) {
  const text =
    state.kind === "running"
      ? t("studio.projects.check.running")
      : state.kind === "failed"
        ? tf("studio.projects.check.failed", { why: state.why })
        : tf("studio.projects.check.done", {
            when: new Date(state.at).toLocaleTimeString("vi-VN", { hour: "2-digit", minute: "2-digit", hour12: false }),
            status: t(`studio.health.${state.status}` as I18nKey),
          });
  return (
    <p role="status" className="slds-text-body_small slds-text-color_weak slds-m-top_xx-small">
      {text}
    </p>
  );
}
