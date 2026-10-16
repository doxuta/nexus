"use client";

// Focus and document title for the project frame (specs/073 FR-017, FR-019; architecture 4.9 amended).
// Two focus targets, because the H1 (project name) sits above ~12 tab stops (switcher, zones, steps):
//   • the project changed, or we just came from outside a project  → the H1
//   • the page changed inside the same project                      → the first h2 of the work surface (`tabIndex=-1`)
// so a keyboard user who presses a step lands in the content, not back in the header, and a screen reader reads the
// new page's name rather than the project's again. Nothing is stolen on the first mount while the user is typing.
import { useSearchParams } from "next/navigation";
import { useEffect, useRef, type RefObject } from "react";
import { t } from "@/lib/i18n";

const isEditable = (el: Element | null): boolean =>
  !!el && (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || el instanceof HTMLSelectElement || (el as HTMLElement).isContentEditable);

// One-shot flag (specs/076 FR-072): the assistant navigated to show a preview, so the next page change must not pull
// focus out of the assistant's card.
let skipRouteFocus = false;
export const skipNextRouteFocus = (): void => { skipRouteFocus = true; };

/** `ready` = the frame has drawn its real h1/surface (identity answered); before that there is nothing worth focusing. */
export function useStudioRouteFocus(root: RefObject<HTMLElement | null>, slug: string, routeKey: string, ready = true): void {
  const last = useRef<{ slug: string; routeKey: string } | null>(null);
  useEffect(() => {
    if (!ready) return;
    const prev = last.current;
    last.current = { slug, routeKey };
    const host = root.current;
    if (!host) return;
    if (!prev && isEditable(document.activeElement)) return; // first mount, the user is already in a field
    if (prev && prev.slug === slug && prev.routeKey === routeKey) return;
    if (prev && prev.slug === slug && prev.routeKey === "") return; // the project root redirecting to its first page is not a page change
    if (skipRouteFocus) { skipRouteFocus = false; return; }
    const toH1 = !prev || prev.slug !== slug;
    const target = toH1
      ? host.querySelector<HTMLElement>("h1")
      : (host.querySelector<HTMLElement>('main h2[tabindex="-1"]') ?? host.querySelector<HTMLElement>("main"));
    if (!target) return;
    if (!target.hasAttribute("tabindex")) target.setAttribute("tabindex", "-1");
    target.focus({ preventScroll: true });
  }, [root, slug, routeKey, ready]);
}

/** "{page} · {project} · Nexus Studio"; restores the previous title when the frame goes away. */
export function useStudioDocumentTitle(page: string, project: string): void {
  // A `router.replace` that only changes the query (`?truong=`) makes Next put its own default title back; the query
  // string is a dependency so the title is written again after it.
  const search = useSearchParams().toString();
  useEffect(() => {
    const before = document.title;
    document.title = [page, project, t("page.studio.shell.name")].filter(Boolean).join(" · ");
    return () => {
      document.title = before;
    };
  }, [page, project, search]);
}
