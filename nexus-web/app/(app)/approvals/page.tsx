// Approvals Inbox — Screen 1 + 2 (8-additional-screens.md)
// GET /approvals/pending verified live; PascalCase envelope.
// Decision overlay inline (approve/reject with comment).
// `?view=pool` is where a work-queue card with the "claim" verb lands (workqueue_service.queueHref):
// it opens the "Chờ nhận" tab, not the default one.
import { ApprovalsInbox } from "@/components/screens/ApprovalsInbox";
import type { WorkItemView } from "@/lib/api";

const VIEWS: readonly WorkItemView[] = ["mine", "pool", "done"];

export default async function ApprovalsPage({ searchParams }: { searchParams: Promise<{ view?: string }> }) {
  const { view } = await searchParams;
  return <ApprovalsInbox initialView={VIEWS.find((v) => v === view)} />;
}
