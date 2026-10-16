import { describe, it, expect } from "vitest";
import { ENGINE_ACTORS, isMetaEntity, metaNoun, metaVerb, systemActorLabel } from "@/lib/auditLabels";

// specs/062 L4 (T065, FR-023): rules, reports, dashboards, branding, apps, inbound hooks and the payment
// account now write audit rows under reserved keys. The viewer must name every one in Vietnamese -
// never the raw token, never the engine fallback "cấu hình hệ thống".
describe("auditLabels - L4 meta keys", () => {
  const keys: Record<string, string> = {
    __workflow_rule__: "quy tắc tự động",
    __report__: "báo cáo",
    __dashboard__: "dashboard",
    __branding__: "thương hiệu",
    __app__: "app",
    __inbound_hook__: "Webhook vào",
    __payment_config__: "Tài khoản nhận tiền",
  };

  it.each(Object.entries(keys))("%s reads %s", (key, noun) => {
    expect(isMetaEntity(key)).toBe(true);
    expect(metaNoun(key)).toBe(noun);
    expect(systemActorLabel(key)).toBe(`Hệ thống · ${noun}`);
    expect(metaNoun(key)).not.toContain("__");
    expect(metaNoun(key)).not.toBe("cấu hình hệ thống");
  });

  // specs/064 S9-7 (T115): the wipe's summary row reads "Xoá dữ liệu mẫu", the generator's "Sinh dữ liệu mẫu".
  it("names the sample-data rows and their verbs, never the raw token", () => {
    expect(isMetaEntity("__sample_data__")).toBe(true);
    expect(metaNoun("__sample_data__")).toBe("dữ liệu mẫu");
    expect(metaVerb("__sample_data__", "delete")).toBe("Xoá dữ liệu mẫu");
    expect(metaVerb("__sample_data__", "create")).toBe("Sinh dữ liệu mẫu");
    expect(metaVerb("__sample_data__", "update")).toBeUndefined();
  });

  it("labels the system actor of a builder-made inbound hook", () => {
    // internal/inboundhook SystemActorID (D-22): records a hook writes show "Webhook vào", never a builder's name.
    expect(ENGINE_ACTORS["f2000000-0000-0000-0000-000000000000"]).toBe("Webhook vào");
    // its siblings are untouched
    expect(ENGINE_ACTORS["f1000000-0000-0000-0000-000000000000"]).toBe("Quy tắc tự động");
    expect(ENGINE_ACTORS["f3000000-0000-0000-0000-000000000000"]).toBe("Tự sao lưu theo lịch");
  });
});
