import { notFound } from "next/navigation";

// /dev/kit is an internal design-review gallery (static sample data). It must not ship as a
// reachable route on production hosts — env-gate it here (server component, zero client cost).
export default function DevKitLayout({ children }: { children: React.ReactNode }) {
  if (process.env.NODE_ENV === "production") notFound();
  return children;
}
