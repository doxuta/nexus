// specs/073 T092 (FR-034, D-UI-27): creating or editing in the Studio happens IN the page, not in a modal. Today the
// builders still open dialogs; each one is a row of test/studio-dialog-ledger.ts (component, label, kind, owner slice).
// Two gates keep the ledger honest:
//   1. closed ledger: the dialogs grep finds in a file EQUAL its ledger rows (a new dialog is red; a dialog that is
//      removed must remove its row, so rows only ever go away);
//   2. behaviour: a builder that has a "tạo" row really opens a dialog when its create button is pressed, and a
//      builder without one opens none (so an exemption cannot go stale and a new modal cannot hide).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { EMBEDDED_REGISTRY } from "./studio-embedded-registry";
import { DIALOG_LEDGER, DIALOG_RE, DIALOG_SCAN_DIRS, DIALOG_SCAN_FILES } from "./studio-dialog-ledger";
import { countPerFile, listSources } from "./studioGateScan";
import { renderEmbedded } from "./studioEmbeddedHarness";

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u-1", isAdmin: true }, isAuthenticated: true }) }));
vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), back: vi.fn(), prefetch: vi.fn() }),
  usePathname: () => "/studio",
  useSearchParams: () => new URLSearchParams(),
}));

const originalFetch = global.fetch;
beforeEach(() => {
  window.localStorage.setItem("nexus_remember", "1");
  window.localStorage.setItem("nexus_token", "test-token");
});
afterEach(() => {
  cleanup();
  global.fetch = originalFetch;
  window.localStorage.clear();
});

const rowsPerFile = (): Record<string, number> => {
  const out: Record<string, number> = {};
  for (const [file] of DIALOG_LEDGER) out[file] = (out[file] ?? 0) + 1;
  return out;
};

describe("the dialog ledger is closed", () => {
  it("dialogs found by grep == ledger rows, per file", () => {
    const files = [...listSources(DIALOG_SCAN_DIRS), ...DIALOG_SCAN_FILES.filter((f) => existsSync(join(process.cwd(), f)))];
    const found = countPerFile(DIALOG_RE, files);
    const rows = rowsPerFile();
    const diffs = [...new Set([...Object.keys(found), ...Object.keys(rows)])]
      .filter((f) => (found[f] ?? 0) !== (rows[f] ?? 0))
      .map((f) => `${f}: grep ${found[f] ?? 0}, ledger ${rows[f] ?? 0} (${(found[f] ?? 0) > (rows[f] ?? 0) ? "a new dialog: build the in-place form instead" : "dialog gone: delete its row"})`);
    expect(diffs).toEqual([]);
  });

  it("every row names a real file, a known kind and an owner slice; no row is written twice", () => {
    const seen = new Set<string>();
    for (const [file, label, kind, owner] of DIALOG_LEDGER) {
      expect(existsSync(join(process.cwd(), file)), file).toBe(true);
      expect(["tạo", "sửa", "xác nhận"]).toContain(kind);
      expect(owner, `${file} · ${label}`).toMatch(/^(F[2-7]|kh\d\w*)$/);
      const key = `${file}|${label}`;
      expect(seen.has(key), `duplicate row ${key}`).toBe(false);
      seen.add(key);
    }
  });

});

describe("pressing create in an embedded builder", () => {
  const withCreate = EMBEDDED_REGISTRY.filter((e) => !e.pending && e.openCreate && e.file);

  it("the registry has builders to press (the check is not vacuous)", () => {
    expect(withCreate.length).toBeGreaterThanOrEqual(4);
  });

  it.each(withCreate.map((e) => [e.id] as const))("%s: opens a dialog exactly when the ledger has a create row for it", async (id) => {
    const entry = withCreate.find((e) => e.id === id)!;
    await renderEmbedded(entry);
    const buttons = await screen.findAllByRole("button", { name: entry.openCreate! });
    await userEvent.click(buttons[0]);
    await new Promise((r) => setTimeout(r, 300));
    const dialogs = document.body.querySelectorAll("[role=dialog],[role=alertdialog]").length;
    const ledgerCreate = DIALOG_LEDGER.some(([file, , kind]) => file === entry.file && kind === "tạo");
    expect(dialogs > 0, `${id}: ${dialogs} dialog(s) after "${entry.openCreate}"; ledger create row: ${ledgerCreate}`).toBe(ledgerCreate);
  });
});

// specs/074 lát f2-access-matrix (T191): the Studio path of "Người dùng & quyền" no longer goes through RolesList. Its Modal/Prompt
// rows stay in the ledger only because /admin/roles (the customer face) still draws them; the Studio page creates a role in place.
describe("RolesList is the customer face only", () => {
  it("no Studio route mounts it and no Studio source imports it", async () => {
    const { ROUTES } = await import("@/lib/studioRoutes");
    expect(ROUTES.filter((r) => r.mounts === "RolesList").map((r) => r.id)).toEqual([]);
    const importers = listSources(["app/studio", "components/studio"]).filter((f) => /screens\/admin\/RolesList/.test(readFileSync(join(process.cwd(), f), "utf8")));
    expect(importers).toEqual([]);
  });
});
