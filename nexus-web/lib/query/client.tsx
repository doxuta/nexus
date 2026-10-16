"use client";

// QueryClientProvider — wraps the whole app in TanStack Query context.
// All client components use useQuery/useMutation through this provider.
import { MutationCache, QueryClient, QueryClientProvider, keepPreviousData } from "@tanstack/react-query";
import { ReactQueryDevtools } from "@tanstack/react-query-devtools";
import { useState, type ReactNode } from "react";
import { qk } from "./keys";

// Stale-time strategy (per fe-architecture spec):
//   - Metadata (entities/fields/layouts/permissions): 10 min stale / 30 min gc
//   - Records: 2 min stale (user data changes frequently)
//   - Notifications: 30s stale (near-real-time)
//   - Me/Auth: 5 min stale
const defaultStaleTime = 2 * 60 * 1000; // 2 min

// Exported so tests can build a client with the app's real defaults (keepPreviousData is the trap, specs/073 FR-025).
export function makeClient() {
  const client: QueryClient = new QueryClient({
    // Any mutation that succeeds may have changed a project (industry, trial switch, a new project): re-read the ledger.
    mutationCache: new MutationCache({ onSuccess: () => void client.invalidateQueries({ queryKey: qk.projects() }) }),
    defaultOptions: {
      queries: {
        staleTime: defaultStaleTime,
        gcTime: 10 * 60 * 1000,
        retry: 1,
        refetchOnWindowFocus: false,
        // D17: keep the last data on screen while a query key changes or a transient auth blip
        // re-fetches — the sidebar/content don't blank out during a silent token refresh.
        placeholderData: keepPreviousData,
      },
    },
  });
  return client;
}

export function NexusQueryProvider({ children }: { children: ReactNode }) {
  // useState ensures each browser session gets a fresh QueryClient
  const [client] = useState(makeClient);
  return (
    <QueryClientProvider client={client}>
      {children}
      {/* Off by default (incl. dev) so the live demo has no floating devtools badge; set
          NEXT_PUBLIC_SHOW_DEVTOOLS=true locally to bring it back for debugging. */}
      {process.env.NEXT_PUBLIC_SHOW_DEVTOOLS === "true" && (
        <ReactQueryDevtools initialIsOpen={false} />
      )}
    </QueryClientProvider>
  );
}
