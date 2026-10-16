import { LayoutFormScreen } from "@/components/layout/LayoutFormScreen";

// /[entity]/[id]/edit — dynamic EDIT form for ANY entity. Prefills from getRecord and PUTs with
// the record version (optimistic lock). Next 16: params is a Promise → await it.
export default async function EditRecordPage({
  params,
}: {
  params: Promise<{ entity: string; id: string }>;
}) {
  const { entity, id } = await params;
  return <LayoutFormScreen entity={entity} recordId={id} />;
}
