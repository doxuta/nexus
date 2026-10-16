import { Suspense, type ReactNode } from "react";
import { ProjectFrame } from "@/components/studio/ProjectFrame";

// /studio/du-an/<slug>/… — the project frame (specs/073). Thin on purpose: the vitest config scans lib/, components/ and
// test/ only, so the logic lives in components/studio/ProjectFrame. Next 16: `params` is a Promise. The frame and the
// ContextGate read the path and ?app= (useSearchParams), which needs a Suspense boundary.
export default async function ProjectLayout({ children, params }: { children: ReactNode; params: Promise<{ slug: string }> }) {
  const { slug } = await params;
  return (
    <Suspense fallback={null}>
      <ProjectFrame slug={slug}>{children}</ProjectFrame>
    </Suspense>
  );
}
