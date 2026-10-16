// Shared by the embedded-surface gates (specs/073 T091/T092). Not a test file. The test file itself must
// vi.mock("@/lib/auth/AuthProvider") and vi.mock("next/navigation") (vi.mock is hoisted per file).
import { vi } from "vitest";
import { render } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ActiveAppProvider } from "@/lib/apps";
import { NavigationBlockerProvider } from "@/lib/navigation-blocker";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { EMBEDDED_RULES, type EmbeddedEntry, type EmbeddedRule } from "./studio-embedded-registry";

/** `fetch` that answers from `routes` (URL substring, longest wins) and `{ data: [] }` for anything else. */
export function fakeFetch(routes: Record<string, unknown> = {}): typeof fetch {
  return vi.fn(async (url: RequestInfo | URL) => {
    const u = String(url);
    const key = Object.keys(routes).filter((k) => u.includes(k)).sort((a, b) => b.length - a.length)[0];
    return new Response(JSON.stringify(key ? routes[key] : { data: [] }), { status: 200, headers: { "Content-Type": "application/json" } });
  }) as unknown as typeof fetch;
}

const CUSTOMER_PICTURES = '[data-testid="branding-preview"], [data-testid="customer-preview-frame"]';

/**
 * Rules whose selector matches at least once inside `root`, with the count. A subtree under those two test ids (not any `.nx-face-customer`) is a
 * picture of the customer's screen (BrandPreview, CustomerPreviewFrame; kh3 T236), so the customer's own tabs and
 * buttons there are not Studio chrome and are not counted.
 */
export function measureEmbedded(root: ParentNode): Partial<Record<EmbeddedRule, number>> {
  const out: Partial<Record<EmbeddedRule, number>> = {};
  for (const [rule, sel] of Object.entries(EMBEDDED_RULES) as [EmbeddedRule, string][]) {
    const n = [...root.querySelectorAll(sel)].filter((el) => !el.closest(CUSTOMER_PICTURES)).length;
    if (n > 0) out[rule] = n;
  }
  return out;
}

export const settle = () => new Promise((r) => setTimeout(r, 400));

/** Mounts one registry entry inside EmbeddedSurface (its `canvas` flag honoured) against its fake network. */
export async function renderEmbedded(entry: EmbeddedEntry) {
  global.fetch = fakeFetch(entry.routes);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const ui = await entry.render!();
  const view = render(
    <QueryClientProvider client={qc}>
      <ActiveAppProvider>
        <NavigationBlockerProvider>
          <EmbeddedSurface canvas={entry.canvas}>{ui}</EmbeddedSurface>
        </NavigationBlockerProvider>
      </ActiveAppProvider>
    </QueryClientProvider>,
  );
  await settle();
  return view;
}
