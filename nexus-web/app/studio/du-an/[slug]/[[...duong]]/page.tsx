import { Suspense } from "react";
import { ProjectPage } from "@/components/studio/ProjectPage";

// /studio/du-an/<slug>/<duong…> — which page shows is decided in components/studio/ProjectPage from the path
// (studioRoutes is the single table). `duong` empty redirects to the first page there.
export default async function ProjectRoutePage({ params }: { params: Promise<{ slug: string; duong?: string[] }> }) {
  const { slug } = await params;
  return (
    <Suspense fallback={null}>
      <ProjectPage slug={slug} />
    </Suspense>
  );
}
