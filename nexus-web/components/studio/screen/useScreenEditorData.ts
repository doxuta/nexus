"use client";

// Everything the form screen editor reads for one table (specs/075 T019): its fields, the layout the server serves,
// and the first records (the stage draws the real screen with the first; the list with up to five). No record is not an error: record = null and the
// stage says so (nothing invented). One query so loading/error is one state.
import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { listRecords, type ApiRecord, type EntityLayout, type FieldMeta, type LayoutView } from "@/lib/api";
import { readForBuilder } from "@/lib/readForBuilder";

export interface ScreenData {
  fields: FieldMeta[];
  layout: EntityLayout | null;
  record: ApiRecord | null;
  /** First records of the table (the list stage draws up to five). */
  records: ApiRecord[];
}

// `delivered` is in the key: flipping it (publish state arrives / first delivery) re-reads over the right layer.
export const screenKey = (entity: string, delivered = false, live = false) => ["studio", "screen", entity, delivered, live] as const;

async function load(entity: string, delivered: boolean, live: boolean): Promise<ScreenData> {
  const [fields, layout, records] = await Promise.all([
    readForBuilder.listFields(delivered, entity, live),
    readForBuilder.getLayout(delivered, entity, live),
    // a record the builder may not read must not block the editor
    listRecords(entity, "limit=5").catch(() => [] as ApiRecord[]),
  ]);
  return { fields, layout, record: records[0] ?? null, records };
}

export function useScreenEditorData(entity: string, delivered = false, live = false): UseQueryResult<ScreenData> {
  return useQuery({ queryKey: screenKey(entity, delivered, live), queryFn: () => load(entity, delivered, live), staleTime: 0, gcTime: 0 });
}

/** The edit (or detail) view as served + whether it is the auto-generated one + the id to PUT/DELETE (absent when auto). */
export function editViewOf(
  layout: EntityLayout | null | undefined,
  which: "edit" | "detail" | "list" = "edit",
): { view: LayoutView | null; isDefault: boolean; layoutId: string | null } {
  const view = layout?.views?.[which] ?? null;
  const id = (view as { id?: string } | null)?.id ?? null;
  const empty = which === "list" ? !view?.columns?.length : !view?.sections?.length;
  const isDefault = !view || view.isDefault === true || empty;
  return { view, isDefault, layoutId: isDefault ? null : id };
}
