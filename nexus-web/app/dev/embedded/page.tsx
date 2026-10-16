"use client";

// /dev/embedded?b=<builder>&e=<entity> — one real builder inside EmbeddedSurface, in a block container whose
// height is its content (the page scrolls), which is what the Studio work surface will be. Measured by
// scripts/embedded-builders-measure.mjs; the numbers are recorded in specs/073-studio-shell-projects/quickstart.md.
import dynamic from "next/dynamic";
import { Suspense, type ComponentType, type ReactNode } from "react";
import { useSearchParams } from "next/navigation";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { RequireAuth } from "@/lib/auth/guards";

const lazy = (load: () => Promise<ComponentType<any>>) => dynamic(() => load().then((C) => ({ default: C })), { ssr: false }); // eslint-disable-line @typescript-eslint/no-explicit-any
const WorkflowBuilder = lazy(() => import("@/components/studio/WorkflowBuilder").then((m) => m.WorkflowBuilder));
const BrandingScreen = lazy(() => import("@/components/studio/BrandingScreen").then((m) => m.BrandingScreen));
const UserList = lazy(() => import("@/components/screens/admin/UserList").then((m) => m.UserList));
const AuditLog = lazy(() => import("@/components/screens/admin/AuditLog").then((m) => m.AuditLog));
const DoctorScreen = lazy(() => import("@/components/screens/admin/DoctorScreen").then((m) => m.DoctorScreen));
const TeamsList = lazy(() => import("@/components/screens/admin/TeamsList").then((m) => m.TeamsList));
const ImportScreen = lazy(() => import("@/components/screens/ImportScreen").then((m) => m.ImportScreen));
const EntityFieldEditor = lazy(() => import("@/components/studio/EntityFieldEditor").then((m) => m.EntityFieldEditor));
const LayoutBuilder = lazy(() => import("@/components/studio/LayoutBuilder").then((m) => m.LayoutBuilder));

const BUILDERS: Record<string, (entity: string) => ReactNode> = {
  WorkflowBuilder: () => <WorkflowBuilder />,
  BrandingScreen: () => <BrandingScreen />,
  UserList: () => <UserList />,
  AuditLog: () => <AuditLog />,
  DoctorScreen: () => <DoctorScreen />,
  TeamsList: () => <TeamsList />,
  ImportScreen: (e) => <ImportScreen entity={e} />,
  EntityFieldEditor: (e) => <EntityFieldEditor entityName={e} />,
  LayoutBuilder: (e) => <LayoutBuilder entityName={e} />,
};

function Probe() {
  const sp = useSearchParams();
  const id = sp.get("b") ?? "";
  const entity = sp.get("e") ?? "crm_accounts";
  const Builder = BUILDERS[id];
  if (!Builder) return <p>Trình dựng không có trong danh sách: {Object.keys(BUILDERS).join(", ")}</p>;
  return (
    <main id="nx-embedded-surface" className="slds-p-around_medium">
      <EmbeddedSurface>
        {Builder(entity)}
      </EmbeddedSurface>
    </main>
  );
}

export default function DevEmbeddedPage() {
  return (
    <RequireAuth>
      <Suspense fallback={null}>
        <Probe />
      </Suspense>
    </RequireAuth>
  );
}
