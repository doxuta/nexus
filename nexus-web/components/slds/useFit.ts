"use client";

// Measuring half of an SLDS overflow ("Thêm") row — global navigation bar, tabs.
import { useEffect, useState, type RefObject } from "react";

/**
 * How many cells fit the bar, measured on a hidden copy of the full row whose last cell is the
 * "Thêm" item. ResizeObserver reports once on observe and again whenever the bar or the row
 * changes size, so there is no synchronous measuring in render. Infinity = everything fits.
 * No ghost row rendered (ghost.current null) = nothing to measure = Infinity.
 */
export function useFit(bar: RefObject<HTMLElement | null>, ghost: RefObject<HTMLElement | null>): number {
  const [fit, setFit] = useState(Infinity);
  useEffect(() => {
    const barEl = bar.current;
    const row = ghost.current;
    if (!barEl || !row || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => {
      const widths = Array.from(row.children, (c) => (c as HTMLElement).offsetWidth);
      const more = widths.pop() ?? 0;
      const avail = barEl.clientWidth;
      if (widths.reduce((a, b) => a + b, 0) <= avail) return setFit(Infinity);
      let used = more;
      let n = 0;
      while (n < widths.length && used + widths[n] <= avail) used += widths[n++];
      setFit(n);
    });
    ro.observe(barEl);
    ro.observe(row);
    return () => ro.disconnect();
  }, [bar, ghost]);
  return fit;
}
