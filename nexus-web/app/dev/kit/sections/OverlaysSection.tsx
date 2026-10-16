"use client";

import { ChevronDown, Columns3, Download, Info, Kanban, List, Pencil, Share2, Star, Table, Trash2, UserRound } from "lucide-react";
import { useState } from "react";
import { Menu, MenuCheckboxItem, MenuDivider, MenuGroup, MenuItem } from "@/components/slds/Menu";
import { Modal, ModalClose, Prompt } from "@/components/slds/Modal";
import { Panel } from "@/components/slds/Panel";
import { Popover } from "@/components/slds/Popover";
import { Toast, toast } from "@/components/slds/Toast";
import { Tooltip } from "@/components/slds/Tooltip";

// Blueprints: modals/base, prompt/base, panels/base, toast/base, menus/dropdown, popovers/base, tooltips/base (SLDS 2.264.1). Static sample data; each block names its variant.
function Demo({ id, title, children }: { id: string; title: string; children: React.ReactNode }) {
  return (
    <div id={id} className="slds-m-bottom_large">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

function PanelDemo() {
  const [open, setOpen] = useState(true);
  return (
    <Demo id="kit-panel" title="Panel — panels/base (docked trái · drilled-in phải)">
      <button type="button" className="slds-button slds-button_neutral slds-m-bottom_x-small" aria-expanded={open} aria-controls="kit-panel-left" onClick={() => setOpen((o) => !o)}>
        {open ? "Ẩn bảng lọc" : "Hiện bảng lọc"}
      </button>
      <div className="slds-box slds-p-around_none flex h-80 overflow-hidden">
        <Panel id="kit-panel-left" open={open} title="Bộ lọc" onClose={() => setOpen(false)}>
          Bảng bên trái nhận mọi bố cục hoặc component.
        </Panel>
        <div className="slds-col slds-p-around_medium">Nội dung chính của trang.</div>
        <Panel open position="right" title="Chi tiết điều kiện lọc rất dài để thử cắt chữ" onBack={() => {}} onClose={() => {}}>
          Trạng thái drilled-in, neo bên phải.
        </Panel>
      </div>
    </Demo>
  );
}

// specs/073 u0-be-mat: docked beside the content from 1280px, overlay with backdrop + focus trap below.
function ResponsivePanelDemo() {
  const [open, setOpen] = useState(false);
  return (
    <Demo id="kit-panel-responsive" title="Panel responsive (gắn cạnh phải từ 1280px · phủ có lớp mờ dưới đó)">
      <button type="button" className="slds-button slds-button_neutral slds-m-bottom_x-small" aria-expanded={open} aria-controls="kit-panel-resp" onClick={() => setOpen((o) => !o)}>
        {open ? "Ẩn chi tiết" : "Hiện chi tiết"}
      </button>
      <div className="slds-box slds-p-around_none flex h-80 overflow-hidden">
        <div className="slds-col slds-p-around_medium">Nội dung chính của trang.</div>
        <Panel id="kit-panel-resp" responsive position="right" open={open} title="Chi tiết" onClose={() => setOpen(false)}>
          <input className="slds-input" aria-label="Ghi chú" placeholder="Ghi chú" />
        </Panel>
      </div>
    </Demo>
  );
}

const noop = () => {};

const LOREM =
  "Hợp đồng được gia hạn tự động mỗi năm trừ khi một bên thông báo chấm dứt trước 30 ngày. Mọi thay đổi về giá phải được hai bên xác nhận bằng văn bản và có hiệu lực từ kỳ thanh toán kế tiếp.";

function ModalFooter({ primary = "Lưu" }: { primary?: string }) {
  return (
    <>
      <ModalClose asChild>
        <button type="button" className="slds-button slds-button_neutral">
          Huỷ
        </button>
      </ModalClose>
      <button type="button" className="slds-button slds-button_brand">
        {primary}
      </button>
    </>
  );
}

const openButton = (label: string) => (
  <button type="button" className="slds-button slds-button_neutral">
    {label}
  </button>
);

function ModalDemo() {
  return (
    <Demo id="kit-modal" title="Modal — modals/base (mặc định có tagline · nhỏ · lớn · directional · headless) · Prompt — prompt/base">
      <div className="flex flex-wrap gap-2">
        <Modal
          trigger={openButton("Modal mặc định")}
          title="Khách hàng mới"
          tagline={
            <>
              Trường có dấu * là bắt buộc. <a href="#">Xem hướng dẫn</a>.
            </>
          }
          footer={<ModalFooter />}
        >
          <p>{LOREM}</p>
          <p>{LOREM}</p>
        </Modal>
        <Modal trigger={openButton("Modal nhỏ")} size="small" title="Đổi chủ sở hữu" footer={<ModalFooter primary="Đổi" />}>
          <p>{LOREM}</p>
        </Modal>
        <Modal trigger={openButton("Modal lớn")} size="large" title="Nhập dữ liệu khách hàng" footer={<ModalFooter primary="Nhập" />}>
          <p>{LOREM}</p>
          <p>{LOREM}</p>
          <p>{LOREM}</p>
        </Modal>
        <Modal
          trigger={openButton("Modal directional")}
          title="Bước 1 / 3 — Chọn đối tượng"
          directionalFooter
          footer={
            <>
              <button type="button" className="slds-button slds-button_neutral">
                Bỏ qua bước này
              </button>
              <button type="button" className="slds-button slds-button_brand">
                Lưu &amp; tiếp
              </button>
            </>
          }
        >
          <p>{LOREM}</p>
        </Modal>
        <Modal trigger={openButton("Modal headless")} label="Xem trước hợp đồng">
          <p>{LOREM}</p>
        </Modal>
        <Prompt trigger={openButton("Prompt lỗi")} theme="error" title="Dịch vụ tạm ngưng">
          Máy chủ đang bảo trì định kỳ. Dữ liệu của bạn vẫn an toàn; vui lòng thử lại sau 15 phút.
        </Prompt>
        <Prompt trigger={openButton("Prompt cảnh báo")} theme="warning" title="Gói dịch vụ sắp hết hạn">
          Gói dịch vụ của tổ chức còn 3 ngày. Liên hệ quản trị viên để gia hạn.
        </Prompt>
        <Prompt trigger={openButton("Prompt thông tin")} theme="info" title="Có phiên bản mới">
          Nexus vừa cập nhật. Tải lại trang để dùng phiên bản mới nhất.
        </Prompt>
      </div>
    </Demo>
  );
}

function ToastDemo() {
  return (
    <Demo id="kit-toast" title="Toast — toast/base (info · success · warning · error · error có chi tiết)">
      <div className="flex flex-col gap-2">
        <div className="slds-notify_container slds-is-relative">
          <Toast title="Tìm thấy 26 khách hàng tiềm năng trùng." action={{ label: "Chọn để gộp", onClick: noop }} onClose={noop} />
        </div>
        <div className="slds-notify_container slds-is-relative">
          <Toast variant="success" title="Đã tạo khách hàng “Công ty ACME”." onClose={noop} />
        </div>
        <div className="slds-notify_container slds-is-relative">
          <Toast variant="warning" title="Không chia sẻ được tệp “bao-cao-q3.pdf” với người đã chọn." onClose={noop} />
        </div>
        <div className="slds-notify_container slds-is-relative">
          <Toast variant="error" title="Không lưu được “Nguyễn Văn An” vì đã có bản ghi trùng tên." onClose={noop} />
        </div>
        <div className="slds-notify_container slds-is-relative">
          <Toast
            variant="error"
            title="Có lỗi khi lưu thay đổi cho “Trần Thị Bình”."
            message="Trường Email không hợp lệ và trường Điện thoại bắt buộc."
            onClose={noop}
          />
        </div>
      </div>
      <div className="mt-3 flex gap-2">
        <button type="button" className="slds-button slds-button_neutral" onClick={() => toast.success("Đã lưu bản ghi.")}>
          Bắn toast thành công
        </button>
        <button type="button" className="slds-button slds-button_neutral" onClick={() => toast({ variant: "error", title: "Không kết nối được máy chủ.", message: "Thử lại sau ít phút." })}>
          Bắn toast lỗi
        </button>
        <button type="button" className="slds-button slds-button_neutral" onClick={() => toast.withAction("Đã xoá 3 bản ghi.", { label: "Hoàn tác", onClick: noop })}>
          Bắn toast có hành động
        </button>
      </div>
    </Demo>
  );
}

const menuTrigger = (
  <button type="button" className="slds-button slds-button_icon slds-button_icon-border-filled" title="Thêm thao tác">
    <ChevronDown className="slds-button__icon nx-lucide" aria-hidden="true" />
    <span className="slds-assistive-text">Thêm thao tác</span>
  </button>
);

function ViewMenu() {
  const [view, setView] = useState("table");
  const views = [
    { id: "table", label: "Dạng bảng", icon: Table },
    { id: "kanban", label: "Dạng Kanban", icon: Kanban },
    { id: "list", label: "Dạng danh sách", icon: List },
  ];
  return (
    <Menu trigger={menuTrigger} size="small">
      {views.map((v) => (
        <MenuCheckboxItem key={v.id} label={v.label} iconRight={v.icon} checked={view === v.id} onCheckedChange={() => setView(v.id)} />
      ))}
    </Menu>
  );
}

// Menus open on click (Radix, one at a time); each cell reserves room for its open dropdown.
function MenuDemo() {
  return (
    <Demo id="kit-menu" title="Menu — menus/dropdown (mặc định · tiêu đề nhóm · icon trái + mục tắt · chọn + icon phải · cảnh báo · canh phải)">
      <div className="flex h-72 flex-wrap items-start gap-x-40">
        <div id="kit-menu-default">
          <Menu trigger={menuTrigger}>
            <MenuItem label="Sửa" />
            <MenuItem label="Sao chép" />
            <MenuItem label="Đổi chủ sở hữu" disabled />
            <MenuDivider />
            <MenuItem label="Xoá" />
          </Menu>
        </div>
        <div id="kit-menu-header">
          <Menu trigger={menuTrigger} size="small">
            <MenuGroup label="Bản ghi">
              <MenuItem label="Sửa" />
              <MenuItem label="Sao chép" />
            </MenuGroup>
            <MenuGroup label="Chia sẻ">
              <MenuItem label="Gửi email" />
              <MenuItem label="Sao chép liên kết" />
            </MenuGroup>
          </Menu>
        </div>
        <div id="kit-menu-icon-left">
          <Menu trigger={menuTrigger}>
            <MenuItem label="Sửa" icon={Pencil} />
            <MenuItem label="Chia sẻ" icon={Share2} />
            <MenuItem label="Chọn cột" icon={Columns3} />
            <MenuItem label="Xuất tệp" icon={Download} disabled />
          </Menu>
        </div>
        <div id="kit-menu-double">
          <ViewMenu />
        </div>
        <div id="kit-menu-alert">
          <Menu trigger={menuTrigger}>
            <MenuItem label="Đồng bộ xong" tone="success" />
            <MenuItem label="Hạn mức sắp hết" tone="warning" />
            <MenuItem label="Người phụ trách" icon={UserRound} />
            <MenuDivider />
            <MenuItem label="Xoá bản ghi" tone="error" />
            <MenuItem label="Xoá vĩnh viễn" icon={Trash2} />
          </Menu>
        </div>
        <div id="kit-menu-right" className="ml-auto">
          <Menu trigger={menuTrigger} align="right">
            <MenuItem label="Sửa" />
            <MenuItem label="Sao chép" />
            <MenuItem label="Xoá" />
          </Menu>
        </div>
      </div>
    </Demo>
  );
}

const SHORT = "Khách hàng này có 3 hợp đồng đang hiệu lực và 1 yêu cầu hỗ trợ chưa đóng.";

// Popovers open on click; each cell reserves room for its open popover (Radix places it).
function PopoverDemo() {
  return (
    <Demo id="kit-popover" title="Popover — popovers/base (mặc định · tiêu đề + icon + footer · badge · tối · cuộn, không nút đóng)">
      <div className="flex min-h-80 flex-wrap items-start gap-x-4">
        <div id="kit-popover-default">
          <Popover trigger={openButton("Mặc định")} label="Tóm tắt khách hàng">
            <p>{SHORT}</p>
          </Popover>
        </div>
        <div id="kit-popover-footer">
          <Popover
            trigger={openButton("Tiêu đề + icon + footer")}
            side="bottom"
            align="start"
            title="Khách hàng quan trọng"
            icon={Star}
            iconLabel="Được đánh dấu"
            footer={
              <div className="slds-grid slds-grid_vertical-align-center">
                <button type="button" className="slds-button slds-button_neutral slds-col_bump-left">
                  Xem hồ sơ
                </button>
              </div>
            }
          >
            <p>{SHORT}</p>
          </Popover>
        </div>
        <div id="kit-popover-badge">
          <Popover trigger={openButton("Badge")} side="bottom" align="start" size="large" title="Bảo trì hệ thống" badge="Thông báo quản trị">
            <p>Hệ thống sẽ bảo trì từ 22:00 đến 23:00 tối nay. Dữ liệu nhập trong thời gian này có thể bị chậm đồng bộ.</p>
          </Popover>
        </div>
        <div id="kit-popover-dark">
          <Popover trigger={openButton("Tối")} dark title="Mẹo" icon={Star}>
            <p>Nhấn giữ Shift khi kéo để chọn nhiều bản ghi.</p>
          </Popover>
        </div>
        <div id="kit-popover-scroll">
          <Popover trigger={openButton("Cuộn · không nút đóng")} side="bottom" align="start" title="Điều khoản" closeButton={false} scrollable footer={<p>Cập nhật 09/2026</p>}>
            <div className="slds-text-longform">
              <p>{LOREM}</p>
              <p>{LOREM}</p>
              <p>{LOREM}</p>
            </div>
          </Popover>
        </div>
      </div>
    </Demo>
  );
}

const TIP = "Mã số thuế gồm 10 chữ số, hoặc 13 chữ số với chi nhánh.";

// Tooltips open on hover or keyboard focus (Radix, one at a time); the row leaves room above.
function TooltipDemo() {
  return (
    <Demo id="kit-tooltip" title="Tooltip — tooltips/base (nút chữ · icon link · nút icon · nút · dưới · phải)">
      <div className="flex flex-wrap items-center gap-x-24 pt-24 pb-24">
        <span id="kit-tooltip-text">
          <Tooltip trigger={<button type="button" className="slds-button">Mã số thuế</button>}>{TIP}</Tooltip>
        </span>
        <span id="kit-tooltip-link">
          <Tooltip
            trigger={
              <a href="#">
                <span className="slds-icon_container slds-icon-utility-info">
                  <Info className="slds-icon slds-icon_xx-small slds-icon-text-default nx-lucide" aria-hidden="true" />
                  <span className="slds-assistive-text">Tìm hiểu thêm</span>
                </span>
              </a>
            }
          >
            {TIP}
          </Tooltip>
        </span>
        <span id="kit-tooltip-icon">
          <Tooltip
            trigger={
              <button type="button" className="slds-button slds-button_icon" title="Trợ giúp">
                <Info className="slds-button__icon nx-lucide" aria-hidden="true" />
                <span className="slds-assistive-text">Trợ giúp</span>
              </button>
            }
          >
            {TIP}
          </Tooltip>
        </span>
        <span id="kit-tooltip-button">
          <Tooltip trigger={openButton("Nút")}>{TIP}</Tooltip>
        </span>
        <span id="kit-tooltip-bottom">
          <Tooltip trigger={openButton("Dưới · giữa")} side="bottom" align="center">
            {TIP}
          </Tooltip>
        </span>
        <span id="kit-tooltip-right">
          <Tooltip trigger={openButton("Phải")} side="right" align="center">
            {TIP}
          </Tooltip>
        </span>
      </div>
    </Demo>
  );
}

export function OverlaysSection() {
  return (
    <div>
      <ModalDemo />
      <MenuDemo />
      <PopoverDemo />
      <TooltipDemo />
      <ToastDemo />
      <PanelDemo />
      <ResponsivePanelDemo />
    </div>
  );
}
