import { notFound } from "next/navigation";

// /dev/embedded mounts ONE real builder inside <EmbeddedSurface> in a page-scrolling container, the way the
// Studio frame will (specs/073 T099). Internal measuring page for scripts/embedded-builders-measure.mjs:
// never a route on production hosts.
export default function DevEmbeddedLayout({ children }: { children: React.ReactNode }) {
  if (process.env.NODE_ENV === "production") notFound();
  return children;
}
