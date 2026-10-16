"use client";

// SLDS activity timeline (blueprint `activity-timeline/base`, SLDS 2.264.1):
//   ul.slds-timeline > li > div.slds-timeline__item_expandable.slds-timeline__item_<type>[.slds-is-open]
//     > span.slds-assistive-text(type) + div.slds-media
//       > div.slds-media__figure > button.slds-button_icon[aria-expanded][aria-controls] (switch glyph)
//                                + div.slds-icon_container.slds-icon-standard-<tile>.slds-timeline__icon
//       + div.slds-media__body > div.slds-timeline__trigger (checkbox? + h3 + meta icons | date + actions)
//                              + p (summary) + article.slds-timeline__item_details[hidden] | error line
// SLDS draws the vertical connector (`:before`) and colours it only for task/call/email/event, so
// `type` is limited to those; any other activity leaves it off and gets SLDS's grey line.
// Opening an item = slds-is-open + aria-expanded together; the blueprint omits aria-expanded while
// closed, here it is always set. Deviation: the details <article> carries no `hidden` attribute. In
// pure SLDS it is inert anyway (SLDS's normalize gives `article` display:block, which beats the UA
// [hidden] rule) and SLDS hides it with visibility/height 0 on `.slds-timeline__item_details` until
// `.slds-is-open` — visibility:hidden also drops it from the accessibility tree. Keeping `hidden`
// would let Tailwind preflight's `[hidden]{display:none!important}` collapse its 10px margin+border
// and pull closed items 10px tighter than the blueprint. The error line (a div, no SLDS rule) keeps
// `hidden`. An item with no details and no error has nothing to open: its toggle stays (it keeps the
// tile on the connector) but disabled.
// Glyphs are Lucide (sprite is CC BY-ND); the tile glyph is white like Icon.tsx tiles.
import clsx from "clsx";
import { ChevronDown, CircleX, type LucideIcon } from "lucide-react";
import { useId, useState, type ReactNode } from "react";
import { t } from "@/lib/i18n";

export type ActivityType = "task" | "call" | "email" | "event";

const TILE: Record<ActivityType, string> = { task: "task", call: "log-a-call", email: "email", event: "event" };

const TYPE_LABEL = {
  task: "slds.timeline.type.task",
  call: "slds.timeline.type.call",
  email: "slds.timeline.type.email",
  event: "slds.timeline.type.event",
} as const;

export interface ActivityTimelineItem {
  id: string;
  /** Colours the connector line and picks the tile colour. */
  type?: ActivityType;
  /** Tile glyph. */
  icon: LucideIcon;
  /** `slds-icon-standard-<iconName>` tile colour; default from `type`, else "default". */
  iconName?: string;
  /** Kind of activity for assistive tech and the tile title; default from `type`. */
  typeLabel?: string;
  title: string;
  href?: string;
  /** Pre-formatted date/time. */
  date: string;
  /** The "Bạn đã tạo công việc với …" line under the title. */
  summary?: ReactNode;
  /** Small utility glyphs after the title (recurring, attachment, public…). */
  meta?: { icon: LucideIcon; label: string }[];
  /** Task completion checkbox before the title. */
  task?: { checked: boolean; onChange: (checked: boolean) => void };
  /** Actions after the date, e.g. a <Menu> on an x-small border-filled icon button. */
  actions?: ReactNode;
  /** Two-column label/value list in the details box. */
  fields?: { label: string; value: ReactNode }[];
  /** Labelled paragraph under the fields (description, email body). */
  body?: { label: string; text: ReactNode };
  /** Shown instead of the details when they could not be loaded. */
  error?: string;
}

export interface ActivityTimelineProps {
  items: ActivityTimelineItem[];
  /** Controlled ids of expanded items. */
  expanded?: string[];
  defaultExpanded?: string[];
  onExpandedChange?: (expanded: string[]) => void;
  className?: string;
}

export function ActivityTimeline({ items, expanded, defaultExpanded = [], onExpandedChange, className }: ActivityTimelineProps) {
  const [inner, setInner] = useState(defaultExpanded);
  const open = expanded ?? inner;
  const toggle = (id: string) => {
    const next = open.includes(id) ? open.filter((x) => x !== id) : [...open, id];
    setInner(next);
    onExpandedChange?.(next);
  };
  return (
    <ul className={clsx("slds-timeline", className)}>
      {items.map((item) => (
        <li key={item.id}>
          <TimelineItem item={item} open={open.includes(item.id)} onToggle={() => toggle(item.id)} />
        </li>
      ))}
    </ul>
  );
}

