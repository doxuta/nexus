// lib/workshopPanels.ts — MỘT XƯỞNG panel registry (NEXUS-MOT-XUONG-2026-07-29.md §2b).
// Panel tokens still read by the AI assistant context (aiPanel in lib/studioRoutes.ts), the entity-hub tabs and the
// builders' crumbs. The old workspace (AppWorkspace) and its PANEL_BRIDGE new-tab map are gone (specs/073 kh7).

export type PanelKey =
  | "tong-quan" | "bang" | "menu" | "thuong-hieu" | "tu-dong" | "duyet" | "hang-doi" | "webhook"
  | "bao-cao" | "dashboard" | "vai-tro" | "nguoi-dung" | "quyen-truong" | "lich-lam-viec" | "mau" | "ban-giao"
  | "man-hinh" | "chay-thu" | "van-hanh" | "bao-tri";

export const PANEL_LABELS: Record<PanelKey, string> = {
  "tong-quan": "Tổng quan",
  bang: "Dữ liệu",
  menu: "Menu app",
  "thuong-hieu": "Thương hiệu",
  "tu-dong": "Tự động hoá",
  duyet: "Quy trình duyệt",
  "hang-doi": "Hàng đợi việc",
  webhook: "Webhook nhận",
  "bao-cao": "Báo cáo",
  dashboard: "Bảng điều khiển",
  "vai-tro": "Vai trò & quyền",
  "nguoi-dung": "Người dùng",
  "quyen-truong": "Phân quyền trường",
  "lich-lam-viec": "Lịch làm việc",
  mau: "Cài mẫu",
  "ban-giao": "Bàn giao khách",
  "man-hinh": "Màn hình",
  "chay-thu": "Chạy thử",
  "van-hanh": "Vận hành",
  "bao-tri": "Bảo trì",
};

export function isPanelKey(v: string): v is PanelKey {
  return Object.prototype.hasOwnProperty.call(PANEL_LABELS, v);
}

/** Normalize a raw `?panel=` value: absent/blank → the default "tong-quan". Never mutates input. */
export function normalizePanel(raw: string | null): string {
  const v = (raw ?? "").trim();
  return v === "" ? "tong-quan" : v;
}

/**
 * Label safe for ANY panel token, known or not — an unknown token echoes itself (truncated),
 * never silently reads as something else (the STUDIO_NAV[""] fallback bug this campaign fixes).
 */
export function panelLabel(panel: string): string {
  if (isPanelKey(panel)) return PANEL_LABELS[panel];
  return panel.length > 40 ? `${panel.slice(0, 40)}…` : panel;
}
