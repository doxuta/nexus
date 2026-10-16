// Breadcrumb + "lên cấp" helpers shared by the SLDS shells (AppShell's crumb strip, the Studio's
// builder header). The Topbar component that used to live here is gone: the customer face moved to
// GlobalHeader (specs/038 T017) and the Studio to ShellGlobalHeader (T038).

// Breadcrumb item — href vắng mặt = cấp hiện tại (không click được). W0-QW7: từng cấp là link sống.
export interface Crumb {
  label: string;
  href?: string;
}

// Parent của route hiện tại — nút back đi "lên" thay vì "lùi lịch sử".
// /accounts/123/edit → /accounts/123 · /admin/users → /admin · /studio/model → /studio
// Cấp 1 (route gốc mỗi face): về root của face đó.
export function parentPath(pathname: string): string {
  const segs = pathname.split("/").filter(Boolean);
  if (segs.length <= 1) return segs[0] === "studio" ? "/studio" : "/home";
  return "/" + segs.slice(0, -1).join("/");
}
