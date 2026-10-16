// Installed pack version vs the version shipped with Nexus (specs/079 f7-nang-cap-hien-co). Pure.
import type { BuiltinVertical } from "@/lib/api";
import { sortInstallsNewestFirst, type InstallRecord } from "@/lib/verticalPlan";

export type PackStatus =
  | { kind: "newer"; installed: string; available: string }
  | { kind: "same" | "local-newer"; installed: string; available: string }
  | { kind: "not-available" | "from-file" | "invalid"; installed: string };

const XYZ = /^\d+\.\d+\.\d+$/;

/** Numeric x.y.z compare; null when either side is not x.y.z.
 *  ponytail: no full semver; a prefix or suffix means "invalid". */
export function comparePackVersions(a: string, b: string): number | null {
  if (!XYZ.test(a) || !XYZ.test(b)) return null;
  const pa = a.split(".").map(Number);
  const pb = b.split(".").map(Number);
  for (let i = 0; i < 3; i++) if (pa[i] !== pb[i]) return pa[i] < pb[i] ? -1 : 1;
  return 0;
}

/** The install ledger is history: keep the newest row per pack code. */
export function latestInstallBySlug(installs: InstallRecord[]): InstallRecord[] {
  const seen = new Set<string>();
  return sortInstallsNewestFirst(installs).filter((r) => !seen.has(r.slug) && !!seen.add(r.slug));
}

export function packStatus(install: InstallRecord, available: BuiltinVertical[]): PackStatus {
  const installed = install.version ?? "";
  if (!install.slug) return { kind: "from-file", installed };
  const shipped = available.find((b) => b.slug === install.slug);
  if (!shipped) return { kind: "not-available", installed };
  const avail = shipped.manifest?.version ?? "";
  const c = comparePackVersions(installed, avail);
  if (c === null) return { kind: "invalid", installed };
  return { kind: c < 0 ? "newer" : c > 0 ? "local-newer" : "same", installed, available: avail };
}

export function hasPackUpgrade(installs: InstallRecord[], builtins: BuiltinVertical[]): boolean {
  return latestInstallBySlug(installs).some((r) => packStatus(r, builtins).kind === "newer");
}
