import { BatchCreateForm } from "@/components/layout/BatchCreateForm";

// /[entity]/batch — multi-row (bulk) create: a spreadsheet-style table, add/remove/duplicate
// rows, submit atomic|partial to POST /:entity/bulk (ENG-BATCH #1).
// Next 16: params is a Promise → await it.
export default async function BatchCreatePage({
  params,
}: {
  params: Promise<{ entity: string }>;
}) {
  const { entity } = await params;
  return <BatchCreateForm entity={entity} />;
}
