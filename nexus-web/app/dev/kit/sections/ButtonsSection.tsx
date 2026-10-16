import clsx from "clsx";
import { Briefcase, Building2, ChartColumn, ChevronDown, Coins, Download, Filter, Heart, Plus, Search, Settings, TriangleAlert } from "lucide-react";
import { useState, type ReactNode } from "react";
import { Avatar } from "@/components/slds/Avatar";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { ButtonGroup } from "@/components/slds/ButtonGroup";
import { ButtonIcon } from "@/components/slds/ButtonIcon";
import { Pill, PillContainer } from "@/components/slds/Pill";
import { ProgressBar } from "@/components/slds/ProgressBar";
import { Spinner } from "@/components/slds/Spinner";

// Blueprints: buttons/base + with-icon, button-icons/*, button-groups/*, spinners/base,
// progress-bar/base + vertical, badges/base, avatar/base + initials, pills/base +
// listbox-of-pill-options (SLDS 2.264.1). Static sample data; one block per
// component with its own id so it can be screenshotted against loop/campaign/refs/shots.
function Block({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <div id={id} className="slds-m-bottom_large">
      <p className="slds-text-title_caps slds-m-bottom_x-small">{title}</p>
      {children}
    </div>
  );
}

function Row({ children, inverse }: { children: ReactNode; inverse?: boolean }) {
  // Inverse samples sit on the blueprint's dark demo band (slds-theme_inverse = same #16325c family).
  return (
    <div className={clsx("flex flex-wrap items-center gap-2 slds-m-bottom_small", inverse && "slds-theme_inverse slds-p-around_x-small")}>
      {children}
    </div>
  );
}

// The spinner is absolutely centred on its nearest positioned ancestor (blueprint demo boxes are
// `position:relative; height:6rem`).
function SpinnerBox({ children, inverse, caption }: { children: ReactNode; inverse?: boolean; caption: string }) {
  return (
    <div className="flex flex-col items-center gap-1">
      <div className={clsx("relative h-24 w-32 slds-box slds-box_x-small", inverse && "slds-theme_inverse")}>{children}</div>
      <span className="slds-text-body_small">{caption}</span>
    </div>
  );
}

const account = { glyph: Building2, name: "account", label: "Khách hàng" };
const opportunity = { glyph: Briefcase, name: "opportunity", label: "Cơ hội" };

// Listbox of pill options with real removal, so the keyboard rules (Left/Right, Delete) can be tried.
function PillListboxDemo() {
  const [items, setItems] = useState(["Hà Nội", "Đà Nẵng", "TP. Hồ Chí Minh", "Cần Thơ"]);
  return (
    <PillContainer variant="listbox">
      {items.map((x) => (
        <Pill key={x} label={x} onRemove={() => setItems((s) => s.filter((y) => y !== x))} />
      ))}
    </PillContainer>
  );
}

