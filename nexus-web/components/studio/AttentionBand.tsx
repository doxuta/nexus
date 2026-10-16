"use client";

// The navy "Cần anh xem" band (specs/073 FR-091, architecture 3.2): the one bold thing on the project board.
// Only drawn when there is something to look at. One column per project, lines already ordered by `attentionItems`.
// Under 768px only two projects show until "Xem thêm" (CSS only: `max-md:hidden`, so no resize listener).
// The band does not navigate: the page decides what "open project" / an action means (enter the project, then the route).
import clsx from "clsx";
import { useState } from "react";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import type { AttentionItem } from "@/lib/projectSignals";

const COLLAPSED_PROJECTS = 2;
const LEVEL_KEY: Record<AttentionItem["level"], I18nKey> = {
  error: "studio.attention.level.error",
  warning: "studio.attention.level.warning",
  info: "studio.attention.level.info",
};

interface Props {
  items: AttentionItem[];
  onOpenProject: (slug: string) => void;
  onAction: (item: AttentionItem) => void;
}

export function AttentionBand({ items, onOpenProject, onAction }: Props) {
  const [all, setAll] = useState(false);
  if (items.length === 0) return null;
  // Group by project, keeping first-appearance order (the items are sorted by severity, so the worst project leads).
  const groups = new Map<string, { name: string; items: AttentionItem[] }>();
  for (const it of items) {
    const g = groups.get(it.projectSlug) ?? { name: it.projectName, items: [] };
    g.items.push(it);
    groups.set(it.projectSlug, g);
  }
  const hidden = Math.max(0, groups.size - COLLAPSED_PROJECTS);
  return (
    <section aria-labelledby="attention-band-title" className="slds-theme_alt-inverse slds-p-horizontal_large slds-p-top_medium slds-p-bottom_x-small slds-m-bottom_large" style={{ borderRadius: "var(--slds-g-radius-border-3)" }}>
      <div className="slds-grid slds-grid_vertical-align-end">
        <h2 id="attention-band-title" className="slds-text-heading_medium slds-m-right_small">
          {t("studio.attention.title")}
        </h2>
        <span className="slds-text-color_inverse-weak">{tf("studio.attention.count", { n: groups.size })}</span>
      </div>
      <ul className="slds-m-top_small grid gap-x-10 [grid-template-columns:repeat(auto-fit,minmax(min(17.5rem,100%),1fr))]">
        {[...groups].map(([slug, g], i) => (
          <li key={slug} data-project={slug} className={clsx("slds-border_top slds-p-vertical_small min-w-0", i >= COLLAPSED_PROJECTS && !all && "max-md:hidden")}>
            <button type="button" className="slds-button slds-text-heading_small slds-text-color_inverse" onClick={() => onOpenProject(slug)}>
              {g.name}
            </button>
            {g.items.map((it, j) => (
              <div key={j} className="slds-grid slds-wrap slds-grid_vertical-align-start slds-m-top_xx-small slds-text-color_inverse-weak">
                <span className={clsx("nx-dot slds-m-right_x-small slds-m-top_xx-small slds-shrink-none", `nx-dot_${it.level}`)} aria-hidden="true" />
                <span className="slds-assistive-text">{t(LEVEL_KEY[it.level])}</span>
                <span className="min-w-0 flex-1 basis-32 slds-m-right_small">{it.text}</span>
                <button type="button" className="slds-button slds-button_inverse slds-no-flex" onClick={() => onAction(it)}>
                  {it.action.label}
                </button>
              </div>
            ))}
          </li>
        ))}
      </ul>
      {hidden > 0 && (
        <button type="button" className="slds-button slds-button_inverse slds-m-vertical_x-small md:hidden" aria-expanded={all} onClick={() => setAll(!all)}>
          {all ? t("studio.attention.less") : tf("studio.attention.more", { n: hidden })}
        </button>
      )}
    </section>
  );
}
