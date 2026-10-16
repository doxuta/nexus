"use client";

// HelpHint — icon "?" tự-giải-thích tại chỗ (E2-IA-DESIGN-SYSTEM.md §11.3).
// W1a: migrate lên primitive Tooltip của Nexus Kit (components/ui/Tooltip.tsx — Radix).
// GIỮ NGUYÊN: public API (helpKey/text/label/side/className), nội dung 42 entries ở
// lib/help-content.ts, style trigger (CircleHelp 14px màu --mut, hit-target 20px),
// hành vi hover HOẶC focus mở / Esc + blur + mouseleave đóng, fail-quiet khi thiếu content.
// ĐƯỢC THÊM FREE từ Radix: portal + collision-flip (thay code đo getBoundingClientRect tay),
// aria-describedby wiring, tooltip BÁM trigger khi cuộn (trước đây phải tự đóng khi scroll —
// workaround 300ms focus-scroll cũng không cần nữa).
import { clsx } from "clsx";
import { CircleHelp } from "lucide-react";
import { getHelp } from "@/lib/help-content";
import { Tooltip } from "@/components/ui/Tooltip";

interface HelpHintProps {
  /** Tra nội dung trong lib/help-content.ts (ưu tiên nếu cả 2 được truyền). */
  helpKey?: string;
  /** Nội dung trực tiếp — dùng khi không đáng thêm 1 key vào help-content.ts. */
  text?: string;
  /** Nhãn a11y tuỳ chỉnh — mặc định lấy `term` của helpKey, hoặc "Trợ giúp". */
  label?: string;
  className?: string;
  /** Ưu tiên đặt tooltip phía trên (mặc định phía dưới, Radix tự lật khi thiếu chỗ). */
  side?: "top" | "bottom";
}

export function HelpHint({ helpKey, text, label, className, side = "bottom" }: HelpHintProps) {
  const entry = helpKey ? getHelp(helpKey) : undefined;
  const content = text ?? entry?.short;

  if (!content) return null; // helpKey lạ hoặc thiếu text — fail-quiet, không vẽ icon vô nghĩa

  const ariaLabel = `Giải thích: ${label ?? entry?.term ?? "trợ giúp"}`;

  return (
    <span className={clsx("relative inline-flex", className)}>
      <Tooltip content={content} side={side} delayDuration={0}>
        <button
          type="button"
          aria-label={ariaLabel}
          className="grid size-5 shrink-0 place-items-center rounded-full text-mut transition-colors hover:text-ac focus-visible:text-ac"
        >
          {/* Icon 14px đúng spec §11.3; nút bấm rộng hơn (20px) chỉ để hit-target dễ bấm hơn trên
              chuột/cảm ứng — không đổi kích thước hiển thị. */}
          <CircleHelp className="size-3.5" strokeWidth={1.75} />
        </button>
      </Tooltip>
    </span>
  );
}