export function ButtonsSection() {
  return (
    <div>
      <Block id="kit-button" title="Button (buttons/base, with-icon)">
        <Row>
          <Button variant="base">Cơ bản</Button>
          <Button>Trung tính</Button>
          <Button variant="brand">Lưu</Button>
          <Button variant="outline-brand">Viền thương hiệu</Button>
          <Button variant="destructive">Xoá</Button>
          <Button variant="text-destructive">Xoá (chữ)</Button>
          <Button variant="success">Hoàn tất</Button>
        </Row>
        <Row inverse>
          <Button variant="inverse">Nút nền tối</Button>
        </Row>
        <Row>
          <Button iconLeft={Download}>Tải về</Button>
          <Button iconRight={Download}>Tải về</Button>
          <Button variant="brand" iconLeft={Plus}>
            Mới
          </Button>
          <Button disabled>Trung tính</Button>
          <Button variant="brand" disabled>
            Lưu
          </Button>
          <Button disabled iconLeft={Download}>
            Tải về
          </Button>
        </Row>
        <Row>
          <Button stretch>Nút giãn hết chiều ngang</Button>
        </Row>
      </Block>

      <Block id="kit-button-icon" title="ButtonIcon (button-icons/*)">
        <Row>
          <ButtonIcon icon={Settings} label="Cài đặt" />
          <ButtonIcon icon={Settings} label="Cài đặt" disabled />
          <ButtonIcon icon={TriangleAlert} label="Lỗi" state="error" />
          <ButtonIcon icon={TriangleAlert} label="Cảnh báo" state="warning" />
          <ButtonIcon icon={Search} label="Tìm" variant="container" />
          <ButtonIcon icon={Search} label="Tìm" variant="border" />
          <ButtonIcon icon={Search} label="Tìm" variant="border-filled" />
          <ButtonIcon icon={Search} label="Tìm" variant="border-filled" disabled />
          <ButtonIcon icon={Search} label="Tìm" variant="brand" />
          <ButtonIcon icon={Settings} label="Thêm tuỳ chọn" variant="border-filled" more />
          <ButtonIcon icon={Settings} label="Thêm tuỳ chọn" variant="border" more />
          <ButtonIcon icon={Settings} label="Thêm tuỳ chọn" variant="container" more />
        </Row>
        <Row>
          <ButtonIcon icon={Heart} label="Thích" variant="border-filled" selected={false} />
          <ButtonIcon icon={Heart} label="Thích" variant="border-filled" selected />
          <ButtonIcon icon={Heart} label="Thích" variant="border-filled" selected disabled />
          <span className="slds-hint-parent">
            <ButtonIcon icon={Settings} label="Thêm tuỳ chọn" variant="border-filled" hint />
          </span>
          <ButtonIcon icon={Search} label="Tìm" variant="border" size="large" />
          <ButtonIcon icon={Search} label="Tìm" variant="border" size="small" />
          <ButtonIcon icon={Search} label="Tìm" variant="border" size="x-small" />
          <ButtonIcon icon={Search} label="Tìm" variant="border" size="xx-small" />
          <ButtonIcon icon={Search} label="Tìm" iconSize="large" />
        </Row>
        <Row inverse>
          <ButtonIcon icon={Search} label="Tìm" variant="inverse" />
          <ButtonIcon icon={Search} label="Tìm" variant="border-inverse" />
          <ButtonIcon icon={Settings} label="Thêm tuỳ chọn" variant="inverse" more />
          <ButtonIcon icon={Search} label="Tìm" variant="inverse" disabled />
        </Row>
      </Block>

      <Block id="kit-button-group" title="ButtonGroup (button-groups/base, list, row)">
        <Row>
          <ButtonGroup label="Thao tác bản ghi">
            <Button>Làm mới</Button>
            <Button>Sửa</Button>
            <Button>Lưu</Button>
          </ButtonGroup>
          <ButtonGroup>
            <Button>Làm mới</Button>
            <Button disabled>Sửa</Button>
            <Button>Lưu</Button>
          </ButtonGroup>
          <ButtonGroup overflow={<ButtonIcon icon={ChevronDown} label="Thêm thao tác" variant="border-filled" aria-haspopup="true" />}>
            <Button>Làm mới</Button>
            <Button>Sửa</Button>
            <Button>Lưu</Button>
          </ButtonGroup>
          <ButtonGroup overflow={<ButtonIcon icon={ChevronDown} label="Thêm thao tác" variant="brand" aria-haspopup="true" />}>
            <Button variant="brand">Lưu</Button>
          </ButtonGroup>
          <ButtonGroup label="Kiểu xem">
            <ButtonIcon icon={ChartColumn} label="Biểu đồ" variant="border-filled" selected />
            <ButtonIcon icon={Filter} label="Lọc danh sách" variant="border-filled" selected={false} />
            <ButtonIcon icon={Settings} label="Thêm thao tác" variant="border-filled" more />
          </ButtonGroup>
        </Row>
        <Row inverse>
          <ButtonGroup overflow={<ButtonIcon icon={ChevronDown} label="Thêm thao tác" variant="border-inverse" aria-haspopup="true" />}>
            <Button variant="inverse">Làm mới</Button>
            <Button variant="inverse">Sửa</Button>
            <Button variant="inverse">Lưu</Button>
          </ButtonGroup>
        </Row>
        <Row>
          <ButtonGroup variant="list">
            <Button>Làm mới</Button>
            <Button>Sửa</Button>
            <Button>Lưu</Button>
          </ButtonGroup>
          <ButtonGroup variant="row">
            <Button>Làm mới</Button>
            <Button>Sửa</Button>
            <Button variant="brand">Lưu</Button>
          </ButtonGroup>
        </Row>
      </Block>

      <Block id="kit-spinner" title="Spinner (spinners/base)">
        <Row>
          <SpinnerBox caption="xx-small">
            <Spinner size="xx-small" />
          </SpinnerBox>
          <SpinnerBox caption="x-small">
            <Spinner size="x-small" />
          </SpinnerBox>
          <SpinnerBox caption="small">
            <Spinner size="small" />
          </SpinnerBox>
          <SpinnerBox caption="medium">
            <Spinner />
          </SpinnerBox>
          <SpinnerBox caption="large">
            <Spinner size="large" />
          </SpinnerBox>
          <SpinnerBox caption="brand">
            <Spinner variant="brand" />
          </SpinnerBox>
          <SpinnerBox caption="container" inverse>
            <Spinner container />
          </SpinnerBox>
          <SpinnerBox caption="inverse" inverse>
            <Spinner variant="inverse" />
          </SpinnerBox>
        </Row>
        <Row>
          <Spinner size="small" inline />
          <span>Đang tải danh sách… (inline)</span>
        </Row>
      </Block>

      <Block id="kit-progress-bar" title="ProgressBar (progress-bar/base, vertical)">
        <div className="grid grid-cols-2 gap-4">
          <div className="flex flex-col gap-3">
            <ProgressBar value={25} label="Tiến độ nhập dữ liệu" />
            <ProgressBar value={25} title="Trợ lý thiết lập" />
            <ProgressBar value={75} circular />
            <ProgressBar value={100} success />
          </div>
          <div className="flex flex-col gap-3">
            <ProgressBar value={75} size="x-small" />
            <ProgressBar value={75} size="small" />
            <ProgressBar value={75} size="medium" />
            <ProgressBar value={75} size="large" />
          </div>
        </div>
        <div className="flex gap-6 h-24 slds-m-top_small">
          <ProgressBar value={0} vertical />
          <ProgressBar value={25} vertical />
          <ProgressBar value={50} vertical />
          <ProgressBar value={75} vertical />
          <ProgressBar value={100} vertical />
        </div>
      </Block>

      <Block id="kit-badge" title="Badge (badges/base)">
        <Row>
          <Badge>Nhãn</Badge>
          <Badge variant="inverse">Nhãn nền tối</Badge>
          <Badge variant="lightest">Nhãn nền trắng</Badge>
          <Badge iconLeft={Coins}>423 tín dụng</Badge>
          <Badge iconRight={Coins}>423 tín dụng</Badge>
          <Badge variant="lightest" iconLeft={Coins}>
            423 tín dụng
          </Badge>
          <Badge variant="inverse" iconLeft={Coins}>
            423 tín dụng
          </Badge>
          <Badge variant="inverse" iconRight={Coins}>
            423 tín dụng
          </Badge>
          <Badge>
            Thành phần: <em>Lightning</em>
          </Badge>
        </Row>
        <Row>
          <Badge theme="success">Đã duyệt</Badge>
          <Badge theme="warning">Chờ duyệt</Badge>
          <Badge theme="error">Bị từ chối</Badge>
        </Row>
      </Block>

      <Block id="kit-avatar" title="Avatar (avatar/base, initials)">
        <Row>
          {/* Image samples use a square asset of our own — never SLDS assets/images (CC BY-ND). */}
          <Avatar name="Ảnh đại diện" src="/globe.svg" />
          <Avatar name="Ảnh đại diện" src="/globe.svg" shape="circle" />
          <Avatar name="Đỗ Xuân Tài" shape="circle" />
          <Avatar name="Đỗ Xuân Tài" shape="circle" inverse />
          <Avatar name="Acme" />
          <Avatar name="Công ty Acme" icon={Building2} />
          <Avatar name="Nguyễn Văn An" shape="circle" colorName="contact" />
          <Avatar name="" shape="circle" />
        </Row>
        <Row>
          <Avatar name="Trần Thị Bích" shape="circle" size="x-small" />
          <Avatar name="Trần Thị Bích" shape="circle" size="small" />
          <Avatar name="Trần Thị Bích" shape="circle" size="medium" />
          <Avatar name="Trần Thị Bích" shape="circle" size="large" />
          <Avatar name="Ảnh đại diện" src="/globe.svg" shape="circle" size="x-small" />
          <Avatar name="Ảnh đại diện" src="/globe.svg" shape="circle" size="large" />
          <Avatar name="Công ty Acme" icon={Building2} size="large" />
        </Row>
      </Block>

      <Block id="kit-pill" title="Pill (pills/base, listbox-of-pill-options)">
        <Row>
          <Pill label="Nhãn thẻ" href="#" onRemove={() => {}} />
          <Pill label="Công ty Acme" href="#" icon={account} onRemove={() => {}} />
          <Pill label="Đỗ Xuân Tài" href="#" avatar={{ name: "Đỗ Xuân Tài" }} onRemove={() => {}} />
          <Pill label="Email sai định dạng" href="#" error onRemove={() => {}} />
          <div className="w-56">
            <PillContainer>
              <Pill label="Nhãn thẻ dài hơn vùng chứa nên bị cắt bớt" href="#" onRemove={() => {}} />
            </PillContainer>
          </div>
        </Row>
        <div className="slds-m-bottom_small">
          <PillContainer>
            <Pill label="Nhãn thẻ" href="#" onRemove={() => {}} />
            <Pill label="Nhãn thẻ" href="#" onRemove={() => {}} />
            <Pill label="Nhãn thẻ" href="#" onRemove={() => {}} />
          </PillContainer>
        </div>
        <div className="flex flex-col gap-2">
          <PillListboxDemo />
          <PillContainer variant="listbox">
            <Pill label="Công ty Acme" icon={account} onRemove={() => {}} />
            <Pill label="Hợp đồng 2026" icon={opportunity} onRemove={() => {}} />
          </PillContainer>
          <PillContainer variant="listbox">
            <Pill label="Đỗ Xuân Tài" avatar={{ name: "Đỗ Xuân Tài" }} onRemove={() => {}} />
            <Pill label="Trần Thị Bích" avatar={{ name: "Trần Thị Bích" }} onRemove={() => {}} />
          </PillContainer>
          <PillContainer variant="listbox">
            <Pill label="Nhãn thẻ" bare onRemove={() => {}} />
            <Pill label="Nhãn thẻ" bare onRemove={() => {}} />
          </PillContainer>
        </div>
      </Block>
    </div>
  );
}
