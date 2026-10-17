// A unit test never reaches a server. test/setup.ts swaps fetch for one that refuses, so a call no test mocked fails
// inside the test run instead of going out: logout() in the beforeEach of McpConnect and ApiKeys used to POST
// http://localhost:8080/api/v1/auth/logout to whatever app listens there. A test that wants a response installs its
// own fetch over this one.
import { describe, expect, it } from "vitest";

describe("fetch in unit tests", () => {
  it("refuses the network", async () => {
    const url = "http://localhost:8080/api/v1/auth/logout";
    await expect(fetch(url, { method: "POST", body: "{}" })).rejects.toThrow(`network disabled in tests: ${url}`);
    await expect(window.fetch(new Request(url))).rejects.toThrow(`network disabled in tests: ${url}`);
  });
});
