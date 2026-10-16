// BrowserFrame — minimal "browser window" chrome around a REAL product screenshot.
// Anti-slop: no colored traffic-lights, no glow — the dots + address pill are drawn in CSS
// with the existing neutral tokens (--bd*, --panel2, --bg) only; radius stays on --r-card.
// Server component (no hooks) — safe inside SSG landing and the (auth) layout.
import { clsx } from "clsx";

export function BrowserFrame({
  children,
  className,
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={clsx(
        "overflow-hidden rounded-card border border-bd bg-panel shadow-pop",
        className,
      )}
    >
      {/* Chrome bar: 3 muted dots left + faux address pill centered — pure decoration */}
      <div className="relative flex h-9 items-center border-b border-bd bg-panel2 px-3.5" aria-hidden="true">
        <span className="flex items-center gap-1.5">
          <span className="size-2.5 rounded-full bg-bd3/50" />
          <span className="size-2.5 rounded-full bg-bd3/50" />
          <span className="size-2.5 rounded-full bg-bd3/50" />
        </span>
        <span className="absolute left-1/2 top-1/2 h-5 w-40 -translate-x-1/2 -translate-y-1/2 rounded-pill border border-bd2 bg-bg" />
      </div>
      {children}
    </div>
  );
}
