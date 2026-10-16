// formulaLive — pure state machine for the W5 LIVE formula editor (plan §3.2).
//
// The editor debounces keystrokes then POSTs /admin/formula/validate (zero-write dry-run that
// runs the EXACT config-time validator — api-conventions §10.3). Because responses can land out
// of order (fast typing + slow network), every request carries a monotonically increasing `seq`;
// a response only applies if its seq matches the latest issued one (stale responses are dropped).
//
// Pure + framework-free so the transition table is unit-testable without timers or fetch.

export type FormulaState =
  | { kind: "idle" } // empty formula — nothing to validate
  | { kind: "typing"; seq: number } // debounce window open
  | { kind: "checking"; seq: number } // request in flight
  | { kind: "valid"; seq: number; fields: string[]; resultType: string }
  | { kind: "invalid"; seq: number; code: string; message: string };

export type FormulaEvent =
  | { type: "input"; formula: string; seq: number } // keystroke (seq = new latest)
  | { type: "debounce-fire"; seq: number } // timer elapsed → fire request
  | { type: "response-ok"; seq: number; fields: string[]; resultType: string }
  | { type: "response-err"; seq: number; code: string; message: string };

export const FORMULA_DEBOUNCE_MS = 450;

export function initialFormulaState(): FormulaState {
  return { kind: "idle" };
}

/** Latest seq the state has seen — responses with a lower seq are stale. */
function stateSeq(s: FormulaState): number {
  return s.kind === "idle" ? 0 : s.seq;
}

export function formulaReducer(state: FormulaState, ev: FormulaEvent): FormulaState {
  switch (ev.type) {
    case "input":
      // Empty formula → idle (no request; the field is simply unconfigured yet).
      if (ev.formula.trim() === "") return { kind: "idle" };
      return { kind: "typing", seq: ev.seq };
    case "debounce-fire":
      // Only the LATEST debounce moves to checking; an old timer that somehow fires is ignored.
      if (state.kind !== "typing" || state.seq !== ev.seq) return state;
      return { kind: "checking", seq: ev.seq };
    case "response-ok":
      if (ev.seq !== stateSeq(state)) return state; // stale response — a newer keystroke exists
      return { kind: "valid", seq: ev.seq, fields: ev.fields, resultType: ev.resultType };
    case "response-err":
      if (ev.seq !== stateSeq(state)) return state;
      return { kind: "invalid", seq: ev.seq, code: ev.code, message: ev.message };
  }
}

/**
 * Shape the validate request body — null when there is nothing worth sending (empty formula or
 * no entity yet). resultType rides along for the three values the backend accepts —
 * number|currency|text (specs/053). Absent, the backend judges the formula as a number, so a
 * dropped "text" hint turns a valid text formula into a 422 and blocks the inspector's Save.
 */
export function shapeValidateRequest(
  entityName: string,
  formula: string,
  resultType?: string,
): { entityName: string; formula: string; resultType?: string } | null {
  const f = formula.trim();
  if (!entityName || f === "") return null;
  const body: { entityName: string; formula: string; resultType?: string } = { entityName, formula: f };
  if (resultType === "number" || resultType === "currency" || resultType === "text") body.resultType = resultType;
  return body;
}
