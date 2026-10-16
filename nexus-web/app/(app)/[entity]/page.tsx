import { LayoutListScreen } from "@/components/layout/LayoutListScreen";

// /[entity] — dynamic LIST for ANY entity, rendered from the layout's list view (columns/sort).
// Layout Engine L2 T6: replaces the old mock-backed EntityList. The previous screen is preserved
// as a preview at /[entity]/preview (components/screens/EntityList.tsx is untouched).
// Next 16: params is a Promise → await it.
export default async function EntityListPage({
  params,
}: {
  params: Promise<{ entity: string }>;
}) {
  const { entity } = await params;
  return <LayoutListScreen entity={entity} />;
}
