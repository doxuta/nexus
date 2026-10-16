// Lịch làm việc — business-hours calendars admin (CALENDAR-1, api-conventions §17).
// Gate RequireAuth → RequireAdmin nằm ở app/admin/layout.tsx (dùng chung cho mọi /admin/*).
import { BusinessCalendars } from "@/components/screens/admin/BusinessCalendars";

export default function AdminBusinessCalendarsPage() {
  return <BusinessCalendars />;
}
