import { Building2, Briefcase, Contact, Ticket, GraduationCap, Search, TriangleAlert, CircleCheck, CircleX } from "lucide-react";
import { Icon } from "@/components/slds/Icon";

// Blueprint: icons/standard, icons/utility (SLDS 2.264.1). Glyphs are Lucide — never the SLDS sprite.
export function IconSection() {
  return (
    <div className="slds-grid slds-wrap slds-gutters_small">
      <div className="slds-col slds-size_1-of-1 slds-m-bottom_small">
        <p className="slds-text-title_caps slds-m-bottom_x-small">Standard (ô màu theo loại đối tượng)</p>
        <div className="flex items-center gap-3">
          <Icon icon={Building2} category="standard" name="account" label="Khách hàng" />
          <Icon icon={Contact} category="standard" name="contact" label="Liên hệ" />
          <Icon icon={Briefcase} category="standard" name="opportunity" label="Cơ hội" />
          <Icon icon={Ticket} category="standard" name="case" label="Phiếu hỗ trợ" />
          <Icon icon={GraduationCap} category="standard" name="education" label="Khoá học" size="medium" />
          <Icon icon={Building2} category="standard" name="account" size="large" label="Khách hàng (lớn)" />
        </div>
      </div>
      <div className="slds-col slds-size_1-of-1">
        <p className="slds-text-title_caps slds-m-bottom_x-small">Utility (glyph không nền)</p>
        <div className="flex items-center gap-3">
          <Icon icon={Search} size="x-small" label="Tìm" />
          <Icon icon={TriangleAlert} size="x-small" tone="warning" label="Cảnh báo" />
          <Icon icon={CircleX} size="x-small" tone="error" label="Lỗi" />
          <Icon icon={CircleCheck} size="x-small" tone="success" label="Thành công" />
        </div>
      </div>
    </div>
  );
}
