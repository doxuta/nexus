"use client";

// ConfirmDialog — Nexus Kit (W1a, plan §4.2). Lưới an toàn CHUẨN cho mọi thao tác phá huỷ,
// 3 variant tăng dần theo rủi ro (dùng cho delete field/entity/relationship, deploy validate…):
//   • simple          — hỏi 1 câu, 2 nút. Cho thao tác đảo ngược được / impact = 0.
//   • impact-manifest — kèm bảng "đụng tới cái gì" (đếm record con, rollup phụ thuộc…)
//     để người bấm THẤY hậu quả trước khi bấm (honest state model — không xoá mù).
//   • type-name       — gõ đúng tên đối tượng mới mở khoá nút (pattern GitHub delete-repo)
//     cho thao tác không đảo ngược được với impact > 0.
// Build trên Dialog.tsx (Radix) — token-only, motion --dur-2/--ease-out, reduced-motion OK.

import { useEffect, useState } from "react";
import { Loader2, TriangleAlert } from "lucide-react";
import { clsx } from "clsx";
import {
  Dialog,
  DialogBody,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "./Dialog";

/** Một dòng trong impact manifest — "3 bản ghi con giữ FK", "2 rollup phụ thuộc"… */
export interface ImpactRow {
  label: string;
  count?: number;
  /** neutral (mặc định) · warn · danger — tô đậm mức rủi ro của dòng. */
  tone?: "neutral" | "warn" | "danger";
}

export interface ConfirmDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: React.ReactNode;
  /** simple (mặc định) · impact (manifest) · type-name (gõ tên xác nhận). */
  variant?: "simple" | "impact" | "type-name";
  /** Bảng impact — bắt buộc có nội dung khi variant="impact"; hiện được kèm type-name. */
  impact?: ImpactRow[];
  /** variant type-name: chuỗi phải gõ ĐÚNG (case-sensitive) để mở khoá nút xác nhận. */
  confirmName?: string;
  /** Nút xác nhận tô đỏ (bg-dg) — mặc định true vì đa số confirm là destructive. */
  danger?: boolean;
  confirmLabel?: string;
  cancelLabel?: string;
  /** Đang chạy — disable cả 2 nút + spinner trên nút xác nhận. */
  loading?: boolean;
  onConfirm: () => void;
}

const IMPACT_TONE: Record<NonNullable<ImpactRow["tone"]>, string> = {
  neutral: "text-fg",
  warn: "text-wnf",
  danger: "text-dgf",
};

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  variant = "simple",
  impact,
  confirmName,
  danger = true,
  confirmLabel,
  cancelLabel = "Huỷ",
  loading = false,
  onConfirm,
}: ConfirmDialogProps) {
  const [typed, setTyped] = useState("");

  // Reset ô gõ tên mỗi lần mở lại — không giữ trạng thái "đã mở khoá" từ lần trước.
  const [prevOpen, setPrevOpen] = useState(open);
  if (open !== prevOpen) {
    setPrevOpen(open);
    if (open) setTyped("");
  }

  const needsTypeName = variant === "type-name" && !!confirmName;
  const locked = needsTypeName && typed !== confirmName;
  const finalConfirmLabel = confirmLabel ?? (danger ? "Xoá" : "Xác nhận");

  return (
    <Dialog open={open} onOpenChange={loading ? () => {} : onOpenChange}>
      <DialogContent hideClose>
        <DialogHeader>
          <div className="flex items-start gap-3">
            {danger && (
              <span className="mt-0.5 grid size-8 shrink-0 place-items-center rounded-input bg-dgd">
                <TriangleAlert className="size-4 text-dgf" strokeWidth={2} />
              </span>
            )}
            <div className="min-w-0">
              <DialogTitle>{title}</DialogTitle>
              {description && <DialogDescription className="mt-1">{description}</DialogDescription>}
            </div>
          </div>
        </DialogHeader>

        {(variant === "impact" || (needsTypeName && impact && impact.length > 0)) && (
          <DialogBody>
            <ul className="divide-y divide-bd2 rounded-input border border-bd bg-panel2/60">
              {(impact ?? []).map((row, i) => (
                <li key={i} className="flex items-center justify-between gap-3 px-3 py-2 text-12-5px">
                  <span className={IMPACT_TONE[row.tone ?? "neutral"]}>{row.label}</span>
                  {typeof row.count === "number" && (
                    <span className={clsx("font-mono text-sub font-semibold tabular", IMPACT_TONE[row.tone ?? "neutral"])}>
                      {row.count.toLocaleString("vi-VN")}
                    </span>
                  )}
                </li>
              ))}
              {variant === "impact" && (!impact || impact.length === 0) && (
                <li className="px-3 py-2 text-12-5px text-fnt">Không có đối tượng nào bị ảnh hưởng.</li>
              )}
            </ul>
          </DialogBody>
        )}

        {needsTypeName && (
          <DialogBody>
            <label className="block text-sub text-fnt">
              Gõ <span className="select-all rounded-badge bg-panel2 px-1.5 py-0.5 font-mono text-11-5px font-medium text-fg">{confirmName}</span> để xác nhận:
            </label>
            <input
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              disabled={loading}
              autoComplete="off"
              spellCheck={false}
              aria-label={`Gõ ${confirmName} để xác nhận`}
              className="mt-2 h-9 w-full rounded-input border border-bd bg-panel px-3 font-mono text-body text-fg placeholder:text-fnt disabled:opacity-60"
              placeholder={confirmName}
            />
          </DialogBody>
        )}

        <DialogFooter>
          <button
            type="button"
            onClick={() => onOpenChange(false)}
            disabled={loading}
            className="h-8 rounded-input border border-bd bg-panel px-3.5 text-12-5px font-medium text-fg transition-colors hover:bg-panel2 disabled:opacity-60"
          >
            {cancelLabel}
          </button>
          <button
            type="button"
            onClick={onConfirm}
            disabled={loading || locked}
            className={clsx(
              "flex h-8 items-center gap-1.5 rounded-input px-3.5 text-12-5px font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-50",
              danger ? "bg-dg text-white hover:bg-dgf" : "bg-ac text-acf hover:bg-ac2",
            )}
          >
            {loading && <Loader2 className="size-3.5 animate-spin" strokeWidth={2} />}
            {finalConfirmLabel}
          </button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
