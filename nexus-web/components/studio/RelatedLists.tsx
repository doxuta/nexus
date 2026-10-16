"use client";

// RelatedLists — the right column of a Studio table page (specs/074 lát 4): flat sections, each with its OWN
// loading / error / empty / data state, so one failing request marks only its section. `slds-has-dividers_bottom`
// rows, no nested card (the page is already inside the frame's surface).
import type { ReactNode } from "react";
import { Button } from "@/components/slds/Button";
import { Spinner } from "@/components/slds/Spinner";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { t } from "@/lib/i18n";

export interface RelatedSection {
  id: string;
  title: string;
  state: "loading" | "error" | "ready";
  count?: number;
  items: ReactNode[];
  /** Sentence shown when `ready` and `items` is empty. */
  empty: string;
  error?: string;
  onRetry?: () => void;
  /** A link under the list (e.g. "Sơ đồ liên kết"). */
  action?: ReactNode;
}

export function RelatedLists({ label, sections }: { label: string; sections: readonly RelatedSection[] }) {
  return (
    <aside aria-label={label} className="slds-border_left lg:w-[300px] lg:shrink-0 slds-p-left_medium">
      {sections.map((s) => (
        <section key={s.id} className="slds-m-bottom_large">
          <h3 className="slds-text-title_caps slds-m-bottom_x-small">
            {s.title}
            {s.state === "ready" && s.count !== undefined && <span className="slds-m-left_xx-small slds-text-color_weak">({s.count})</span>}
          </h3>
          {s.state === "loading" && (
            <div className="slds-is-relative slds-p-around_medium"><Spinner size="small" /></div>
          )}
          {s.state === "error" && (
            <div>
              <ScopedNotification theme="error">{s.error}</ScopedNotification>
              {s.onRetry && <div className="slds-m-top_x-small"><Button onClick={s.onRetry}>{t("studio.query.retry")}</Button></div>}
            </div>
          )}
          {s.state === "ready" && (s.items.length === 0
            ? <p className="slds-text-color_weak">{s.empty}</p>
            : <ul className="slds-has-dividers_bottom">{s.items.map((it, i) => <li key={i} className="slds-item">{it}</li>)}</ul>)}
          {s.action && s.state !== "loading" && <p className="slds-m-top_x-small">{s.action}</p>}
        </section>
      ))}
    </aside>
  );
}
