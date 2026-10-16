import { LayoutDetailScreen } from "@/components/layout/LayoutDetailScreen";

// /[entity]/[id] — dynamic DETAIL view for ANY entity, rendered from the layout's detail view.
// Relation fields are expanded to { id, label } so the UI shows names instead of UUIDs (RM-07).
// Layout Engine L2 T6. Next 16: params is a Promise → await it.
export default async function RecordDetailPage({
  params,
}: {
  params: Promise<{ entity: string; id: string }>;
}) {
  const { entity, id } = await params;
  return <LayoutDetailScreen entity={entity} recordId={id} />;
}
