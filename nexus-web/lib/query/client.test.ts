// specs/073 so1a T454: any successful mutation re-reads the project ledger — the board, the switcher and the navy band
// all read ["projects"], so a changed industry / trial switch / created project must reach all three.
import { describe, expect, it } from "vitest";
import { makeClient } from "@/lib/query/client";
import { qk } from "@/lib/query/keys";

const PROJECTS_KEY = qk.projects();

describe("makeClient", () => {
  it("marks ['projects'] stale after a successful mutation, and leaves other queries alone", async () => {
    const client = makeClient();
    client.setQueryData(PROJECTS_KEY, [{ slug: "x" }]);
    client.setQueryData(["entities"], [{ name: "e" }]);
    await client.getMutationCache().build(client, { mutationFn: async () => "ok" }).execute(undefined);
    expect(client.getQueryState(PROJECTS_KEY)?.isInvalidated).toBe(true);
    expect(client.getQueryState(["entities"])?.isInvalidated).toBe(false);
  });

  it("a failed mutation changes nothing", async () => {
    const client = makeClient();
    client.setQueryData(PROJECTS_KEY, []);
    await client
      .getMutationCache()
      .build(client, { mutationFn: async () => Promise.reject(new Error("no")) })
      .execute(undefined)
      .catch(() => undefined);
    expect(client.getQueryState(PROJECTS_KEY)?.isInvalidated).toBe(false);
  });
});
