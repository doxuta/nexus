// NEXUS STUDIO — metadata tĩnh cho thư viện vertical (catalog minh hoạ).
// W0-QW5: PROJECTS (danh sách "dự án khách" bịa) + STUDIO_USER (user cứng) đã XOÁ —
// mock chết, 0 import, từng là nguồn "fake data" Founder chỉ mặt. Khi có backend
// sys_projects (P5) thì lấy qua API, không hard-code lại.
// VERTICALS giữ: đây là CATALOG hiển thị (label/badge/industries/entities) —
// VerticalLibrary đối chiếu với GET /admin/verticals/builtin; pack chưa có builtin
// backend nằm ở mục "Lộ trình" (honest-muted, KHÔNG có nút cài), không giả cài đặt.
// Với pack builtin (crm/clinic): entities + BUILTIN_PACK_FACTS PHẢN CHIẾU nội dung
// THẬT của nexus-platform/internal/services/vertical_pack/builtin.go — đổi pack ở
// backend thì cập nhật ở đây (nút "Xem trước nội dung" chạy dry-run thật để đối chứng).

export interface Vertical {
  name: string;
  label: string;
  badge: string;
  industries: string;
  entities: string[];
  tone: "ac" | "cy" | "ok" | "wn" | "dg" | "neu";
}

// Thư viện vertical dựng sẵn (seed-pack) — ráp dự án mới cực nhanh.
export const VERTICALS: Vertical[] = [
  // crm/clinic: entity chips = đúng các entity pack builtin thật sự tạo (builtin.go).
  { name: "crm", label: "CRM / Bán hàng", badge: "CRM", industries: "Bán lẻ, dịch vụ, B2B", entities: ["Khách hàng", "Liên hệ", "Cơ hội", "Leads", "Hoạt động"], tone: "ac" },
  { name: "clinic", label: "Phòng khám", badge: "PK", industries: "Y tế", entities: ["Bệnh nhân", "Lịch hẹn"], tone: "wn" },
  // Các vertical dưới đây CHƯA có pack backend — chỉ hiện ở mục "Lộ trình" (muted).
  { name: "lms", label: "LMS / Đào tạo", badge: "LMS", industries: "Giáo dục, trung tâm", entities: ["Học viên", "Khoá học", "Lớp", "Học phí"], tone: "cy" },
  { name: "ticketing", label: "Ticketing / Hỗ trợ", badge: "TK", industries: "Dịch vụ, SaaS", entities: ["Ticket", "Khách", "SLA", "Kênh"], tone: "ok" },
  { name: "inventory", label: "Kho / Phân phối", badge: "KHO", industries: "Sản xuất, đại lý", entities: ["Sản phẩm", "Đơn", "Tồn kho", "Đại lý"], tone: "dg" },
  { name: "hrm", label: "Nhân sự (HRM)", badge: "HRM", industries: "Mọi DN", entities: ["Nhân viên", "Phòng ban", "Chấm công", "Nghỉ phép"], tone: "neu" },
];

/**
 * orderBuiltinsForDisplay — thứ tự hiển thị pack builtin trên trang thư viện:
 * theo thứ tự catalog (CRM = vertical tham chiếu → đứng đầu), backend trả theo alphabet.
 * Slug lạ (không có trong catalog) giữ nguyên thứ tự backend, xếp cuối.
 * Generic: nhận slug trần ("crm") HOẶC object mang slug ({slug, manifest, entityCount} từ
 * GET /admin/verticals/builtin) — cùng một luật sắp xếp, không nhân đôi helper.
 */
export function orderBuiltinsForDisplay<T extends string | { slug: string }>(items: T[]): T[] {
  const idx = new Map(VERTICALS.map((v, i) => [v.name, i]));
  const slugOf = (x: T): string => (typeof x === "string" ? x : x.slug);
  return [...items].sort(
    (a, b) => (idx.get(slugOf(a)) ?? Number.MAX_SAFE_INTEGER) - (idx.get(slugOf(b)) ?? Number.MAX_SAFE_INTEGER),
  );
}

// ── Facts về pack builtin (mirror builtin.go — KHÔNG phải aspiration) ────────────
// Đây là mô tả nội dung THẬT của pack backend đang ship; card dùng để nói pack sâu
// tới đâu TRƯỚC khi cài. Đối chứng sống: "Xem trước nội dung" chạy dry-run thật.
// Lưu ý version: GET /admin/verticals/builtin nay trả manifest THẬT của từng pack
// (FE-CATCHUP-2) → name/version/description/entityCount hiển thị từ API sống, KHÔNG
// bao giờ hard-code ở đây. Facts bên dưới chỉ là checklist nội dung (mô tả), không
// mang version; version cài-rồi vẫn đọc từ nhật ký cài như cũ.
export interface BuiltinPackFacts {
  /** Một câu định vị pack. */
  tagline: string;
  /** Điểm nội dung thật — mỗi mục một năng lực engine mà pack chứng minh. */
  features: string[];
}

export const BUILTIN_PACK_FACTS: Record<string, BuiltinPackFacts> = {
  crm: {
    tagline: "CRM đầy đủ — pipeline từ lead tới doanh thu, chứng minh chiều sâu engine.",
    features: [
      "5 đối tượng: khách hàng · liên hệ · cơ hội · leads · hoạt động",
      "Quan hệ N-N liên hệ ↔ cơ hội (stakeholders)",
      "Rollup doanh thu theo khách: sum / count / avg",
      "Mã tự sinh (auto-number) cho cơ hội & lead",
      "Lookup tên khách + liên hệ chính 1-1",
      "21 bản ghi mẫu · dashboard CRM Overview · 2 vai trò ACL",
    ],
  },
  clinic: {
    tagline: "Phòng khám cơ bản — bệnh nhân và lịch hẹn, cài sạch trong một bước.",
    features: [
      "2 đối tượng: bệnh nhân · lịch hẹn",
      "Quan hệ 1-N lịch hẹn → bệnh nhân",
      "Mã bệnh nhân duy nhất (unique)",
      "Bố cục list/detail + 2 vai trò (lễ tân, bác sĩ)",
    ],
  },
};
