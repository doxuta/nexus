"use client";

// Timeline — Nexus Kit primitive (W6, plan §4.2 "Timeline (audit/approval)").
// Vertical rail + tone-tinted circular markers; token-only (no raw colors), density compact.
// Used by: approval detail (/approvals/[id]), LayoutDetail "Duyệt" tab; audit "Hoạt động" (W7).
// Motion: none (static list — MOTION dial 2); rail hides itself on the last item via group-last.

import { clsx } from "clsx";
import type { LucideIcon } from "lucide-react";

// Marker tones reuse the StatusBadge vocabulary (bits.tsx) so timeline nodes and status pills
// read as one system. `active` ring highlights the step currently waiting (pending approver).
const NODE_TONE: Record<string, { fill: string; text: string }> = {
  ok: { fill: "bg-okd", text: "text-ok" },
  cy: { fill: "bg-cyd", text: "text-cy" },
  wn: { fill: "bg-wnd", text: "text-wnf" },
  dg: { fill: "bg-dgd", text: "text-dgf" },
  neu: { fill: "bg-neud", text: "text-mut" },
  ac: { fill: "bg-acd", text: "text-ac-hi" },
};

export function Timeline({ children, className }: { children: React.ReactNode; className?: string }) {
  return <ol className={clsx("flex flex-col", className)}>{children}</ol>;
}

export function TimelineItem({
  icon: Icon,
  tone = "neu",
  active = false,
  title,
  time,
  children,
  className,
}: {
  icon: LucideIcon;
  tone?: string;
  /** Highlight the in-flight step (pending approver) with an accent ring. */
  active?: boolean;
  title: React.ReactNode;
  time?: React.ReactNode;
  children?: React.ReactNode;
  className?: string;
}) {
  const t = NODE_TONE[tone] ?? NODE_TONE.neu;
  return (
    <li className={clsx("group relative flex gap-3 pb-5 last:pb-0", className)}>
      {/* Rail — hidden on the last item */}
      <span
        aria-hidden
        className="absolute bottom-0 left-[11px] top-7 w-px bg-bd2 group-last:hidden"
      />
      {/* Node marker */}
      <span
        className={clsx(
          "relative z-[1] mt-px grid size-6 shrink-0 place-items-center rounded-full",
          t.fill,
          t.text,
          active && "ring-2 ring-ac ring-offset-2 ring-offset-panel",
        )}
      >
        <Icon className="size-3" strokeWidth={2.2} />
      </span>
      {/* Content */}
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
          <span className="text-body font-medium text-fg">{title}</span>
          {time && <span className="ml-auto shrink-0 font-mono text-caption text-fnt">{time}</span>}
        </div>
        {children}
      </div>
    </li>
  );
}
