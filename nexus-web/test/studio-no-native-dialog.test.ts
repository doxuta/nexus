// specs/073 kh1a T174 (FR-065): destructive actions are confirmed IN the page (InlineConfirm, Prompt that retypes a code),
// never with the browser's own confirm/alert/prompt: those cannot name the target well, cannot be tested, and look foreign.
import { existsSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { NATIVE_DIALOG_RE, countPerFile, findNativeDialogs, listSources, ratchet, read } from "./studioGateScan";

// Outside the scanned folders, listed here so it is visible: leaving a page with unsaved changes is a synchronous
// answer to a link click (it must preventDefault in the same tick), not a confirmation of a destructive action.
const OUTSIDE_SCOPE = { "lib/navigation-blocker.tsx": "leave the page and drop unsaved changes: synchronous, not a destructive-action confirm" };

// Screens drawn in the Studio but living outside its folders (specs/079 FR-007).
const OUTSIDE_FOLDERS = ["components/screens/admin/DoctorScreen.tsx"];

describe("Studio uses no native browser dialog", () => {
  it("no window.confirm / alert / prompt (or a bare call) beyond the allow-list", () => {
    expect(ratchet("nativeDialog", countPerFile(NATIVE_DIALOG_RE, [...listSources(), ...OUTSIDE_FOLDERS]))).toEqual([]);
  });

  it("self-check: a sample with window.confirm is caught, our own handlers are not", () => {
    expect(findNativeDialogs("if (window.confirm('Xoá?')) del();")).toHaveLength(1);
    expect(findNativeDialogs("alert('x')")).toHaveLength(1);
    expect(findNativeDialogs("const v = prompt('Mã?')")).toHaveLength(1);
    expect(findNativeDialogs("onConfirm();")).toHaveLength(0);
    expect(findNativeDialogs("async function confirm() {}")).toHaveLength(0);
    expect(findNativeDialogs("dialog.confirm(x)")).toHaveLength(0);
    expect(findNativeDialogs("// window.confirm is banned")).toHaveLength(0);
  });

  it("the one out-of-scope exception is still real (keeps the list honest)", () => {
    for (const file of Object.keys(OUTSIDE_SCOPE)) {
      expect(existsSync(join(process.cwd(), file))).toBe(true);
      expect(findNativeDialogs(read(file)).length).toBeLessThanOrEqual(1);
    }
  });
});
