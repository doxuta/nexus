"use client";

// Width of the WORK SURFACE (not the window): the assistant dock narrows it, so a 1280px window can still need the
// stacked layout (specs/075 FR-105). stacked = width < 1000.
import { useCallback, useLayoutEffect, useRef, useState } from "react";

export const STACK_BELOW = 1000;

export function useSurfaceWidth(): { ref: (el: HTMLElement | null) => void; width: number; stacked: boolean } {
  const [width, setWidth] = useState(0);
  const ro = useRef<ResizeObserver | null>(null);
  const el = useRef<HTMLElement | null>(null);
  const ref = useCallback((node: HTMLElement | null) => {
    ro.current?.disconnect();
    el.current = node;
    if (!node) return;
    setWidth(node.getBoundingClientRect().width);
    if (typeof ResizeObserver === "undefined") return;
    ro.current = new ResizeObserver((entries) => setWidth(entries[0]?.contentRect.width ?? node.getBoundingClientRect().width));
    ro.current.observe(node);
  }, []);
  useLayoutEffect(() => () => ro.current?.disconnect(), []);
  // width 0 = not measured yet (or jsdom): behave as wide so the first paint is the two-column layout
  return { ref, width, stacked: width > 0 && width < STACK_BELOW };
}
