import { ImportScreen } from "@/components/screens/ImportScreen";

// /[entity]/import — CSV import wizard: upload → column mapping → preview → run.
// Next 16: params is a Promise → await it.
export default async function ImportPage({
  params,
}: {
  params: Promise<{ entity: string }>;
}) {
  const { entity } = await params;
  return <ImportScreen entity={entity} />;
}
