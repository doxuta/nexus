"use client";

// Embedded work surface (specs/073 FR-005, UI-ARCH D-UI-27). A builder mounted inside the Studio frame
// must not repeat the frame: no card in a card, no second page title, no icon tile, no chip per row.
// Wrappers (PageHeader, Card, Panel, Tabs, Badge, Avatar, EmptyState; ObjectHomePage joins in F6) read
// useEmbedded() and draw their flat form; outside an EmbeddedSurface they are byte-identical to before.
// EmbeddedSurfaceReset is the way out for a subtree that must look like the customer's screen.
import { createContext, useContext, type ReactNode } from "react";

const EmbeddedContext = createContext(false);

export function useEmbedded(): boolean {
  return useContext(EmbeddedContext);
}

// `canvas` (specs/073 T099): old builders such as the former xyflow relationship designer assume a shell as tall as the
// viewport (`flex min-h-0 flex-1` + `h-full`), but the work surface scrolls with the page and the canvas collapsed
// to 0px. The shell gives them a floor instead of editing the builder: the viewport minus the sticky stack
// (--nx-sticky-stack, set by the frame; 0 until then), as a flex column the builder's root fills. Which builders
// need it is the `canvas` flag of test/studio-embedded-registry.tsx.
export function EmbeddedSurface({ value = true, canvas = false, children }: { value?: boolean; canvas?: boolean; children: ReactNode }) {
  const body = canvas ? (
    <div className="flex flex-col" style={{ minHeight: "calc(100dvh - var(--nx-sticky-stack, 0px))" }}>
      {children}
    </div>
  ) : (
    children
  );
  return <EmbeddedContext.Provider value={value}>{body}</EmbeddedContext.Provider>;
}

export function EmbeddedSurfaceReset({ children }: { children: ReactNode }) {
  return <EmbeddedContext.Provider value={false}>{children}</EmbeddedContext.Provider>;
}
