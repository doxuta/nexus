import { Star, Check, X } from "lucide-react";
import { valueLabel, type FieldSpec } from "@/lib/metadata";
import { formatMoney, formatDate, formatDateTime, parseServerDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import { Badge } from "@/components/slds/Badge";
import { PolymorphicDisplay } from "@/components/fields/widgets";

/**
 * Định dạng giá trị của field TÍNH-SẴN (formula / rollup) — MỘT thân dùng chung.
 *
 * Ba hình dạng giá trị thật server trả về:
 *   • `{amount, currencyCode}` — khi field gốc là tiền tệ (rollup SUM trên currency).
 *   • `number` — COUNT/AVG…
 *   • chuỗi DECIMAL của MySQL, vd `"235000.0000"` — parse được thì format như số.
 * Không parse được thì trả nguyên văn, KHÔNG bịa (một chuỗi lạ hiện nguyên hình vẫn tốt hơn là
 * biến thành "NaN" hay số 0).
 */
function formatComputed(value: unknown): string {
  if (value && typeof value === "object" && "amount" in (value as object)) {
    const cv = value as { amount: number | string; currencyCode?: string };
    return formatMoney(Number(cv.amount), cv.currencyCode);
  }
  if (typeof value === "number") return new Intl.NumberFormat("vi-VN").format(value);
  const n = Number(value);
  return Number.isFinite(n) && String(value ?? "").trim() !== ""
    ? new Intl.NumberFormat("vi-VN").format(n)
    : String(value ?? "");
}

// READ-mode renderers (handoff màn 05) — how EACH field type reads when VIEWED (not the input
// widget). Used by record detail, list/kanban cells and the form's locked/read-only cells.
// Engine symmetric with FieldInput (write-mode). SLDS 2.264.1: plain text + SLDS text/icon colour
// classes, picklist values as `badges`, a relation as a record link (form-element/record-detail).
const EMPTY = <span className="slds-text-color_weak">—</span>;

// Glyph + text on one line (Tailwind preflight makes svg block-level; a layout utility keeps it inline).
const ICON_TEXT = "inline-flex items-center gap-1";

export function FieldDisplay({ field, value }: { field: FieldSpec; value: unknown }) {
  const o = field.options ?? {};
  if (isEmpty(value)) return EMPTY;

  switch (field.fieldType) {
    case "email":
      return <a href={`mailto:${value}`}>{String(value)}</a>;
    case "boolean":
      return value ? (
        <span className={ICON_TEXT}>
          <Check aria-hidden="true" className="slds-icon slds-icon_x-small slds-icon-text-success nx-lucide" />
          {t("page.recordForm.yes")}
        </span>
      ) : (
        <span className={`${ICON_TEXT} slds-text-color_weak`}>
          <X aria-hidden="true" className="slds-icon slds-icon_x-small slds-icon-text-light nx-lucide" />
          {t("page.recordForm.no")}
        </span>
      );
    case "picklist":
      // valueLabel: the stored CODE stays what it is; only the text a human reads is resolved
      // through options.valueLabels ("New" → "Mới"). No label → the code itself, never invented.
      return <Badge>{valueLabel(o, String(value))}</Badge>;
    case "multi_select":
      return (
        <span className="inline-flex flex-wrap gap-1">
          {arr(value).map((v) => <Badge key={v}>{valueLabel(o, v)}</Badge>)}
        </span>
      );
    case "rating": {
      const max = o.max ?? 5, n = num(value);
      return (
        <span className="inline-flex gap-0.5" title={`${n}/${max}`}>
          {Array.from({ length: max }, (_, i) => (
            <Star
              key={i}
              aria-hidden="true"
              className={`slds-icon slds-icon_x-small nx-lucide ${i < n ? "slds-icon-text-warning" : "slds-icon-text-light"}`}
            />
          ))}
          <span className="slds-assistive-text">{`${n}/${max}`}</span>
        </span>
      );
    }
    case "currency": {
      // Currency values are stored as { amount, currencyCode } — render the SAME currency as the
      // input form (D10/D15), not a hardcoded VNĐ, and read `.amount` from the object (not the object).
      const cv =
        value && typeof value === "object" && "amount" in (value as Record<string, unknown>)
          ? (value as { amount: number; currencyCode?: string })
          : { amount: num(value), currencyCode: (o as Record<string, unknown>).currency as string | undefined };
      return <span className="tabular-nums">{formatMoney(cv.amount, cv.currencyCode)}</span>;
    }
    case "number":
      return <span className="tabular-nums">{new Intl.NumberFormat("vi-VN").format(num(value))}</span>;
    case "date":
      return <span className="tabular-nums">{formatDate(String(value))}</span>;
    case "datetime": {
      const d = parseServerDateTime(String(value));
      return (
        <span className="tabular-nums">
          {formatDateTime(d)} <span className="slds-text-color_weak">· {relativeVN(d)}</span>
        </span>
      );
    }
    case "relation": {
      // TRI-STATE (RM-07 + EXP-01): expand → {id,label} LINKED to the target record; field-ACL
      // denies the target's labelField → {id} only (no label — render the id, NEVER "[object Object]");
      // no expand → raw id. Link target is /<relatedEntity>/<id> (from options.relatedEntity).
      if (value && typeof value === "object") {
        const ev = value as { id?: string; label?: string };
        if (typeof ev.label === "string" && ev.label !== "") {
          const target = typeof o.relatedEntity === "string" ? o.relatedEntity : "";
          // Link only when a real target can be built; else plain text (no misleading link).
          return target && ev.id ? <a href={`/${target}/${ev.id}`}>{ev.label}</a> : <span>{ev.label}</span>;
        }
        return <span className="slds-text-color_weak">{ev.id ?? ""}</span>;
      }
      return <span className="slds-text-color_weak">{String(value)}</span>;
    }
    case "user": {
      // Value is a userId. With ?expand= the server sends {id, label, email} (label = full_name,
      // may be empty for a user who never set one). Same tri-state discipline as `relation`:
      // name → email → raw id, and NEVER "[object Object]" — which is exactly what the missing
      // case produced before, since an expanded object fell through to the String(value) default.
      // No link: users are not records, there is no /users/:id screen to open.
      if (value && typeof value === "object") {
        const uv = value as { id?: string; label?: string; email?: string };
        const name = typeof uv.label === "string" && uv.label !== "" ? uv.label : "";
        const email = typeof uv.email === "string" && uv.email !== "" ? uv.email : "";
        if (name || email) return <span>{name || email}</span>;
        return <span className="slds-text-color_weak">{uv.id ?? ""}</span>;
      }
      // Not expanded (caller forgot ?expand=) — show the raw id rather than inventing a name.
      return <span className="slds-text-color_weak">{String(value)}</span>;
    }
    case "polymorphic_relation": {
      // Value is an atomic { type, id } (no server-side label expansion). Resolve the label
      // client-side and link to /<type>/<id>; isEmpty above already handled the unset case.
      if (value && typeof value === "object") {
        const pv = value as { type?: string; id?: string };
        if (pv.type && pv.id) return <PolymorphicDisplay value={{ type: pv.type, id: pv.id }} />;
      }
      return EMPTY;
    }
    case "rich_text":
      // Handoff: render HTML đã định dạng (sanitize ở P5 khi nối backend thật).
      return <span>{stripTags(String(value))}</span>;
    case "composite":
      return (
        <span className="inline-flex flex-wrap gap-x-3 gap-y-0.5">
          {(o.subFields ?? []).map((sf) => {
            const sv = obj(value)[sf.name];
            return isEmpty(sv) ? null : (
              <span key={sf.name}>
                <span className="slds-text-color_weak">{sf.label}: </span>
                <span>{String(sv)}</span>
              </span>
            );
          })}
        </span>
      );
    case "file":
      // Attachments live on the record's "Tệp" tab (file API) — a form/list cell has no inline
      // value to show. Render the standard empty marker instead of a fake "coming soon" badge.
      return EMPTY;
    case "image": {
      // Display a small thumbnail if value is a URL string or {url, name} object.
      const src = typeof value === "string" ? value : (value as Record<string, unknown>)?.url as string | undefined;
      if (!src) return EMPTY;
      return (
        // eslint-disable-next-line @next/next/no-img-element
        <img src={src} alt={String((value as Record<string, unknown>)?.name ?? "")} className="h-10 w-10 object-cover" />
      );
    }
    case "formula":
      // Computed server-side: always read-only, marked ƒ.
      //
      // MỘT XƯỞNG (2026-07-29): dùng CHUNG `formatComputed` với rollup. Trước đây nhánh này là
      // `String(value)` trần, nên "Thành tiền" của báo giá in ra `ƒ 3500000` — không dấu phẩy
      // nghìn, không đơn vị — trong khi nhánh rollup NGAY BÊN DƯỚI đã format đúng từ lâu. Cùng một
      // bài toán, hai lời giải, một cái bị bỏ quên: đúng lớp lỗi "hai đường trôi khỏi nhau" mà
      // chiến dịch này tồn tại để diệt. Phát hiện trên BẢN IN của một báo giá thật.
      // specs/053 §3.4: a text-result formula shows verbatim ("001" stays "001", not "1").
      return (
        <Computed
          glyph="ƒ"
          text={field.options?.resultType === "text" && value != null ? String(value) : formatComputed(value)}
        />
      );
    case "rollup":
      // Aggregate over child records (SUM/COUNT/AVG/MIN/MAX), computed server-side & read-only —
      // read-time opt-in via ?rollup= (the detail view requests it). The value is a COUNT/numeric agg
      // (number, or a DECIMAL string from MySQL) OR — when the aggregated field is a currency — a
      // {amount, currencyCode} object; formatComputed renders each (a naive String() would print
      // "[object Object]" for the currency case). Σ mark, mirroring the formula ƒ.
      return <Computed glyph="Σ" text={formatComputed(value)} />;
    case "text":
    default:
      return <span>{String(value)}</span>;
  }
}

function Computed({ glyph, text }: { glyph: string; text: string }) {
  return (
    <span>
      <span className="slds-text-color_weak slds-m-right_x-small" aria-hidden="true">
        {glyph}
      </span>
      <span className="tabular-nums">{text}</span>
    </span>
  );
}

// ── helpers ──
function isEmpty(v: unknown): boolean {
  if (v == null) return true;
  if (typeof v === "string") return v.trim() === "";
  if (Array.isArray(v)) return v.length === 0;
  if (typeof v === "object") return Object.keys(v as object).length === 0;
  return false; // 0 / false là giá trị hợp lệ, KHÔNG rỗng
}
const num = (v: unknown) => (typeof v === "number" ? v : Number(v) || 0);
const arr = (v: unknown) => (Array.isArray(v) ? (v as string[]) : []);
const obj = (v: unknown) => (v && typeof v === "object" ? (v as Record<string, unknown>) : {});
const stripTags = (s: string) => s.replace(/<[^>]*>/g, "");
// Direction-aware: a time still ahead reads "còn X" (it used to be "vừa xong" — now - d is negative).
export function relativeVN(d: Date, now: number = Date.now()): string {
  const diff = now - d.getTime();
  if (diff < 0) {
    const ahead = -diff;
    const h = Math.floor(ahead / 3_600_000);
    if (h < 1) return t("page.recordForm.inLessThanHour");
    if (h < 24) return tf("page.recordForm.inHours", { n: h });
    const days = Math.floor(h / 24);
    return days < 30 ? tf("page.recordForm.inDays", { n: days }) : formatDate(d);
  }
  const h = Math.floor(diff / 3_600_000);
  if (h < 1) return t("page.recordForm.justNow");
  if (h < 24) return tf("page.recordForm.hoursAgo", { n: h });
  const days = Math.floor(h / 24);
  return days < 30 ? tf("page.recordForm.daysAgo", { n: days }) : formatDate(d);
}
