// NexusLogo — thương hiệu "Nexus Be" (E2-IA-DESIGN-SYSTEM.md §10.2).
// Glyph "N-node": chữ N dựng từ 2 cột dọc + 1 đường chéo nối 4 node tròn ở 4 đầu — ẩn dụ
// metadata-graph (entity = node, relationship = edge/cạnh nối). SVG tay, lưới 24x24,
// stroke 2px, 1 màu currentColor — tự ăn --fg/--acf theo ngữ cảnh nơi đặt (KHÔNG ảnh
// raster/AI-gen).
import { clsx } from "clsx";

export interface NexusLogoProps {
  /** Kích thước badge (px) — glyph bên trong tự co theo tỉ lệ. */
  size?: number;
  /** "full" = badge + wordmark "Nexus" (mặc định). "glyph" = chỉ badge, không chữ. */
  variant?: "full" | "glyph";
  className?: string;
}

const GRID = 24; // lưới toạ độ glyph
const NODE_R = 2.1; // bán kính node tròn (đơn vị lưới)
const NODE_D = NODE_R * 2; // đường kính node — spec §10.2: khoảng cách wordmark = 1 node

/**
 * NexusMark — glyph trần (không badge), toạ độ tự tính trên lưới 24x24:
 * cột trái x=6 (y 4→20), cột phải x=18 (y 4→20), chéo nối (6,4)→(18,20) — 3 cạnh + 4 node
 * tròn ở (6,4) (6,20) (18,4) (18,20). Dùng nguyên trạng ở app/icon.svg (file tĩnh, không
 * import được component React nên toạ độ được lặp lại thủ công ở đó — xem NAV-MAP/báo cáo).
 */
export function NexusMark({ size, className }: { size: number; className?: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox={`0 0 ${GRID} ${GRID}`}
      fill="none"
      className={className}
      aria-hidden="true"
    >
      <path
        d="M6 4 V20 M18 4 V20 M6 4 L18 20"
        stroke="currentColor"
        strokeWidth={2}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <circle cx="6" cy="4" r={NODE_R} fill="currentColor" stroke="none" />
      <circle cx="6" cy="20" r={NODE_R} fill="currentColor" stroke="none" />
      <circle cx="18" cy="4" r={NODE_R} fill="currentColor" stroke="none" />
      <circle cx="18" cy="20" r={NODE_R} fill="currentColor" stroke="none" />
    </svg>
  );
}

/**
 * NexusLogo — badge vuông bo góc (nền --ac, glyph trắng --acf — contrast acf/ac đo được
 * ~6.5:1 light / ~4.6:1 dark, xem globals.css) + wordmark tuỳ chọn. Đặt ở Sidebar (App),
 * StudioSidebar, /login.
 * Wordmark weight: spec đòi 650, Be Vietnam Pro (next/font) chỉ nạp static 400/500/600/700
 * (không có trục variable) → xấp xỉ bằng 600 (font-semibold, nấc nạp gần nhất) — ghi rõ vì
 * luật anti-slop yêu cầu trung thực approximation.
 */
export function NexusLogo({ size = 28, variant = "full", className }: NexusLogoProps) {
  const badgeRadius = Math.round(size * 0.32);
  const markSize = Math.round(size * 0.62);
  const gap = Math.round(markSize * (NODE_D / GRID)); // = bề rộng 1 node ở tỉ lệ mark thật

  const badge = (
    <span
      className="grid shrink-0 place-items-center bg-ac text-acf"
      style={{ width: size, height: size, borderRadius: badgeRadius }}
    >
      <NexusMark size={markSize} />
    </span>
  );

  if (variant === "glyph") {
    return <span className={clsx("inline-flex", className)}>{badge}</span>;
  }

  return (
    <span className={clsx("inline-flex items-center", className)} style={{ gap }}>
      {badge}
      <span
        className="font-display text-fg"
        style={{ fontSize: Math.round(size * 0.6), fontWeight: 600, letterSpacing: "-0.02em" }}
      >
        Nexus
      </span>
    </span>
  );
}
