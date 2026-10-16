"use client";

// SLDS path (blueprint `path/base`, SLDS 2.264.1): default, later-stage, different-stage-selected,
// with-coaching, different-coaching, lost, won. Hand-rolled because the roles switch with coaching
// (path docs): without coaching the nav is a listbox of options, with coaching a tablist of tabs
// that each carry aria-expanded for the coaching area. Left/Right move focus and selection (roving
// tabindex); the selection stays on the first/last stage at the ends.
// The main button follows the blueprint states: "mark complete" on the current stage, "mark as
// current" on another stage, "change closed state" on a closed (won/lost) current stage.
// `mode="steps"` (specs/073): the Studio build-steps bar — same `slds-path__nav` track, but a real <nav>
// of links (no listbox/tablist, no main button, no won/lost). Every step is a link reached with Tab; Left/Right
// are an extra. A step is `complete` true/false (hidden text) or null (unknown: no tick, no text).
// Not wrapped: the with-overflow scroll buttons and the stage tooltip (no caller yet).
import clsx from "clsx";
import { Check, ChevronRight } from "lucide-react";
import Link from "next/link";
import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { revealInScroller } from "./revealInScroller";

export interface PathStage {
  value: string;
  label: string;
}

export interface PathCoaching {
  /** "Key Fields This Stage", read-only values. */
  keyFields?: { label: string; value: ReactNode }[];
  /** "Guidance for Success" long-form content. */
  guidance?: ReactNode;
}

export type PathAction = "complete" | "setCurrent" | "changeClosed";

export interface PathStep {
  key: string;
  label: string;
  sublabel?: string;
  /** true = enough, false = missing, null = not known (no tick, no hidden text). */
  complete: boolean | null;
  href: string;
}

export interface PathStepsProps {
  mode: "steps";
  steps: PathStep[];
  /** Key of the open step (the one navy segment). */
  current: string;
  /** Accessible name of the `nav` — required, a page has several. */
  label: string;
  className?: string;
}

export type PathProps = (PathStagesProps & { mode?: "stages" }) | PathStepsProps;

export interface PathStagesProps {
  stages: PathStage[];
  /** Value of the stage the record is in. */
  current: string;
  /** The current stage closes the process. Lost leaves earlier stages incomplete. */
  closed?: "won" | "lost";
  /** Stage coaching; when given, the path gets the collapsible coaching area. */
  coaching?: (stage: PathStage) => PathCoaching | undefined;
  onEditKeyFields?: (stage: PathStage) => void;
  /** Main button handler, called with the selected stage. Omit for a read-only path. */
  onAction?: (action: PathAction, stage: PathStage) => void;
  defaultCoachingOpen?: boolean;
  className?: string;
}

const ACTION_LABEL = {
  complete: "slds.path.markComplete",
  setCurrent: "slds.path.markCurrent",
  changeClosed: "slds.path.changeClosed",
} as const;