function TimelineItem({ item, open, onToggle }: { item: ActivityTimelineItem; open: boolean; onToggle: () => void }) {
  const id = useId();
  const { type, icon: Glyph, title, href, date, summary, meta, task, actions, fields, body, error } = item;
  const typeLabel = item.typeLabel ?? (type ? t(TYPE_LABEL[type]) : "");
  const expandable = Boolean(error || fields?.length || body);
  const toggleLabel = `${t("slds.timeline.toggleDetails")} ${title}`;
  const heading = <strong>{title}</strong>;
  return (
    <div className={clsx("slds-timeline__item_expandable", type && `slds-timeline__item_${type}`, open && expandable && "slds-is-open")}>
      {typeLabel && <span className="slds-assistive-text">{typeLabel}</span>}
      <div className="slds-media">
        <div className="slds-media__figure">
          <button
            type="button"
            className="slds-button slds-button_icon"
            title={toggleLabel}
            aria-controls={expandable ? id : undefined}
            aria-expanded={expandable ? open : undefined}
            disabled={!expandable}
            onClick={onToggle}
          >
            <ChevronDown aria-hidden="true" className="slds-button__icon slds-timeline__details-action-icon nx-lucide" />
            <span className="slds-assistive-text">{toggleLabel}</span>
          </button>
          <div
            className={clsx("slds-icon_container slds-timeline__icon", `slds-icon-standard-${item.iconName ?? (type ? TILE[type] : "default")}`)}
            title={typeLabel || undefined}
          >
            <Glyph aria-hidden="true" className="slds-icon slds-icon_small nx-lucide nx-lucide_tile" />
          </div>
        </div>
        <div className="slds-media__body">
          <div className="slds-grid slds-grid_align-spread slds-timeline__trigger">
            <div className="slds-grid slds-grid_vertical-align-center slds-truncate_container_75 slds-no-space">
              {task && (
                <div className="slds-checkbox">
                  <input type="checkbox" id={`${id}-done`} checked={task.checked} onChange={(e) => task.onChange(e.target.checked)} />
                  <label className="slds-checkbox__label" htmlFor={`${id}-done`}>
                    <span className="slds-checkbox_faux" />
                    <span className="slds-form-element__label slds-assistive-text">{`${t("slds.timeline.markComplete")} ${title}`}</span>
                  </label>
                </div>
              )}
              <h3 className="slds-truncate" title={title}>
                {href ? <a href={href}>{heading}</a> : heading}
              </h3>
              {meta && meta.length > 0 && (
                <div className="slds-no-flex">
                  {meta.map(({ icon: MetaGlyph, label }) => (
                    <span key={label} className="slds-icon_container" title={label}>
                      <MetaGlyph aria-hidden="true" className="slds-icon slds-icon_xx-small slds-icon-text-default slds-m-left_x-small nx-lucide" />
                      <span className="slds-assistive-text">{label}</span>
                    </span>
                  ))}
                </div>
              )}
            </div>
            <div className="slds-timeline__actions slds-timeline__actions_inline">
              <p className="slds-timeline__date">{date}</p>
              {actions}
            </div>
          </div>
          {summary != null && <p className="slds-m-horizontal_xx-small">{summary}</p>}
          {error ? (
            <div className="slds-grid slds-text-color_error slds-m-top_x-small slds-m-horizontal_xx-small" id={id} hidden={!open}>
              <span className="slds-icon_container slds-icon-utility-error" title={t("slds.timeline.error")}>
                <CircleX aria-hidden="true" className="slds-icon slds-icon-text-error slds-icon_x-small slds-m-right_xx-small nx-lucide" />
                <span className="slds-assistive-text">{t("slds.timeline.error")}</span>
              </span>
              <p>{error}</p>
            </div>
          ) : (
            expandable && (
              <article
                className={clsx(
                  "slds-box slds-timeline__item_details slds-theme_shade slds-m-top_x-small slds-m-horizontal_xx-small",
                  open && "slds-p-around_medium",
                )}
                id={id}
              >
                {fields && fields.length > 0 && (
                  <ul className="slds-list_horizontal slds-wrap">
                    {fields.map((f) => (
                      <li key={f.label} className="slds-grid slds-grid_vertical slds-size_1-of-2 slds-p-bottom_small">
                        <span className="slds-text-title slds-p-bottom_x-small">{f.label}</span>
                        <span className="slds-text-body_medium slds-truncate" title={typeof f.value === "string" ? f.value : undefined}>
                          {f.value}
                        </span>
                      </li>
                    ))}
                  </ul>
                )}
                {body && (
                  <div>
                    <span className="slds-text-title">{body.label}</span>
                    <p className="slds-p-top_x-small">{body.text}</p>
                  </div>
                )}
              </article>
            )
          )}
        </div>
      </div>
    </div>
  );
}
