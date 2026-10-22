// Test setup — runs before every test file.
// Imports @testing-library/jest-dom matchers (toBeInTheDocument, toHaveClass, etc.).
import "@testing-library/jest-dom";

// No unit test reaches a server. A fetch no test mocked (logout() in a beforeEach, say) used to go out to whatever
// listened on localhost:8080; it now fails like a dropped connection. A test that wants a response installs its own
// fetch over this one (global.fetch = …, vi.stubGlobal, vi.spyOn(…).mockResolvedValue). test/hermetic-fetch.test.ts.
globalThis.fetch = async (input) => {
  throw new TypeError(`network disabled in tests: ${input instanceof Request ? input.url : String(input)}`);
};

// The suite asserts the Vietnamese default UI. happy-dom's browser lists only English, which getLocale() (lib/i18n)
// would turn into the English UI (issue #6), so the test browser lists Vietnamese. A test that needs another browser
// language spies on this getter (lib/i18n/i18n.test.ts).
Object.defineProperty(window.navigator, "languages", { get: () => ["vi-VN", "vi"], configurable: true });
