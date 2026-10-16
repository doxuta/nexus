// layoutSchema — sinh zod schema ĐỘNG từ field metadata (doc §4.1, D3 Founder: rhf + zod).
//
// LayoutForm (T5) feeds this schema into zodResolver so react-hook-form validates client-side
// (reward-early / punish-late) BEFORE hitting the API. The server still validates authoritatively
// (422), but the dynamic schema catches the common cases inline:
//   • required        → non-empty (string .min(1), array .min(1), required number/boolean present)
//   • email           → z.email()
//   • number/currency → z.coerce.number() (+ min/max)  — widgets store these as strings
//   • text/rich_text  → maxLength → .max(), pattern → .regex()
//   • picklist        → z.enum(values) when options.values present
//   • multi_select    → z.array(string) (+ min 1 when required)
//
// Built table-driven per fieldType so adding a type is one switch arm. Unknown types fall back to
// a permissive z.any() (forward-compat, mirrors the registry's "no applicable renderer" rule).

import { z } from "zod";
import type { FieldMeta } from "@/lib/api";

// VN error messages (the form surfaces these under each field).
const MSG = {
  required: "Bắt buộc nhập",
  email: "Email không hợp lệ",
  number: "Phải là số",
  pattern: "Định dạng không hợp lệ",
  tooLong: (n: number) => `Tối đa ${n} ký tự`,
  min: (n: number) => `Tối thiểu ${n}`,
  max: (n: number) => `Tối đa ${n}`,
  pickOne: "Chọn một giá trị",
};

// A field's options blob (sys_fields.options JSON) — only the bits validation cares about.
interface FieldValidationOptions {
  maxLength?: number;
  pattern?: string;
  min?: number;
  max?: number;
  values?: string[];
}

// Build the zod type for ONE field from its metadata.
export function fieldZodType(field: FieldMeta): z.ZodTypeAny {
  const required = field.isRequired ?? false;
  const o = (field.options ?? {}) as FieldValidationOptions;

  switch (field.fieldType) {
    case "email": {
      let s = z.string().trim();
      if (required) s = s.min(1, MSG.required);
      // Allow empty when optional; otherwise must be a valid email.
      const withEmail = required
        ? s.pipe(z.email(MSG.email))
        : z.union([z.literal(""), z.email(MSG.email)]);
      return withEmail;
    }

    case "number":
    case "currency": {
      // Widgets store these as strings ("" when empty) or numbers — coerce, allow empty when optional.
      const base = z.coerce.number({ message: MSG.number });
      let n = base;
      if (typeof o.min === "number") n = n.min(o.min, MSG.min(o.min));
      if (typeof o.max === "number") n = n.max(o.max, MSG.max(o.max));
      // #275: required + empty must fail HERE — z.coerce turns "" into 0 and the server would never
      // hear about it (buildPayload omits the key, so an edit would silently keep the old value).
      if (required) return z.any().refine((v) => !(v === "" || v == null), MSG.required).pipe(n);
      // Optional number: accept empty string / undefined → undefined, else coerce.
      return z.preprocess(
        (v) => (v === "" || v == null ? undefined : v),
        n.optional(),
      );
    }

    case "boolean":
      return z.coerce.boolean();

    case "rating": {
      let n = z.coerce.number({ message: MSG.number });
      if (typeof o.max === "number") n = n.max(o.max, MSG.max(o.max));
      n = n.min(required ? 1 : 0, required ? MSG.required : MSG.min(0));
      return n;
    }

    case "picklist": {
      const values = o.values ?? [];
      if (values.length > 0) {
        const e = z.enum(values as [string, ...string[]], { message: MSG.pickOne });
        return required ? e : z.union([z.literal(""), e]);
      }
      // No constrained values → plain string.
      let s = z.string();
      if (required) s = s.min(1, MSG.pickOne);
      return s;
    }

    case "multi_select": {
      let a = z.array(z.string());
      if (required) a = a.min(1, MSG.required);
      return a;
    }

    case "relation": {
      // Stored as the related record's id (string). Required → non-empty.
      let s = z.string();
      if (required) s = s.min(1, MSG.required);
      return s;
    }

    case "polymorphic_relation": {
      // Atomic { type, id } object (or null/"" when unset — the picker emits null until both set).
      // Lenient by design: accept the empty states so a valid-empty optional never blocks submit;
      // when required, both halves must be present. Server validates target membership/existence.
      const ref = z.object({ type: z.string(), id: z.string() });
      const any = z.union([ref, z.literal(""), z.null()]);
      if (required) {
        // Accept every shape first, then refine so an unset value reports MSG.required (not a raw
        // zod "expected object, received null" message).
        return any.refine(
          (v) => v != null && v !== "" && typeof v === "object" && v.type !== "" && v.id !== "",
          { message: MSG.required },
        );
      }
      return any.optional();
    }

    case "composite":
      // Nested object of sub-values; light validation only (sub-required handled by server).
      return z.record(z.string(), z.unknown());

    case "file":
    case "image":
      // No input in the form — the "Tệp" tab owns these refs and buildPayload never sends them
      // (ATTACHMENT_FIELD_TYPES, specs/060), so nothing here can block a save.
      return z.any();

    case "formula":
      // Formula fields are always read-only and never submitted (server computes them).
      return z.any();

    case "date":
    case "datetime": {
      let s = z.string();
      if (required) s = s.min(1, MSG.required);
      return s;
    }

    case "rich_text":
    case "text": {
      let s = z.string();
      if (required) s = s.trim().min(1, MSG.required);
      if (typeof o.maxLength === "number") s = s.max(o.maxLength, MSG.tooLong(o.maxLength));
      if (o.pattern) {
        try {
          s = s.regex(new RegExp(o.pattern), MSG.pattern);
        } catch {
          // Invalid stored regex → skip the constraint rather than crash the form.
        }
      }
      return s;
    }

    default:
      // Forward-compat: unknown/new types validate permissively (registry fallback rule).
      return z.any();
  }
}

// Assemble a z.object schema from a list of fields. Pass the SAME field list the form renders so
// the keys line up with react-hook-form's values. Unknown/extra keys are stripped by default.
export function buildLayoutSchema(fields: FieldMeta[]): z.ZodObject<Record<string, z.ZodTypeAny>> {
  const shape: Record<string, z.ZodTypeAny> = {};
  for (const f of fields) shape[f.name] = fieldZodType(f);
  return z.object(shape);
}
