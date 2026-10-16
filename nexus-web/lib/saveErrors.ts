// lib/saveErrors.ts — where a refused save is shown (specs/048 FE-before-save-hooks-1).
//
// A before_save / before_delete rule refuses a write with 422 RECORD_HOOK_VETO:
//   { error: <first violation's message>, code: "RECORD_HOOK_VETO",
//     details: { field?: <first anchor the caller may see>, violations: [{ field?, message }, …] } }
// Every violation is a sentence the Nexus team wrote for the user. The server already dropped the
// anchors of fields the caller's role may not see, so an anchor here is always safe to show.
// When the rules cannot be read the server refuses with 503 RECORD_HOOKS_UNAVAILABLE ("busy, retry").
//
// Two readers, one module so every screen says the same thing:
//   • mapSaveError   — the record form: each violation under its input when that input is drawn and
//                      writable, every other one in the form-level notification, verbatim.
//   • formatSaveError — screens with ONE line of text (toast, a list's result strip, a batch row):
//                      the first violation's sentence, never an error code or a field key.
import { ApiError, type FieldMeta } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";

export const RECORD_HOOK_VETO = "RECORD_HOOK_VETO";

export interface Violation {
  field?: string;
  message: string;
}

/** The violations of a RECORD_HOOK_VETO in server order. A veto without the list is one violation. */
export function vetoViolations(err: ApiError): Violation[] {
  const raw = err.details.violations;
  const list = Array.isArray(raw)
    ? raw.flatMap((v): Violation[] => {
        const o = v as { field?: unknown; message?: unknown } | null;
        if (!o || typeof o.message !== "string" || o.message === "") return [];
        return [{ message: o.message, ...(typeof o.field === "string" && o.field ? { field: o.field } : {}) }];
      })
    : [];
  if (list.length > 0) return list;
  const field = typeof err.details.field === "string" && err.details.field ? err.details.field : undefined;
  return err.message ? [{ message: err.message, ...(field ? { field } : {}) }] : [];
}

export interface SaveErrorMap {
  /** Root field name → message, only for inputs the form draws and the caller may write. */
  fieldErrors: Record<string, string>;
  /** Everything with no input to sit under, for the form-level notification, in order. */
  banner: string[];
}

/**
 * Place a failed save on the record form.
 *
 * `rendered` = the fields the form draws right now (a field left off the layout, or hidden by a
 * visibility rule, is not in it); a `readOnly` one is drawn as text — field-ACL lock or the layout
 * cell's read-only toggle, the caller merges both — so it has no input to carry an error. `all` =
 * every field of the entity as served (its `readOnly` is the role's lock only), for naming a field
 * the layout left out and for telling the two kinds of read-only apart.
 * 409 is not handled here — the form shows its own reload banner for a version conflict.
 */
export function mapSaveError(err: unknown, rendered: readonly FieldMeta[], all: readonly FieldMeta[] = rendered): SaveErrorMap {
  const out: SaveErrorMap = { fieldErrors: {}, banner: [] };
  if (!(err instanceof ApiError)) {
    out.banner.push(err instanceof Error && err.message ? err.message : t("page.recordForm.errNetwork"));
    return out;
  }
  const writable = (root: string) => rendered.some((f) => f.name === root && !f.readOnly);

  // A rule's sentence is shown as written: under its input, or in the notification. Never wrapped in
  // "required field missing from this form" / "locked for your role" — a rule is neither.
  if (err.code === RECORD_HOOK_VETO) {
    for (const v of vetoViolations(err)) {
      const root = v.field?.split(".")[0];
      if (root && writable(root) && !(root in out.fieldErrors)) out.fieldErrors[root] = v.message;
      else out.banner.push(v.message);
    }
    if (Object.keys(out.fieldErrors).length === 0 && out.banner.length === 0) out.banner.push(errorMessage(err));
    return out;
  }

  const field = typeof err.details.field === "string" ? err.details.field : undefined;
  const root = field?.split(".")[0];
  const label = (name: string) => all.find((f) => f.name === name)?.label ?? rendered.find((f) => f.name === name)?.label ?? name;
  if (err.code === "RECORD_REQUIRED_LOCKED") {
    // queue #62: the server names the locked field and the admin's type-aware way out — alone,
    // wherever the field sits (a layout that omits it would otherwise say "add it to the layout").
    out.banner.push(err.message);
  } else if (root && writable(root)) {
    // Composite sub-paths ("fullname.first") anchor on the rendered root input.
    out.fieldErrors[root] = err.message;
  } else if (root && rendered.some((f) => f.name === root && f.readOnly)) {
    // field-lock-signal: on the form, as text — there is no input, and "add it to the layout" would lie.
    // Drawn as text by the role (`all` keeps the field-ACL lock) or by the layout's read-only cell —
    // blaming the role for the layout's choice would send the user to the wrong admin setting.
    const byRole = all.some((f) => f.name === root && f.readOnly);
    out.banner.push(tf(byRole ? "page.recordForm.errLocked" : "page.recordForm.errReadOnlyCell", { label: label(root), message: err.message }));
  } else if (root && all.some((f) => f.name === root)) {
    out.banner.push(tf("page.recordForm.errNotOnForm", { label: label(root), message: err.message }));
  } else {
    out.banner.push(err.message || errorMessage(err));
  }
  return out;
}

/** One row of a batch result (bulk update/delete, bulk create): `{ code, error, details }`. */
export interface SaveErrorRow {
  code?: string;
  error?: string;
  details?: Record<string, unknown>;
}

const isRow = (v: unknown): v is SaveErrorRow =>
  typeof v === "object" && v !== null && !(v instanceof Error) && ("code" in v || "error" in v);

/**
 * One sentence for a refused write — an ApiError or a batch row. A veto gives its first violation;
 * everything else goes through errorMessage (code map, else the server's sentence, else a generic
 * Vietnamese line). Never an error code, never a field key.
 */
export function formatSaveError(err: unknown): string {
  const e = isRow(err) ? new ApiError(422, err.code ?? "", err.error ?? "", err.details ?? {}) : err;
  if (e instanceof ApiError && e.code === RECORD_HOOK_VETO) return vetoViolations(e)[0]?.message || errorMessage(null);
  return errorMessage(e);
}
