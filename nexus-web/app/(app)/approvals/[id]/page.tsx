import { ApprovalDetail } from "@/components/screens/ApprovalDetail";

// /approvals/[id] — approval instance detail (W6 §3.12).
// GET /approvals/:id (api-conventions §9.5) — record link + process context + history timeline.
// Next 16: params is a Promise → await it.
export default async function ApprovalDetailPage({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await params;
  return <ApprovalDetail id={id} />;
}
