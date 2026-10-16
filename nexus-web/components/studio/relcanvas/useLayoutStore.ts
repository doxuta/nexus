"use client";

// Canvas layout store — zustand + zundo temporal (W2 §3.1). Scope is LAYOUT ONLY (node
// positions): schema mutations are live server writes guarded by confirm dialogs and are
// deliberately NOT undoable (honest state model — Cmd+Z must never "un-delete" a relationship
// it cannot restore). One temporal entry per drag-stop / auto-layout, capped at 50.

import { create } from "zustand";
import { temporal } from "zundo";
import { useStore as useZustandStore } from "zustand";
import type { XY } from "@/lib/relCanvas";

interface LayoutState {
  positions: Record<string, XY>;
  /** Bulk replace (initial load, dagre auto-layout) — ONE undo entry. */
  setPositions: (p: Record<string, XY>) => void;
  /** Commit a single node's position (onNodeDragStop) — ONE undo entry per drag. */
  moveNode: (id: string, xy: XY) => void;
}

export const useLayoutStore = create<LayoutState>()(
  temporal(
    (set) => ({
      positions: {},
      setPositions: (positions) => set({ positions }),
      moveNode: (id, xy) =>
        set((s) => ({ positions: { ...s.positions, [id]: xy } })),
    }),
    {
      partialize: (s) => ({ positions: s.positions }),
      limit: 50,
      // Skip no-op snapshots (drag that ends where it started).
      equality: (a, b) => JSON.stringify(a.positions) === JSON.stringify(b.positions),
    },
  ),
);

/** Reactive view over the temporal (undo/redo) store — zundo's temporal store is a vanilla
 *  zustand store, so components subscribe through useStore(). */
export function useTemporal<T>(selector: (s: {
  pastStates: unknown[];
  futureStates: unknown[];
  undo: (n?: number) => void;
  redo: (n?: number) => void;
  clear: () => void;
  pause: () => void;
  resume: () => void;
}) => T): T {
  return useZustandStore(useLayoutStore.temporal, selector as never) as T;
}

/** Load positions WITHOUT recording an undo entry (initial hydrate / server refresh). */
export function hydratePositions(positions: Record<string, XY>) {
  const t = useLayoutStore.temporal.getState();
  t.pause();
  useLayoutStore.getState().setPositions(positions);
  t.resume();
  t.clear(); // fresh history — you can't undo into a previous schema's layout
}
