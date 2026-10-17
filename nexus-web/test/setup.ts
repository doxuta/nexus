// Test setup — runs before every test file.
// Imports @testing-library/jest-dom matchers (toBeInTheDocument, toHaveClass, etc.).
import "@testing-library/jest-dom";

// No unit test reaches a server. A fetch no test mocked (logout() in a beforeEach, say) used to go out to whatever
// listened on localhost:8080; it now fails like a dropped connection. A test that wants a response installs its own
// fetch over this one (global.fetch = …, vi.stubGlobal, vi.spyOn(…).mockResolvedValue). test/hermetic-fetch.test.ts.
globalThis.fetch = async (input) => {
  throw new TypeError(`network disabled in tests: ${input instanceof Request ? input.url : String(input)}`);
};
