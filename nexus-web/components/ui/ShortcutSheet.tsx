"use client";

// ShortcutSheet — W0-QW12: overlay "?" liệt kê phím tắt toàn app.
// Mount MỘT LẦN trong Topbar (mọi face đều có Topbar). Hai đường mở:
//   1. Phím "?" toàn cục (bỏ qua khi đang gõ trong input/textarea/contenteditable).
//   2. openShortcutSheet() — Sidebar "Phím tắt & Trợ giúp" gọi qua CustomEvent, không cần
//      prop-drill qua shell.
// Motion: fade 120ms qua class nx-tooltip-in sẵn có (reduced-motion đã kill toàn cục ở globals).
// Mở đường cho keyboard layer của canvas W2 — thêm shortcut mới chỉ cần thêm dòng vào SHORTCUTS.

import { useEffect, useState } from "react";
import { Keyboard, X } from "lucide-react";
import { Kbd } from "@/components/ui/bits";

const OPEN_EVENT = "nexus:shortcut-sheet";

/** Mở cheatsheet từ bất kỳ component nào (Sidebar footer, help menu…). */
export function openShortcutSheet() {
  window.dispatchEvent(new CustomEvent(OPEN_EVENT));
}

type Shortcut = { keys: string[]; label: string };
type ShortcutGroup = { title: string; items: Shortcut[] };

const SHORTCUTS: ShortcutGroup[] = [
  {
    title: "Toàn cục",
    items: [
      { keys: ["⌘", "K"], label: "Mở bảng lệnh (tìm & điều hướng)" },
      { keys: ["?"], label: "Mở bảng phím tắt này" },
      { keys: ["Esc"], label: "Đóng overlay / hộp thoại" },
    ],
  },
  {
    title: "Bảng lệnh ⌘K",
    items: [
      { keys: ["↑", "↓"], label: "Di chuyển lựa chọn" },
      { keys: ["Enter"], label: "Mở mục đang chọn" },
      { keys: ["Esc"], label: "Đóng bảng lệnh" },
    ],
  },
];

function isTypingTarget(t: EventTarget | null): boolean {
  if (!(t instanceof HTMLElement)) return false;
  const tag = t.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || t.isContentEditable;
}

export function ShortcutSheet() {
  const [open, setOpen] = useState(false);

  useEffect(() => {
    const onOpen = () => setOpen(true);
    const onKey = (e: KeyboardEvent) => {
      // "?" = Shift+/ trên bàn phím US; e.key đã là "?" nên không cần check shiftKey.
      if (e.key === "?" && !isTypingTarget(e.target) && !e.metaKey && !e.ctrlKey && !e.altKey) {
        e.preventDefault();
        setOpen((o) => !o);
      } else if (e.key === "Escape") {
        setOpen(false);
      }
    };
    window.addEventListener(OPEN_EVENT, onOpen);
    document.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener(OPEN_EVENT, onOpen);
      document.removeEventListener("keydown", onKey);
    };
  }, []);

  if (!open) return null;

  return (
    <div
      className="fixed inset-0 z-[60] flex items-start justify-center pt-[14vh]"
      onClick={() => setOpen(false)}
    >
      <div className="absolute inset-0 bg-fg/10" style={{ backdropFilter: "blur(3px)" }} aria-hidden />
      <div
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-modal="true"
        aria-label="Phím tắt"
        className="nx-tooltip-in relative w-full max-w-[420px] overflow-hidden rounded-card border border-bd bg-panel shadow-dialog"
      >
        <div className="flex items-center gap-2.5 border-b border-bd px-4 py-3">
          <Keyboard className="size-4 shrink-0 text-mut" strokeWidth={1.8} />
          <h2 className="font-display text-14px font-semibold text-fg">Phím tắt</h2>
          <button
            onClick={() => setOpen(false)}
            aria-label="Đóng"
            className="ml-auto grid size-7 place-items-center rounded-input text-mut transition-colors hover:bg-panel2 hover:text-fg"
          >
            <X className="size-3.5" strokeWidth={2} />
          </button>
        </div>

        <div className="max-h-[56vh] overflow-y-auto px-4 py-3">
          {SHORTCUTS.map((group) => (
            <div key={group.title} className="mb-3 last:mb-0">
              <p className="pb-1.5 text-10-5px font-semibold tracking-wider text-fnt">
                {group.title.toUpperCase()}
              </p>
              <ul className="divide-y divide-bd2">
                {group.items.map((s) => (
                  <li key={s.label} className="flex items-center gap-3 py-2 text-body">
                    <span className="flex-1 text-mut">{s.label}</span>
                    <span className="flex shrink-0 items-center gap-1">
                      {s.keys.map((k) => (
                        <Kbd key={k}>{k}</Kbd>
                      ))}
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>

        <div className="border-t border-bd px-4 py-2 text-10-5px text-fnt">
          Nhấn <Kbd>?</Kbd> bất kỳ lúc nào để mở lại bảng này.
        </div>
      </div>
    </div>
  );
}
