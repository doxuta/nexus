"use client";

// Record page template (specs/038 contracts/shell.md "Template trang", RESEARCH §5.5 #2).
// Presentation only: no fetching, no metadata — the L2 detail renderer (LayoutDetail) supplies the
// highlights, stages, tab contents and right-column cards during the Phase 2 migration.
// Layout, all SLDS 2.264.1:
//   PageHeader record-home (page-headers/record-home): icon, object type, record name, action
//     group + overflow menu, detail-row highlights
//   Path (path/base) in a card — only when the record has stages
//   div.slds-grid.slds-wrap > col 2/3: div.slds-tabs_card > Tabs (tabs/base; the tabs that do not fit
//                                     the width go into the blueprint's overflow "Thêm" tab)
//                           > col 1/3: aside slot (related-list cards, notes)
// `slds-large-size_*` switches at SLDS's "large" breakpoint (64em = 1024px), so the page falls back
// to one column below that. Every region carries the same top margin, so the stacked columns keep
// the same rhythm as the side-by-side ones.
import clsx from "clsx";
import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import type { BreadcrumbItem } from "../Breadcrumbs";
import { PageHeader, type PageHeaderDetail } from "../PageHeader";
import { Path, type PathStagesProps } from "../Path";
import { Tabs, type TabItem } from "../Tabs";
import { ActionGroup, type PageAction } from "./ActionGroup";

export interface RecordHomePageProps {
  icon: { glyph: LucideIcon; name: string };
  /** Object type above the name, e.g. "Khách hàng". */
  objectLabel: string;
  /** Record name. */
  title: string;
  /** Record actions: the first `maxVisibleActions` are buttons, the rest go into the overflow menu. */
  actions?: PageAction[];
  maxVisibleActions?: number;
  /** Highlight fields (compact layout). */
  details?: PageHeaderDetail[];
  /** Parent trail above the name. */
  breadcrumbs?: BreadcrumbItem[];
  /** Stage path; rendered only when it has stages. */
  path?: PathStagesProps;
  tabs: TabItem[];
  tab?: string;
  defaultTab?: string;
  onTabChange?: (value: string) => void;
  /** Accessible name of the tablist. */
  tabsLabel?: string;
  /** Right column (related lists, notes). */
  aside?: ReactNode;
  className?: string;
}

export function RecordHomePage({
  icon,
  objectLabel,
  title,
  actions,
  maxVisibleActions = 3,
  details,
  breadcrumbs,
  path,
  tabs,
  tab,
  defaultTab,
  onTabChange,
  tabsLabel,
  aside,
  className,
}: RecordHomePageProps) {
  return (
    <div className={className}>
      <PageHeader
        variant="record-home"
        icon={icon}
        label={objectLabel}
        title={title}
        breadcrumbs={breadcrumbs}
        details={details}
        actions={actions && actions.length > 0 ? [<ActionGroup key="actions" actions={actions} maxVisible={maxVisibleActions} />] : undefined}
      />
      {path && path.stages.length > 0 && (
        <div className="slds-card slds-m-top_medium">
          <div className="slds-card__body slds-card__body_inner">
            <Path {...path} />
          </div>
        </div>
      )}
      <div className="slds-grid slds-wrap slds-gutters_x-small">
        <div className={clsx("slds-col slds-size_1-of-1 slds-m-top_medium", aside && "slds-large-size_2-of-3")}>
          <div className="slds-tabs_card">
            <Tabs items={tabs} value={tab} defaultValue={defaultTab} onValueChange={onTabChange} label={tabsLabel} maxVisible="auto" />
          </div>
        </div>
        {aside && <div className="slds-col slds-size_1-of-1 slds-large-size_1-of-3 slds-m-top_medium">{aside}</div>}
      </div>
    </div>
  );
}
