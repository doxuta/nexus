// Landing "/" (issue #8): the agent-native copy, in the reader's language, hydrated without a mismatch (React #418).
import { afterEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { render, screen } from "@testing-library/react";
import { renderToString } from "react-dom/server";
import { hydrateRoot } from "react-dom/client";
import viCatalog from "@/lib/i18n/vi";
import enCatalog from "@/lib/i18n/en";
import { Landing } from "./Landing";

vi.mock("next/navigation", () => ({ useRouter: () => ({ replace: vi.fn() }) }));
// next/image needs Next's loader; the test only reads the alt text.
// eslint-disable-next-line @next/next/no-img-element
vi.mock("next/image", () => ({ default: ({ alt }: { alt: string }) => <img alt={alt} /> }));

const prefer = (locale: "vi" | "en") => localStorage.setItem("nexus.prefs.v1", JSON.stringify({ locale }));
const landingKeys = Object.keys(viCatalog).filter((k) => k.startsWith("landing.")) as (keyof typeof viCatalog)[];
const en = enCatalog as Record<string, string>;

afterEach(() => localStorage.clear());

describe("Landing", () => {
  it("renders the Vietnamese copy by default, with the demo, repo, README and write-up links", () => {
    render(<Landing />);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe(viCatalog["landing.hero.title"]);
    const hrefs = screen.getAllByRole("link").map((a) => a.getAttribute("href"));
    expect(hrefs).toEqual(expect.arrayContaining([
      "/login",
      "https://github.com/doxuta/nexus",
      "https://github.com/doxuta/nexus#readme",
      "https://github.com/doxuta/nexus/blob/main/docs/writeup/turning-the-lights-back-on.md",
    ]));
  });

  it("renders English for a reader who chose English", () => {
    prefer("en");
    render(<Landing />);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe(en["landing.hero.title"]);
    expect(screen.getByText(en["landing.diff.measured.title"])).toBeTruthy();
  });

  it("hydrates the server's Vietnamese without a mismatch, then switches to the reader's English", async () => {
    // The server knows no preference: it renders Vietnamese. The reader's browser prefers English.
    const html = renderToString(<Landing />);
    expect(html).toContain(viCatalog["landing.hero.title"]);
    prefer("en");

    const container = document.createElement("div");
    container.innerHTML = html;
    document.body.appendChild(container);
    const recoverable: unknown[] = [];
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    await act(async () => {
      hydrateRoot(container, <Landing />, { onRecoverableError: (e) => recoverable.push(e) });
    });
    const logged = consoleError.mock.calls.map((c) => String(c[0]));
    consoleError.mockRestore();

    expect(recoverable).toEqual([]);
    expect(logged.filter((m) => /hydrat|did not match|#418/i.test(m))).toEqual([]);
    expect(container.querySelector("h1")?.textContent).toBe(en["landing.hero.title"]);
    container.remove();
  });

  it("every landing string has English", () => {
    expect(landingKeys.length).toBeGreaterThan(30);
    expect(landingKeys.filter((k) => !en[k])).toEqual([]);
  });

  // Verifier round 1 (#64): isolation is "a convention plus tests" (ADR 0005), so no "safe by construction"; no
  // unmeasured number.
  it("the copy drops the internal-factory framing, claims nothing the README does not, and has no em dash", () => {
    const framing = /nội bộ|nhà máy|bàn giao|factory|handover|DOTB|90\/10|—|by construction|theo cấu tạo|2[.,]000/i;
    const values = landingKeys.flatMap((k) => [viCatalog[k], en[k]]);
    expect(values.filter((v) => framing.test(v))).toEqual([]);
  });
});