function PathStages({
  stages,
  current,
  closed,
  coaching,
  onEditKeyFields,
  onAction,
  defaultCoachingOpen = false,
  className,
}: PathStagesProps) {
  const [picked, setPicked] = useState<string | null>(null);
  const [open, setOpen] = useState(defaultCoachingOpen);
  const links = useRef<(HTMLAnchorElement | null)[]>([]);
  const stageNameId = useId();
  const contentId = useId();

  const currentIndex = stages.findIndex((s) => s.value === current);
  const selectedIndex = Math.max(0, stages.findIndex((s) => s.value === (picked ?? current)));
  const selected = stages[selectedIndex];
  const hasCoaching = Boolean(coaching);
  const action: PathAction =
    selectedIndex !== currentIndex ? "setCurrent" : closed ? "changeClosed" : "complete";

  const move = (e: KeyboardEvent<HTMLUListElement>) => {
    if (e.altKey || e.metaKey || e.ctrlKey || e.shiftKey) return; // keep browser Back/Forward and word-jump shortcuts
    const step = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const next = Math.min(stages.length - 1, Math.max(0, selectedIndex + step));
    setPicked(stages[next].value);
    links.current[next]?.focus();
  };

  const stageCoaching = coaching && selected ? coaching(selected) : undefined;

  return (
    <div className={clsx("slds-path", hasCoaching && "slds-path_has-coaching", hasCoaching && open && "slds-is-expanded", className)}>
      <div className="slds-grid slds-path__track">
        <div className="slds-grid slds-path__scroller-container">
          {hasCoaching && (
            <button
              type="button"
              className={clsx("slds-button slds-button_icon slds-button_icon-border-filled slds-path__trigger", open && "slds-path__trigger_open")}
              title={t("slds.path.toggleCoaching")}
              aria-expanded={open}
              aria-controls={contentId}
              onClick={() => setOpen(!open)}
            >
              <ChevronRight aria-hidden="true" className="slds-button__icon nx-lucide" />
              <span className="slds-assistive-text">{t("slds.path.showDetails")}</span>
            </button>
          )}
          <div className="slds-path__scroller">
            <div className="slds-path__scroller_inner">
              <ul
                className="slds-path__nav"
                role={hasCoaching ? "tablist" : "listbox"}
                aria-orientation="horizontal"
                aria-labelledby={stageNameId}
                onKeyDown={move}
              >
                {stages.map((stage, i) => {
                  const state =
                    i === currentIndex ? "current" : i < currentIndex && closed !== "lost" ? "complete" : "incomplete";
                  const isSelected = i === selectedIndex;
                  return (
                    <li
                      key={stage.value}
                      className={clsx(
                        "slds-path__item",
                        `slds-is-${state}`,
                        state === "current" && closed && `slds-is-${closed}`,
                        isSelected && "slds-is-active",
                      )}
                      role="presentation"
                    >
                      <a
                        ref={(el) => {
                          links.current[i] = el;
                        }}
                        className="slds-path__link"
                        role={hasCoaching ? "tab" : "option"}
                        aria-selected={isSelected}
                        aria-expanded={hasCoaching ? open : undefined}
                        tabIndex={isSelected ? 0 : -1}
                        onClick={() => setPicked(stage.value)}
                      >
                        <span className="slds-path__stage">
                          <Check aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
                          {state !== "incomplete" && (
                            <span className="slds-assistive-text">
                              {t(state === "current" ? "slds.path.currentStage" : "slds.path.stageComplete")}
                            </span>
                          )}
                        </span>
                        <span className="slds-path__title">{stage.label}</span>
                      </a>
                    </li>
                  );
                })}
              </ul>
            </div>
          </div>
        </div>
        <div className="slds-grid slds-path__action">
          <span id={stageNameId} className="slds-path__stage-name">
            {t("slds.path.stage")} {stages[currentIndex]?.label}
          </span>
          {onAction && selected && (
            <button
              type="button"
              className="slds-button slds-button_brand slds-path__mark-complete"
              onClick={() => {
                onAction(action, selected);
                setPicked(null);
              }}
            >
              {action === "complete" && (
                <Check aria-hidden="true" className="slds-button__icon slds-button__icon_left nx-lucide" />
              )}
              {t(ACTION_LABEL[action])}
            </button>
          )}
          {hasCoaching && (
            <button
              type="button"
              className="slds-button slds-button_neutral slds-path__trigger-coaching-content"
              aria-expanded={open}
              aria-controls={contentId}
              onClick={() => setOpen(!open)}
            >
              {t(open ? "slds.path.showLess" : "slds.path.showMore")}
            </button>
          )}
        </div>
      </div>
      {hasCoaching && (
        <div className={clsx("slds-path__content", !open && "slds-is-collapsed")} id={contentId}>
          <div className="slds-path__coach slds-grid">
            <div className="slds-path__keys">
              <div className="slds-grid slds-grid_align-spread slds-path__coach-title">
                <h2>{t("slds.path.keyFields")}</h2>
                {onEditKeyFields && selected && (
                  <button
                    type="button"
                    className="slds-button slds-path__coach-edit slds-text-body_small"
                    onClick={() => onEditKeyFields(selected)}
                  >
                    {t("slds.path.edit")}
                  </button>
                )}
              </div>
              <div className="slds-form" role="list">
                {stageCoaching?.keyFields?.map((field) => (
                  <div key={field.label} className="slds-form__row">
                    <div className="slds-form__item" role="listitem">
                      <div className="slds-form-element slds-form-element_readonly slds-form-element_stacked slds-hint-parent">
                        <span className="slds-form-element__label">{field.label}</span>
                        <div className="slds-form-element__control">
                          <div className="slds-form-element__static">{field.value}</div>
                        </div>
                      </div>
                    </div>
                  </div>
                ))}
              </div>
            </div>
            <div className="slds-path__guidance">
              <h2 className="slds-path__coach-title">{t("slds.path.guidance")}</h2>
              <div className="slds-text-longform slds-path__guidance-content">{stageCoaching?.guidance}</div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

export function Path(props: PathProps) {
  return props.mode === "steps" ? <PathSteps {...props} /> : <PathStages {...props} />;
}

function PathSteps({ steps, current, label, className }: PathStepsProps) {
  const links = useRef<(HTMLAnchorElement | null)[]>([]);
  const currentIndex = steps.findIndex((s) => s.key === current);
  const reveal = (i: number) => revealInScroller(links.current[i], links.current[i]?.closest<HTMLElement>(".slds-path__scroller"));
  // The strip scrolls inside itself (narrow screens): keep the open step in view.
  useEffect(() => {
    if (currentIndex >= 0) reveal(currentIndex);
  }, [currentIndex]);
  const move = (e: KeyboardEvent<HTMLUListElement>) => {
    if (e.altKey || e.metaKey || e.ctrlKey || e.shiftKey) return; // keep browser Back/Forward and word-jump shortcuts
    const step = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1 : 0;
    const at = links.current.findIndex((a) => a === document.activeElement);
    if (!step || at < 0) return;
    e.preventDefault();
    links.current[Math.min(steps.length - 1, Math.max(0, at + step))]?.focus();
  };
  return (
    <nav aria-label={label} className={clsx("slds-path nx-path-steps", className)}>
      <div className="slds-path__scroller overflow-x-auto">
        <ul className="slds-path__nav" onKeyDown={move}>
          {steps.map((step, i) => {
            const open = i === currentIndex;
            return (
              <li
                key={step.key}
                className={clsx(
                  "slds-path__item",
                  open ? "slds-is-current slds-is-active" : step.complete === true ? "slds-is-complete" : "slds-is-incomplete",
                )}
              >
                <Link
                  ref={(el) => {
                    links.current[i] = el;
                  }}
                  href={step.href}
                  className="slds-path__link"
                  aria-current={open ? "step" : undefined}
                  onFocus={() => reveal(i)}
                >
                  <span className="slds-path__stage">
                    {step.complete === true && <Check aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />}
                  </span>
                  <span className="slds-path__title">
                    {step.label}
                    {step.sublabel && <span className="slds-text-body_small slds-m-left_xx-small">{step.sublabel}</span>}
                    {step.complete !== null && (
                      <span className="slds-assistive-text">{t(step.complete ? "studio.a11y.stepDone" : "studio.a11y.stepTodo")}</span>
                    )}
                  </span>
                </Link>
              </li>
            );
          })}
        </ul>
      </div>
    </nav>
  );
}
