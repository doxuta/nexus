"use client";

// Feedback & content family (specs/038 §5.4): ExpandableSection, Accordion, Alert,
// ScopedNotification, EmptyState, DockedFormFooter, ProgressIndicator, ActivityTimeline. Static sample data
// only; each block names the blueprint/variant it copies and carries an id so it can be
// screenshotted on its own.
import { CalendarDays, CalendarX2, ChevronDown, Globe, ListTodo, Mail, Paperclip, Phone, Repeat, SearchX, UserRound, Users } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Accordion, type AccordionSection } from "@/components/slds/Accordion";
import { ActivityTimeline, type ActivityTimelineItem } from "@/components/slds/ActivityTimeline";
import { Alert, type AlertVariant } from "@/components/slds/Alert";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { DockedFormFooter } from "@/components/slds/DockedFormFooter";
import { EmptyState } from "@/components/slds/EmptyState";
import { ExpandableSection } from "@/components/slds/ExpandableSection";
import { Menu, MenuItem } from "@/components/slds/Menu";
import { ProgressIndicator } from "@/components/slds/ProgressIndicator";
import { ScopedNotification, type ScopedNotificationTheme } from "@/components/slds/ScopedNotification";

function Demo({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <div id={id} className="slds-m-bottom_large">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

const ADDRESS = "Tầng 12, toà nhà Minh Phát, 88 Láng Hạ, Đống Đa, Hà Nội. Giao hàng giờ hành chính, gọi trước 30 phút.";

const FAQ: AccordionSection[] = [
  { id: "billing", summary: "Chu kỳ xuất hoá đơn", content: "Hoá đơn xuất vào ngày 1 hằng tháng, hạn thanh toán 15 ngày." },
  { id: "support", summary: "Kênh hỗ trợ khách hàng", content: "Tổng đài 1900 6868 (8:00–17:30), email hotro@minhphat.vn." },
  { id: "contract", summary: "Điều khoản gia hạn hợp đồng", content: "Hợp đồng tự gia hạn 12 tháng nếu không có thông báo trước 30 ngày." },
];

const moreActions = () => (
  <Menu align="right" trigger={<ButtonIcon icon={ChevronDown} label="Hiện thêm" variant="border-filled" size="x-small" />}>
    <MenuItem label="Sửa" />
    <MenuItem label="Sao chép" />
    <MenuItem label="Xoá" />
  </Menu>
);
const FAQ_WITH_ACTIONS: AccordionSection[] = FAQ.map((s) => ({ ...s, actions: moreActions() }));

const ALERTS: { variant: AlertVariant; text: string; link: string }[] = [
  { variant: "warning", text: "Trình duyệt của bạn đã cũ, một số chức năng có thể chạy chậm.", link: "Xem hướng dẫn" },
  { variant: "error", text: "Không kết nối được máy chủ đồng bộ. Dữ liệu mới nhất chưa được tải.", link: "Thử lại" },
  { variant: "offline", text: "Bạn đang ở chế độ ngoại tuyến.", link: "Tìm hiểu thêm" },
];

function AlertDemo() {
  const [shown, setShown] = useState(true);
  return (
    <Demo id="kit-alert" title="Alert — alert/base (mặc định · cảnh báo · lỗi · ngoại tuyến)">
      <div className="grid gap-2">
        {shown ? (
          <Alert icon={UserRound} iconName="user" onClose={() => setShown(false)}>
            Đang đăng nhập với tài khoản Nguyễn Văn An (an.nguyen@minhphat.vn). <a href="#">Đăng xuất</a>
          </Alert>
        ) : (
          <button type="button" className="slds-button slds-button_neutral" onClick={() => setShown(true)}>
            Hiện lại thông báo
          </button>
        )}
        {ALERTS.map((a) => (
          <Alert key={a.variant} variant={a.variant} onClose={() => {}}>
            {a.text} <a href="#">{a.link}</a>
          </Alert>
        ))}
      </div>
    </Demo>
  );
}

const SCOPED: { theme: ScopedNotificationTheme; text: string }[] = [
  { theme: "light", text: "Có vẻ khách hàng này đã tồn tại trong hệ thống." },
  { theme: "dark", text: "Có vẻ khách hàng này đã tồn tại trong hệ thống." },
  { theme: "info", text: "Bản ghi đang được người khác chỉnh sửa." },
  { theme: "success", text: "Đã gộp 2 bản ghi trùng." },
  { theme: "warning", text: "Bản ghi đã bị thay đổi sau khi bạn mở. Tải lại để xem bản mới nhất." },
  { theme: "error", text: "Không lưu được: bản ghi đã bị người khác xoá." },
];

const ONBOARDING = ["Thông tin công ty", "Người liên hệ", "Xác minh email", "Hợp đồng", "Kích hoạt"].map((label, i) => ({ id: `step-${i}`, label }));

const itemActions = () => (
  <Menu align="right" trigger={<ButtonIcon icon={ChevronDown} label="Thêm thao tác cho mục này" variant="border-filled" size="x-small" />}>
    <MenuItem label="Sửa" />
    <MenuItem label="Xoá" />
  </Menu>
);

const An = <a href="#">Nguyễn Văn An</a>;
const You = <a href="#">Bạn</a>;

function TimelineDemo() {
  const [done, setDone] = useState(false);
  const items: ActivityTimelineItem[] = [
    {
      id: "task",
      type: "task",
      icon: ListTodo,
      title: "Gửi báo giá gói triển khai cho Minh Phát và xin xác nhận của phòng mua hàng",
      href: "#",
      date: "09:00 | 20/09/2026",
      summary: (
        <>
          {You} đã tạo công việc với {An}
        </>
      ),
      meta: [{ icon: Repeat, label: "Công việc lặp lại" }],
      task: { checked: done, onChange: setDone },
      actions: itemActions(),
      fields: [
        { label: "Người liên hệ", value: <a href="#">Nguyễn Văn An</a> },
        { label: "Liên quan tới", value: <a href="#">Hợp đồng triển khai 2026</a> },
      ],
      body: { label: "Mô tả", text: "Chốt đơn giá và phạm vi trước cuộc họp tuần sau." },
    },
    {
      id: "call",
      type: "call",
      icon: Phone,
      title: "Gọi xác nhận lịch họp",
      href: "#",
      date: "10:00 | 23/09/2026",
      summary: (
        <>
          {You} đã ghi cuộc gọi với <a href="#">Trần Thị Bình</a>
        </>
      ),
      actions: itemActions(),
      error: "Không tải được chi tiết cuộc gọi",
    },
    {
      id: "email",
      type: "email",
      icon: Mail,
      title: "Re: Báo giá gói triển khai",
      href: "#",
      date: "10:30 | 24/09/2026",
      summary: (
        <>
          {You} đã gửi email cho {An}
        </>
      ),
      meta: [
        { icon: Users, label: "Email nhóm" },
        { icon: Paperclip, label: "Có tệp đính kèm" },
      ],
      actions: itemActions(),
      fields: [
        { label: "Người gửi", value: <a href="#">Lê Hoàng Cường</a> },
        { label: "Người nhận", value: <a href="#">Nguyễn Văn An</a> },
      ],
      body: { label: "Nội dung", text: "Chào anh An, em gửi lại báo giá đã cập nhật theo góp ý hôm qua." },
    },
    {
      id: "event",
      type: "event",
      icon: CalendarDays,
      title: "Họp chốt hợp đồng",
      href: "#",
      date: "14:00 | 26/09/2026",
      summary: (
        <>
          {You} đã tạo sự kiện với {An} và 3 người khác
        </>
      ),
      meta: [{ icon: Globe, label: "Chia sẻ công khai" }],
      actions: itemActions(),
      fields: [
        { label: "Địa điểm", value: "88 Láng Hạ, Đống Đa, Hà Nội" },
        { label: "Thời gian", value: "26/09, 14:00 – 15:30" },
      ],
    },
  ];
  return (
    <Demo id="kit-activity-timeline" title="ActivityTimeline — activity-timeline/base (đóng · mở · lỗi)">
      <ActivityTimeline items={items} defaultExpanded={["task", "call"]} />
    </Demo>
  );
}

// .slds-docked-form-footer is position:fixed; will-change:transform makes each frame its containing
// block so the sample docks to the frame, not to the kit's viewport. The errors only appear after Lưu,
// as in a real form, so the popover opening by itself doesn't grab focus when the kit loads.
function DockedFormFooterDemo() {
  const [failed, setFailed] = useState(false);
  return (
    <Demo id="kit-docked-form-footer" title="DockedFormFooter — docked-form-footer/base (mặc định · có lỗi: bấm Lưu, popover lỗi tự mở)">
      <div className="grid gap-4">
        <div className="slds-box relative h-24 will-change-transform">
          <p>Biểu mẫu khách hàng…</p>
          <DockedFormFooter onCancel={() => {}} onSave={() => {}} />
        </div>
        <div className="slds-box relative h-60 will-change-transform">
          <p>Biểu mẫu khách hàng…</p>
          <DockedFormFooter
            onCancel={() => setFailed(false)}
            onSave={() => setFailed(true)}
            errors={
              failed && (
                <p>
                  Email không đúng định dạng. Số điện thoại phải có 10 chữ số. <a href="#">Tìm hiểu thêm</a>
                </p>
              )
            }
          />
        </div>
      </div>
    </Demo>
  );
}

export function FeedbackContentSection() {
  return (
    <>
      <Demo id="kit-expandable-section" title="ExpandableSection — expandable-section/base (mở · đóng · không thu gọn)">
        <ExpandableSection title="Thông tin địa chỉ">
          <p>{ADDRESS}</p>
        </ExpandableSection>
        <ExpandableSection title="Thông tin hệ thống" defaultOpen={false}>
          <p>Người tạo: Nguyễn Văn An · Ngày tạo: 12/09/2026.</p>
        </ExpandableSection>
        <ExpandableSection title="Tóm tắt" collapsible={false}>
          <p>{ADDRESS}</p>
        </ExpandableSection>
      </Demo>

      <Demo id="kit-accordion" title="Accordion — accordion/base (mặc định: mở một mục)">
        <Accordion sections={FAQ} />
      </Demo>

      <Demo id="kit-accordion-actions" title="Accordion — accordion/base has-action-overflow (mở nhiều mục · menu thao tác)">
        <Accordion sections={FAQ_WITH_ACTIONS} multiple defaultOpen={["billing", "contract"]} />
      </Demo>

      <AlertDemo />

      <Demo id="kit-scoped-notification" title="ScopedNotification — scoped-notifications/base (sáng · tối · info · success · warning · error)">
        <div className="grid gap-2">
          {SCOPED.map((n) => (
            <ScopedNotification key={n.theme} theme={n.theme}>
              {n.text} <a href="#">Xem chi tiết</a>
            </ScopedNotification>
          ))}
        </div>
      </Demo>

      <Demo id="kit-empty-state" title="EmptyState — illustration/base (mặc định · chỉ tiêu đề · có hành động)">
        <div className="grid grid-cols-3 gap-4">
          <EmptyState title="Chưa có khách hàng nào" description="Khách hàng bạn tạo hoặc được chia sẻ sẽ hiện ở đây." />
          <EmptyState icon={CalendarX2} title="Không có sự kiện sắp tới" />
          <EmptyState
            icon={SearchX}
            title="Không tìm thấy kết quả"
            description="Thử bỏ bớt điều kiện lọc hoặc đổi từ khoá."
            action={
              <button type="button" className="slds-button slds-button_neutral">
                Xoá bộ lọc
              </button>
            }
          />
        </div>
      </Demo>

      <Demo id="kit-empty-state-plain" title="EmptyState plain — trang rỗng của khu Dựng (không hình, căn trái, một nút)">
        <EmptyState
          plain
          title="Chưa có bảng nào"
          description="Bảng là nơi khách lưu thông tin, ví dụ Khách hàng, Lịch hẹn, Hoá đơn."
          action={
            <button type="button" className="slds-button slds-button_brand">
              + Thêm bảng đầu tiên
            </button>
          }
        />
      </Demo>

      <DockedFormFooterDemo />

      <Demo id="kit-progress-indicator" title="ProgressIndicator — progress-indicator/base (bước hiện tại · lỗi · nền xám; rê/tab vào điểm để xem tên bước)">
        <div className="grid gap-4">
          <div className="pt-12" id="kit-progress-indicator-default">
            <ProgressIndicator steps={ONBOARDING} current={2} />
          </div>
          <ProgressIndicator steps={ONBOARDING} current={1} error />
          <div className="slds-theme_shade slds-p-around_medium">
            <ProgressIndicator steps={ONBOARDING} current={3} shade />
          </div>
        </div>
      </Demo>

      <Demo id="kit-progress-indicator-vertical" title="ProgressIndicator — progress-indicator/vertical (mặc định · dấu xanh lá · lỗi)">
        <div className="grid grid-cols-3 gap-4">
          <ProgressIndicator steps={ONBOARDING} current={2} vertical />
          <ProgressIndicator steps={ONBOARDING} current={3} vertical success />
          <ProgressIndicator steps={ONBOARDING} current={1} vertical error />
        </div>
      </Demo>

      <TimelineDemo />
    </>
  );
}
