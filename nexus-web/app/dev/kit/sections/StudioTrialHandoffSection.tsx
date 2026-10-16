"use client";

// /dev/kit "Chạy thử": SampleDataByTable on STATIC data (specs/077 T042). The query cache is pre-seeded and the
// identity answer says "not this project", so the fetchers never run and the kit makes no request.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";
import { SampleDataByTable } from "@/components/studio/trial/SampleDataByTable";

const ENTITIES = [
  { id: "e1", name: "leads", label: "Khách tiềm năng" },
  { id: "e2", name: "contacts", label: "Liên hệ" },
  { id: "e3", name: "deals", label: "Cơ hội" },
];
const MIXED = {
  total: 10,
  entities: [
    { entity: "leads", label: "Khách tiềm năng", count: 8, live: 6, inBin: 2, bySource: { ai: 3, generator: 5, pack: 0 } },
    { entity: "contacts", label: "Liên hệ", count: 2, live: 2, inBin: 0, bySource: { ai: 0, generator: 0, pack: 2 } },
  ],
};

function seeded(slug: string, summary: unknown) {
  const c = new QueryClient();
  c.setQueryData(["studio", "identity"], { isPlatformOperator: true });
  c.setQueryData(["project", slug, "entities"], ENTITIES);
  c.setQueryData(["project", slug, "studio", "sampleData"], summary);
  return c;
}

export function StudioTrialHandoffSection() {
  const [many] = useState(() => seeded("kit-nhieu-nguon", MIXED));
  const [none] = useState(() => seeded("kit-chua-co", { total: 0, entities: [] }));
  return (
    <div>
      <p className="slds-text-title_caps slds-m-bottom_x-small">Nhiều nguồn, có dòng trong thùng rác, bảng 0 mẫu</p>
      <QueryClientProvider client={many}>
        <SampleDataByTable slug="kit-nhieu-nguon" />
      </QueryClientProvider>
      <p className="slds-text-title_caps slds-m-top_large slds-m-bottom_x-small">Chưa có dòng mẫu nào</p>
      <QueryClientProvider client={none}>
        <SampleDataByTable slug="kit-chua-co" />
      </QueryClientProvider>
    </div>
  );
}
