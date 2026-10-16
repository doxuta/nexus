// VN formatting (handoff §Ngôn ngữ): tiền luôn "VNĐ", nhóm hàng nghìn bằng dấu chấm; cột hẹp
// dùng dạng rút gọn "tỷ/triệu" với dấu phẩy thập phân. Ngày DD/MM/YYYY, giờ HH:mm.

const nf = new Intl.NumberFormat("vi-VN");

/** "3.500.000.000 VNĐ" — số đầy đủ, nhóm nghìn bằng dấu chấm. */
export function formatVND(amount: number): string {
  return `${nf.format(Math.round(amount))} VNĐ`;
}

/**
 * Format a money amount RESPECTING its currency code (D10/D15). Currency field values are stored as
 * `{ amount, currencyCode }`; the list + detail must show the SAME currency as the input form, not a
 * hardcoded VNĐ. VND → "… VNĐ"; other ISO codes → Intl currency (e.g. USD → "75.000 US$").
 */
export function formatMoney(amount: number, code?: string | null): string {
  const c = (code || "VND").toUpperCase();
  if (c === "VND") return formatVND(amount);
  try {
    return new Intl.NumberFormat("vi-VN", { style: "currency", currency: c, maximumFractionDigits: 0 }).format(amount);
  } catch {
    return `${nf.format(Math.round(amount))} ${c}`;
  }
}

/** Dạng rút gọn cho cột hẹp: "3,50 tỷ VNĐ", "760 triệu VNĐ" (dấu phẩy = thập phân VN). */
export function formatVNDCompact(amount: number): string {
  const abs = Math.abs(amount);
  if (abs >= 1_000_000_000) {
    const v = amount / 1_000_000_000;
    return `${formatDecimal(v)} tỷ VNĐ`;
  }
  if (abs >= 1_000_000) {
    const v = amount / 1_000_000;
    return `${Math.round(v)} triệu VNĐ`;
  }
  return formatVND(amount);
}

function formatDecimal(v: number): string {
  // 2 chữ số thập phân, dấu phẩy kiểu VN, bỏ ".00".
  const s = v.toFixed(2).replace(".", ",");
  return s.replace(/,00$/, "");
}

/** DD/MM/YYYY */
// All displayed times are pinned to Asia/Ho_Chi_Minh (D6) so they read the same regardless of the
// viewer's browser timezone — a VN product should never show UTC or a foreign-tz clock.
const VN_TZ = "Asia/Ho_Chi_Minh";
const dateFmt = new Intl.DateTimeFormat("vi-VN", { timeZone: VN_TZ, day: "2-digit", month: "2-digit", year: "numeric" });
const dateTimeFmt = new Intl.DateTimeFormat("vi-VN", { timeZone: VN_TZ, day: "2-digit", month: "2-digit", year: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });

export function formatDate(d: Date | string): string {
  const date = typeof d === "string" ? new Date(d) : d;
  if (isNaN(date.getTime())) return "—";
  return dateFmt.format(date);
}

/**
 * Parse a server datetime deterministically. The server returns "2026-07-14 09:00:00" (space, no
 * zone) and stores UTC (D1), so a zoneless value is read AS UTC — `new Date()` would read it in the
 * browser's zone and shift the clock by the viewer's offset. Zoned values (Z / +07:00) pass through.
 */
export function parseServerDateTime(v: string): Date {
  const s = v.trim();
  return new Date(/^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(:\d{2}(\.\d+)?)?$/.test(s) ? `${s.replace(" ", "T")}Z` : s);
}

const timeFmt = new Intl.DateTimeFormat("vi-VN", { timeZone: VN_TZ, hour: "2-digit", minute: "2-digit", hour12: false });

/** HH:mm in Asia/Ho_Chi_Minh ("—" when the value is not a date). */
export function formatTime(d: Date | string): string {
  const date = typeof d === "string" ? parseServerDateTime(d) : d;
  if (isNaN(date.getTime())) return "—";
  return timeFmt.format(date);
}

/** DD/MM/YYYY HH:mm in Asia/Ho_Chi_Minh. */
export function formatDateTime(d: Date | string): string {
  const date = typeof d === "string" ? parseServerDateTime(d) : d;
  if (isNaN(date.getTime())) return "—";
  return dateTimeFmt.format(date);
}

/** "512 B" · "1,5 KB" · "3,2 MB" · "1,25 GB" — byte size for files and storage (VN decimal comma); null -> "—". */
export function formatBytes(bytes: number | null): string {
  if (bytes == null) return "—";
  const one = (v: number) => v.toLocaleString("vi-VN", { minimumFractionDigits: 1, maximumFractionDigits: 1 });
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 ** 2) return `${one(bytes / 1024)} KB`;
  if (bytes < 1024 ** 3) return `${one(bytes / 1024 ** 2)} MB`;
  return `${(bytes / 1024 ** 3).toLocaleString("vi-VN", { minimumFractionDigits: 2, maximumFractionDigits: 2 })} GB`;
}
