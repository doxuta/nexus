import { LayoutFormScreen } from "@/components/layout/LayoutFormScreen";

// /[entity]/new — dynamic CREATE form for ANY entity, rendered from the layout's edit view.
// Next 16: params is a Promise → await it.
export default async function NewRecordPage({
  params,
}: {
  params: Promise<{ entity: string }>;
}) {
  const { entity } = await params;
  return <LayoutFormScreen entity={entity} />;
}
