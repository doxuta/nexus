"use client";

// Reveal — fade-up nhẹ khi cuộn vào khung nhìn (E2-IA-DESIGN-SYSTEM.md §10.3: "Motion: fade-up
// nhẹ theo scroll, IntersectionObserver, tôn trọng prefers-reduced-motion, KHÔNG parallax/
// particle/gradient-mesh"). Nội dung con LUÔN nằm trong DOM (chỉ opacity/translate-y, không
// display:none/hidden) — an toàn cho crawler/no-JS, không "gate visibility bằng class" theo
// đúng luật design-anti-slop. Nếu prefers-reduced-motion bật, bỏ qua animation hoàn toàn (hiện
// ngay, không chờ observer) thay vì chỉ rút ngắn duration.
import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { clsx } from "clsx";

export function Reveal({
  children,
  className,
  delayMs = 0,
}: {
  children: React.ReactNode;
  className?: string;
  delayMs?: number;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const [seen, setSeen] = useState(false);
  // Không có animation (reduced-motion / không có IntersectionObserver) → hiện ngay, không qua effect.
  const skipAnim = useSyncExternalStore(
    () => () => {},
    () =>
      !!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ||
      typeof IntersectionObserver === "undefined",
    () => false,
  );
  const shown = seen || skipAnim;

  useEffect(() => {
    if (skipAnim) return;
    const el = ref.current;
    if (!el) return;
    const obs = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) {
            setSeen(true);
            obs.disconnect();
            break;
          }
        }
      },
      { threshold: 0.15, rootMargin: "0px 0px -10% 0px" },
    );
    obs.observe(el);
    return () => obs.disconnect();
  }, [skipAnim]);

  return (
    <div
      ref={ref}
      className={clsx(
        "transition-all duration-500 ease-out",
        shown ? "translate-y-0 opacity-100" : "translate-y-3 opacity-0",
        className,
      )}
      style={delayMs ? { transitionDelay: `${delayMs}ms` } : undefined}
    >
      {children}
    </div>
  );
}
