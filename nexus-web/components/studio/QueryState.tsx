"use client";

import type { ReactNode } from "react";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { errorMessage } from "@/lib/errorMessages";
import { t } from "@/lib/i18n";

/** The few fields of a React Query result this component reads. */
export interface QueryLike {
  isPending: boolean;
  isError: boolean;
  error: unknown;
  data: unknown;
  refetch: () => unknown;
}

const isEmptyData = (d: unknown) => d == null || (Array.isArray(d) && d.length === 0);

/**
 * Loading / error / empty for a working surface (specs/073 FR-018). Loading is a Spinner (no hand-made skeleton), the
 * error is the sentence for the server's code plus "Thử lại", empty is whatever the page passes (a compact left-aligned
 * state, not a 96px glyph). "Not built yet" is PendingPage, not this.
 */
export function QueryState({ query, empty, children }: { query: QueryLike; empty?: ReactNode; children: ReactNode }) {
  if (query.isPending) {
    return (
      <div className="slds-is-relative slds-p-around_xx-large">
        <Spinner size="medium" />
      </div>
    );
  }
  if (query.isError) {
    return (
      <div className="slds-p-around_medium">
        <ScopedNotification theme="error">{errorMessage(query.error)}</ScopedNotification>
        <div className="slds-m-top_small">
          <Button variant="neutral" onClick={() => void query.refetch()}>
            {t("studio.query.retry")}
          </Button>
        </div>
      </div>
    );
  }
  if (empty !== undefined && isEmptyData(query.data)) return <>{empty}</>;
  return <>{children}</>;
}
