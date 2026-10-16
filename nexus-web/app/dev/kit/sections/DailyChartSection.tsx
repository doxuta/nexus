"use client";

// DailyChart (specs/078 u7-muc-dung-bieu-do): one series, then a rule-run series with its failures. Static sample data.
import { DailyChart } from "@/components/charts/DailyChart";

const DAYS = Array.from({ length: 30 }, (_, i) => {
  const d = new Date(Date.UTC(2026, 8, 15 + i));
  return { day: d.toISOString().slice(0, 10), i };
});
const showDay = (d: string) => d.split("-").reverse().join("/");
const users = DAYS.map(({ day, i }) => ({ day, values: { ok: i % 7 === 5 || i % 7 === 6 ? 0 : 3 + ((i * 5) % 6) } }));
const runs = DAYS.map(({ day, i }) => ({ day, values: { ok: 20 + ((i * 7) % 15), failed: i % 9 === 4 ? 3 : 0 } }));

export function DailyChartSection() {
  return (
    <div className="grid gap-8">
      <div>
        <p className="slds-text-title_caps slds-m-bottom_x-small">Một chuỗi</p>
        <DailyChart title="Người đăng nhập mỗi ngày" series={[{ key: "ok", label: "Người đăng nhập", tone: "normal" }]} points={users} formatDay={showDay} />
      </div>
      <div>
        <p className="slds-text-title_caps slds-m-bottom_x-small">Hai chuỗi, một trục, lỗi có icon và chữ</p>
        <DailyChart
          title="Lần chạy luật"
          series={[
            { key: "ok", label: "Thành công", tone: "normal" },
            { key: "failed", label: "Lỗi", tone: "error" },
          ]}
          points={runs}
          formatDay={showDay}
        />
      </div>
    </div>
  );
}
