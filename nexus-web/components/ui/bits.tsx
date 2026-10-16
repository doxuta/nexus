import { clsx } from "clsx";
import type { LucideIcon } from "lucide-react";

// Tone → token classes. Full class strings (KHÔNG nội suy động) để Tailwind JIT bắt được.
// Badge spec (handoff): nền *d + chữ đậm + chấm 6px màu gốc, pill, 3px 10px, 12px.
// W0-QW11: tone ac dùng text-ac-hi (KHÔNG text-ac) — chữ accent trên nền tint acd chỉ đạt
// 3.02:1 ở dark mode với --ac; --ac-hi là token chữ-trên-tint đạt ≥4.5:1 cả hai mode.
const STATUS_TONE: Record<string, { fill: string; text: string; dot: string }> = {
  ok: { fill: "bg-okd", text: "text-ok", dot: "bg-ok" },
  cy: { fill: "bg-cyd", text: "text-cy", dot: "bg-cy" },
  wn: { fill: "bg-wnd", text: "text-wnf", dot: "bg-wn" },
  dg: { fill: "bg-dgd", text: "text-dgf", dot: "bg-dg" },
  neu: { fill: "bg-neud", text: "text-mut", dot: "bg-neu" },
  ac: { fill: "bg-acd", text: "text-ac-hi", dot: "bg-ac" },
};

export function StatusBadge({ label, tone }: { label: string; tone: string }) {
  const t = STATUS_TONE[tone] ?? STATUS_TONE.neu;
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1.5 rounded-pill px-2.5 py-[3px] text-sub font-medium",
        t.fill,
        t.text,
      )}
    >
      <span className={clsx("size-1.5 rounded-full", t.dot)} />
      {label}
    </span>
  );
}

// Avatar tròn chữ cái đầu (người phụ trách / user).
export function Avatar({ initials, size = 22 }: { initials: string; size?: number }) {
  return (
    <span
      className="inline-grid place-items-center rounded-full bg-panel2 text-mut font-semibold"
      style={{ width: size, height: size, fontSize: size <= 22 ? 9.5 : 11, border: "1px solid var(--bd)" }}
    >
      {initials}
    </span>
  );
}

// ── EmptyState (W0-QW10, nâng cấp Phase 5b) — glyph line-art trong vòng tròn --panel2
// mềm + MỘT câu + MỘT hành động chính (Notion/Twenty style, nhịp dọc 96px = py-24).
// Primitive dùng chung cho mọi màn rỗng (/dashboards, /reports, /approvals, list entity,
// ERD canvas). CTA là ReactNode để caller tự phân vai (admin → link Studio; user → text).
// API giữ nguyên — mọi call site cũ chạy tiếp không sửa.
export function EmptyState({
  icon: Icon,
  title,
  desc,
  cta,
  className,
}: {
  icon: LucideIcon;
  title: string;
  desc?: React.ReactNode;
  cta?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={clsx("flex flex-col items-center justify-center gap-3 px-6 py-24 text-center", className)}>
      <div className="grid size-12 place-items-center rounded-full bg-panel2">
        <Icon className="size-5 text-mut" strokeWidth={1.6} />
      </div>
      <div>
        <p className="font-display text-title font-semibold text-fg">{title}</p>
        {desc && <p className="mx-auto mt-1 max-w-sm text-12-5px text-fnt">{desc}</p>}
      </div>
      {cta && <div className="mt-1">{cta}</div>}
    </div>
  );
}

// ── Kbd (W0-QW12) — phím tắt hiển thị trong cheatsheet/tooltip ────────────────
export function Kbd({ children }: { children: React.ReactNode }) {
  return (
    <kbd className="inline-flex min-w-[20px] items-center justify-center rounded-input border border-bd bg-panel2 px-1.5 py-0.5 font-mono text-caption text-mut">
      {children}
    </kbd>
  );
}
