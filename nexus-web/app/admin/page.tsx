// /admin — redirect to /admin/users
import { redirect } from "next/navigation";

export default function AdminPage() {
  redirect("/admin/users");
}
