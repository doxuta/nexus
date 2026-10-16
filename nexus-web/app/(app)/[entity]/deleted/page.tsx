import { RecycleBinScreen } from "@/components/screens/RecycleBinScreen";

// /[entity]/deleted — recycle-bin (soft-deleted records + restore, W9c).
// Backend live: GET /:entity/deleted + POST /:entity/:id/restore (api-conventions §13.1).
// Next 16: params is a Promise → await it.
export default async function DeletedRecordsPage({
  params,
}: {
  params: Promise<{ entity: string }>;
}) {
  const { entity } = await params;
  return <RecycleBinScreen entity={entity} />;
}
