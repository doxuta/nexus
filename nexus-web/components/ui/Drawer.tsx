"use client";

import { useEffect } from "react";
import { clsx } from "clsx";
import { X, Maximize2, ChevronRight } from "lucide-react";

// Record Drawer (P2 — Sheet trượt từ phải 632px). Xem/sửa KHÔNG rời danh sách.
export function Drawer({
  open,
  onClose,
  crumbs,
  fullHref,
  children,
}: {
  open: boolean;
  onClose: () => void;
  crumbs: string[];
  fullHref?: string;
  children: React.ReactNode;
}) {
  // esc đóng + khoá scroll nền khi mở.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onClose();
    document.addEventListener("keydown", onKey);
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = "";
    };
  }, [open, onClose]);

  return (
    <div className={clsx("fixed inset-0 z-50", open ? "" : "pointer-events-none")} aria-hidden={!open}>
      {/* overlay */}
      <div
        onClick={onClose}
        className={clsx("absolute inset-0 bg-fg/20 transition-opacity duration-200", open ? "opacity-100" : "opacity-0")}
      />
      {/* panel */}
      <div
        className={clsx(
          "absolute right-0 top-0 flex h-full w-[632px] max-w-[92vw] flex-col bg-bg shadow-drawer transition-transform duration-200 ease-out",
          open ? "translate-x-0" : "translate-x-full",
        )}
      >
        {/* drawer header */}
        <div className="flex h-12 shrink-0 items-center gap-2 border-b border-bd bg-panel px-4">
          <nav className="flex items-center gap-1 text-12-5px text-fnt">
            {crumbs.map((c, i) => (
              <span key={i} className="flex items-center gap-1">
                {i > 0 && <ChevronRight className="size-3 text-bd3" strokeWidth={2} />}
                <span className={i === crumbs.length - 1 ? "max-w-[220px] truncate font-medium text-fg" : ""}>{c}</span>
              </span>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-1">
            {fullHref && (
              <a href={fullHref} className="flex items-center gap-1.5 rounded-input px-2 py-1 text-sub font-medium text-mut hover:bg-panel2">
                <Maximize2 className="size-3.5" strokeWidth={1.8} /> Mở trang đầy đủ
              </a>
            )}
            <button onClick={onClose} className="grid size-7 place-items-center rounded-input text-fnt hover:bg-panel2 hover:text-fg">
              <X className="size-4" strokeWidth={2} />
            </button>
          </div>
        </div>
        {/* drawer body */}
        <div className="flex min-h-0 flex-1 flex-col overflow-hidden">{open && children}</div>
      </div>
    </div>
  );
}
