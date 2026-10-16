"use client";

// /dev/kit "Studio" (specs/073 FR-009, lát u0-nen-token): the Studio token layer on show. The
// hidden `.nx-face-studio` marker below turns the layer on for the WHOLE /dev/kit page while this
// section is mounted, unless the URL carries ?studio=off (the layer is `html:has(.nx-face-studio)`), which is the point: every other
// section can be eyeballed against the Studio look. Not linked from any nav; dev only.
//
// `.nx-parity-probe` is what scripts/studio-token-parity.mjs measures: the same template rendered
// inside `.nx-face-customer` (must equal the real customer route) and outside it (must differ).
import { Users } from "lucide-react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState, useSyncExternalStore } from "react";
import { Button } from "@/components/slds/Button";
import { Avatar } from "@/components/slds/Avatar";
import { Badge } from "@/components/slds/Badge";
import { Card } from "@/components/slds/Card";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { Prompt } from "@/components/slds/Modal";
import { PageHeader } from "@/components/slds/PageHeader";
import { Panel } from "@/components/slds/Panel";
import { Path } from "@/components/slds/Path";
import { Tabs } from "@/components/slds/Tabs";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { RecordFormPage } from "@/components/slds/templates/RecordFormPage";
import { SecretOnceBlock } from "@/components/studio/SecretOnceBlock";
import { CustomerPreviewFrame } from "@/components/studio/CustomerPreviewFrame";
import { StatusText, draftStateLabel, type StatusKind, type StatusState } from "@/components/studio/StatusText";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { PendingPage } from "@/components/studio/PendingPage";
import { ProjectHeader } from "@/components/studio/ProjectHeader";
import { QueryState } from "@/components/studio/QueryState";
import { StepHeader } from "@/components/studio/StepHeader";
import { AttentionBand } from "@/components/studio/AttentionBand";
import { BuildMeter } from "@/components/studio/BuildMeter";
import { StageBoard } from "@/components/studio/StageBoard";
import { ChangeList } from "@/components/studio/ChangeList";
import { PublishReview } from "@/components/studio/PublishReview";
import { brandScopeStyle } from "@/lib/branding";
import type { BuildStepState } from "@/lib/buildSteps";
import { attentionItems } from "@/lib/projectSignals";
import { SAMPLE_ROWS } from "./projectRows";

const STEP_KEYS: BuildStepState["key"][] = ["data", "screens", "automation", "access", "reports", "branding", "trial", "deliver"];
const meterSteps = (done: number): BuildStepState[] => STEP_KEYS.map((key, i) => ({ key, complete: i < done, optional: false, sublabel: "" }));

function Template() {
  return (
    <Card title="Khách hàng mẫu" padded boundary>
      <h2 className="slds-text-heading_medium slds-m-bottom_small">Thông tin chung</h2>
      <Tabs
        label="Hồ sơ"
        items={[
          { value: "a", label: "Chi tiết", content: "Thông tin chi tiết." },
          { value: "b", label: "Liên quan", content: "Liên hệ và phiếu hỗ trợ." },
        ]}
      />
      <div className="slds-m-vertical_small">
        <Input label="Tên khách hàng" defaultValue="Công ty Minh Phát" />
      </div>
      <Button variant="brand">Lưu</Button>
    </Card>
  );
}

// Fake field and user rows (dev kit only): the two lists the Studio mounts most, with the chips and
// initials tiles they draw in the customer face. Same tree twice, once flat inside EmbeddedSurface.
const FAKE_FIELDS = [
  { name: "Tên khách hàng", type: "Chữ", flags: [["Bắt buộc", "error"], ["Duy nhất", undefined]] },
  { name: "Số điện thoại", type: "Chữ", flags: [["Có chỉ mục", undefined]] },
  { name: "Ngày ký hợp đồng", type: "Ngày", flags: [] },
] as const;
const FAKE_USERS = [
  { name: "Nguyễn Văn An", status: "Đang dùng", theme: "success" },
  { name: "Trần Thị Bình", status: "Tạm khoá", theme: "warning" },
] as const;

function KitForm() {
  return (
    <RecordFormPage title="Khách hàng mới" sections={FAKE_FORM_SECTIONS} onSubmit={(e) => e.preventDefault()} onCancel={() => {}} />
  );
}

function EmbeddedSample() {
  return (
    <div className="flex min-w-0 flex-1 flex-col lg:flex-row">
      <div className="min-w-0 flex-1">
        <PageHeader title="Khách hàng" icon={{ glyph: Users, name: "account", label: "Khách hàng" }} meta="3 trường" actions={[<Button key="a" variant="brand">Thêm trường</Button>]} breadcrumbs={[{ label: "Dữ liệu", href: "#" }]} />
        <Card title="Trường" count={FAKE_FIELDS.length}>
          <ul>
            {FAKE_FIELDS.map((f) => (
              <li key={f.name} className="slds-grid slds-grid_vertical-align-center slds-p-vertical_x-small slds-border_bottom">
                <span className="flex-1">{f.name}</span>
                <span className="flex-1 slds-text-color_weak">{f.type}</span>
                <span className="flex-1 slds-grid slds-wrap">
                  {f.flags.map(([label, theme]) => (
                    <Badge key={label} theme={theme} className="slds-m-right_x-small">{label}</Badge>
                  ))}
                </span>
              </li>
            ))}
          </ul>
        </Card>
        <Card title="Người dùng">
          <ul>
            {FAKE_USERS.map((u) => (
              <li key={u.name} className="slds-grid slds-grid_vertical-align-center slds-p-vertical_x-small slds-border_bottom">
                <Avatar name={u.name} shape="circle" className="slds-m-right_small" />
                <span className="flex-1">{u.name}</span>
                <Badge theme={u.theme}>{u.status}</Badge>
              </li>
            ))}
          </ul>
        </Card>
        <Tabs label="Mục" items={[{ value: "a", label: "Quy tắc", content: <EmptyState title="Chưa có quy tắc" description="Quy tắc chạy khi bản ghi thay đổi." action={<Button variant="neutral">Thêm quy tắc</Button>} /> }, { value: "b", label: "Quyền", content: <p>Ai được xem và sửa bảng này.</p> }]} />
      </div>
      <Panel open title="Chi tiết trường" position="right">
        <p>Tên khách hàng</p>
      </Panel>
    </div>
  );
}

const SWATCH = [40, 50, 60, 70, 80, 90, 95];
const TONES = ["success", "error", "warn", "weak", "proposed"] as const;
const TONE_SAMPLE: Record<(typeof TONES)[number], string> = {
  success: "Đang dùng",
  error: "Lỗi",
  warn: "Bắt buộc sẽ bật khi xuất bản",
  weak: "Chỉ xem",
  proposed: "Đề xuất",
};
// Table 4.8 of the UI architecture, one row per state; the label comes from draftStateLabel, not from here.
const STATE_ROWS: [StatusState, StatusKind?][] = [
  ["edited"], ["new"], ["deleted"], ["requiredOnPublish"], ["live", "direct"], ["live", "untracked"],
  ["deleteFailed"], ["proposed"], ["inUse"], ["complete"], ["connecting"], ["locked"], ["error"], ["viewOnly"], ["hidden"],
];
// The four grounds the status colours must hold 4.5:1 on (measured by scripts/studio-token-parity.mjs).
const GROUNDS = [
  { id: "page", className: "", style: { background: "var(--slds-g-color-neutral-base-95)" }, label: "Nền trang" },
  { id: "card", className: "slds-theme_default", style: undefined, label: "Nền thẻ" },
  { id: "shade", className: "slds-theme_shade", style: undefined, label: "Nền dải" },
  { id: "selected", className: "", style: { background: "var(--slds-g-color-neutral-base-90)" }, label: "Nền dòng chọn" },
] as const;

// Fake form sections (dev kit only): the preview frame wraps the same presentational template /[entity]/new draws.
const FAKE_FORM_SECTIONS = [
  {
    id: "info",
    title: "Thông tin chung",
    body: (
      <div className="slds-grid slds-gutters slds-wrap">
        <div className="slds-col slds-size_1-of-1 slds-large-size_1-of-2"><Input label="Tên khách hàng" defaultValue="Công ty Minh Phát" /></div>
        <div className="slds-col slds-size_1-of-1 slds-large-size_1-of-2"><Input label="Số điện thoại" defaultValue="0901 234 567" /></div>
      </div>
    ),
  },
  { id: "more", title: "Liên hệ", body: <Input label="Địa chỉ" defaultValue="12 Nguyễn Huệ, Quận 1" /> },
];

// specs/073 kh1a: the project frame's parts, with the identity answer pre-seeded so the kit makes no request.
function FrameParts() {
  const [client] = useState(() => {
    const c = new QueryClient();
    c.setQueryData(["studio", "identity"], { isPlatformOperator: true });
    return c;
  });
  const [confirming, setConfirming] = useState(true);
  const noop = () => {};
  const loading = { isPending: true, isError: false, error: null, data: undefined, refetch: noop };
  const failed = { isPending: false, isError: true, error: new Error("Máy chủ chưa trả lời."), data: undefined, refetch: noop };
  const empty = { isPending: false, isError: false, error: null, data: [], refetch: noop };
  return (
    <QueryClientProvider client={client}>
      <p className="slds-text-title_caps slds-m-bottom_x-small">Khung dự án: dải đầu (xưởng Nexus, rồi dự án khách tên dài)</p>
      <div className="slds-m-bottom_medium">
        <ProjectHeader slug="northwind" identity={{ name: "Xưởng Nexus" }} zone="dung" home openApp={noop} />
        <ProjectHeader
          slug="acme"
          identity={{ name: "Phòng khám Nha khoa Sài Gòn Tâm Đức chi nhánh quận Bảy và trung tâm chỉnh nha", industry: "Nha khoa", stageLabel: "Đang dựng", originLabel: "Từ gói mẫu" }}
          zone="van-hanh"
        />
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Đầu bước: đủ, chưa đủ, chưa rõ</p>
      <div className="slds-m-bottom_medium">
        <StepHeader step={{ key: "data", complete: true }} />
        <StepHeader step={{ key: "access", complete: false }} actions={<Button variant="neutral">Thêm vai</Button>} />
        <StepHeader step={{ key: "reports", complete: null }} />
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Xác nhận tại chỗ (có nội dung thêm)</p>
      <div className="slds-m-bottom_medium">
        {confirming ? (
          <InlineConfirm tone="error" message="Ngưng dự án Phòng khám Minh Anh?" confirmLabel="Ngưng dự án" onConfirm={() => setConfirming(false)} onCancel={() => setConfirming(false)}>
            <Input label="Gõ lại mã dự án để xác nhận" defaultValue="" />
          </InlineConfirm>
        ) : (
          <Button variant="neutral" onClick={() => setConfirming(true)}>Hiện lại</Button>
        )}
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Trạng thái: đang tải, lỗi, rỗng</p>
      <div className="slds-m-bottom_medium">
        <QueryState query={loading}>x</QueryState>
        <QueryState query={failed}>x</QueryState>
        <QueryState query={empty} empty={<p className="nx-lede slds-p-around_medium">Chưa có bảng nào. Bấm Bảng mới để thêm.</p>}>x</QueryState>
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Sổ dự án: dải Cần anh xem, bảng giai đoạn, thanh tiến độ (dữ liệu mẫu, chỉ ở đây)</p>
      <div className="slds-m-bottom_medium">
        <AttentionBand items={attentionItems(SAMPLE_ROWS, new Date("2026-10-09T10:00:00Z"))} onOpenProject={noop} onAction={noop} />
        <StageBoard rows={SAMPLE_ROWS} query="" onOpen={noop} onOpenStep={noop} onTrial={noop} onIndustry={noop} onHealthCheck={noop} onSettings={noop} />
        <p className="slds-text-body_small slds-text-color_weak slds-m-vertical_x-small">Bảng khi cột rỗng và khi tìm không khớp</p>
        <StageBoard rows={SAMPLE_ROWS.slice(0, 2)} query="" onOpen={noop} onOpenStep={noop} onTrial={noop} onIndustry={noop} onHealthCheck={noop} onSettings={noop} />
        <div className="slds-m-top_small" />
        <StageBoard rows={SAMPLE_ROWS} query="zzz" onOpen={noop} onOpenStep={noop} onTrial={noop} onIndustry={noop} onHealthCheck={noop} onSettings={noop} />
        <p className="slds-text-body_small slds-text-color_weak slds-m-vertical_x-small">Thanh tiến độ: 0, 3 và 8 bước</p>
        <div className="flex flex-col gap-2" style={{ maxWidth: "16rem" }}>
          {[0, 3, 8].map((n) => (
            <BuildMeter key={n} steps={meterSteps(n)} />
          ))}
        </div>
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Xuất bản: danh sách thay đổi + − ~ = và bảng xem lại (đọc máy chủ thật, chưa đăng nhập thì báo lỗi tải)</p>
      <PublishParts />

      <p className="slds-text-title_caps slds-m-bottom_x-small">Trang chưa có trong bản này</p>
      <div className="slds-m-bottom_medium">
        <PendingPage feature="Cổng nhận dữ liệu" parent={{ label: "Tự động", href: "/dev/kit" }} />
        <PendingPage variant="inline" feature="Lịch làm việc" parent={{ label: "Tự động", href: "/dev/kit" }} />
      </div>
    </QueryClientProvider>
  );
}

function PublishParts() {
  const [open, setOpen] = useState(false);
  return (
    <div className="slds-m-bottom_medium">
      <div className="flex flex-wrap gap-6 slds-m-bottom_small">
        <ChangeList
          items={[
            { op: "add", text: "Luật gửi thư chào mừng" },
            { op: "change", text: "Báo cáo doanh thu tháng" },
            { op: "remove", text: "Bố cục cũ của Khách hàng" },
            { op: "same", text: "Thương hiệu" },
          ]}
        />
        <ChangeList tone="proposed" items={[{ op: "add", text: "Trường Ngày hẹn" }, { op: "change", text: "Form Lịch hẹn" }]} />
      </div>
      {open ? (
        <PublishReview anchor={<Button variant="brand">Xuất bản</Button>} onClose={() => setOpen(false)} />
      ) : (
        <Button variant="brand" onClick={() => setOpen(true)}>Xuất bản</Button>
      )}
    </div>
  );
}

// specs/078 u7-nen-nhung-mau-trang: the one-time secret block and the object home flat inside the work surface.
function OperateParts() {
  const [secret, setSecret] = useState(true);
  const [q, setQ] = useState("");
  return (
    <div className="slds-m-bottom_medium" data-testid="operate-parts">
      <p className="slds-text-title_caps slds-m-bottom_x-small">Khối bí mật hiện một lần (SecretOnceBlock)</p>
      <div className="slds-m-bottom_medium">
        {secret ? (
          <SecretOnceBlock
            rows={[{ label: "Email", value: "an@khach.vn" }, { label: "Mật khẩu", value: "Kx9-demo-only-0000" }]}
            onDone={() => setSecret(false)}
          >
            Mật khẩu này chỉ hiện một lần.
          </SecretOnceBlock>
        ) : (
          <Button onClick={() => setSecret(true)}>Hiện lại khối mẫu</Button>
        )}
      </div>
      <p className="slds-text-title_caps slds-m-bottom_x-small">Trang danh sách đối tượng, nhúng phẳng (ObjectHomePage)</p>
      <div className="slds-border_top slds-border_right slds-border_bottom slds-border_left slds-p-around_medium">
        <EmbeddedSurface>
          <ObjectHomePage
            icon={{ glyph: Users, name: "user" }}
            label="Người dùng"
            title="Tất cả người dùng"
            meta="3 người"
            search={{ value: q, onChange: setQ, placeholder: "Tìm người" }}
          >
            <p className="slds-p-around_medium slds-text-body_small">Bảng nằm ở đây.</p>
          </ObjectHomePage>
        </EmbeddedSurface>
      </div>
    </div>
  );
}

export function StudioSection() {
  // `?studio=off` leaves the layer off so the parity script has a same-template customer baseline.
  const on = useSyncExternalStore(
    () => () => {},
    () => new URLSearchParams(window.location.search).get("studio") !== "off",
    () => false,
  );
  return (
    <div>
      {on && <span className="nx-face-studio" hidden />}

      <p className="nx-lede slds-m-bottom_medium">
        Lớp token Studio: nhấn <b className="tabular-nums">electric-blue</b>, nút viên thuốc, chữ 14px, bo 8px.
      </p>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Bảng màu nhấn (brand 40 đến 95)</p>
      <div className="flex flex-wrap gap-2 slds-m-bottom_medium">
        {SWATCH.map((n) => (
          <div
            key={n}
            className="slds-text-body_small slds-p-around_x-small"
            style={{ background: `var(--slds-g-color-brand-base-${n})`, color: n >= 70 ? "var(--slds-g-color-neutral-base-10)" : "var(--slds-g-color-neutral-base-100)", minWidth: "3.5rem", textAlign: "center" }}
          >
            {n}
          </div>
        ))}
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Thang chữ</p>
      <div className="slds-m-bottom_medium">
        <h3 className="slds-text-heading_large">Tiêu đề lớn (700)</h3>
        <h3 className="slds-text-heading_medium">Tiêu đề vừa (600)</h3>
        <h3 className="slds-text-heading_small">Tiêu đề nhỏ (600)</h3>
        <p>Thân chữ 14px, số liệu <span className="tabular-nums">1.234.567</span>.</p>
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Nhãn trạng thái dạng chữ: năm tông</p>
      <ul className="flex flex-wrap gap-4 slds-m-bottom_medium">
        {TONES.map((k) => (
          <li key={k}><StatusText tone={k}>{TONE_SAMPLE[k]}</StatusText></li>
        ))}
      </ul>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Nhãn theo trạng thái (bảng 4.8), trên bốn nền</p>
      <div className="flex flex-col gap-2 slds-m-bottom_medium" data-testid="status-grounds">
        {GROUNDS.map((g) => (
          <div key={g.id} className={`${g.className} slds-p-around_small`} style={g.style} data-ground={g.id}>
            <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_xx-small">{g.label}</p>
            <ul className="flex flex-wrap gap-x-6 gap-y-1">
              {TONES.map((k) => (
                <li key={k}><StatusText tone={k}>{TONE_SAMPLE[k]}</StatusText></li>
              ))}
              {g.id === "page" && STATE_ROWS.map(([state, kind]) => {
                const l = draftStateLabel(state, kind);
                return l ? <li key={`${state}${kind ?? ""}`}><StatusText tone={l.tone}>{l.label}</StatusText></li> : null;
              })}
            </ul>
          </div>
        ))}
        <div data-ground="controls" className="slds-p-around_small" style={{ background: "var(--slds-g-color-neutral-base-95)" }}>
          <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_xx-small">Nút chính và liên kết trên nền trang</p>
          <Button variant="brand">Lưu</Button> <a href="#studio" className="slds-m-left_small">Xem lịch sử</a>
        </div>
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Khung xem màn khách: một form thật của bảng mẫu (màu khách đặt thì chỉ đổi trong khung)</p>
      <div className="flex flex-col gap-4 slds-m-bottom_medium" data-testid="preview-frames">
        <div data-frame="default">
          <CustomerPreviewFrame appName="Phòng khám Minh Anh" caption="Xem như khách thấy">
            <KitForm />
          </CustomerPreviewFrame>
        </div>
        <div data-frame="accent">
          <CustomerPreviewFrame accentColor="#0b7a5e" appName="Phòng khám Minh Anh" caption="Màu khách đã đặt">
            <KitForm />
          </CustomerPreviewFrame>
        </div>
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Chấm mức độ trên nền navy và khối đề xuất</p>
      <div className="flex flex-wrap gap-4 slds-m-bottom_medium">
        <div className="slds-theme_alt-inverse slds-p-around_small flex gap-3 items-center">
          {(["error", "warning", "info"] as const).map((k) => (
            <span key={k} className={`nx-dot nx-dot_${k}`} role="img" aria-label={k} />
          ))}
        </div>
        <div className="nx-proposed slds-p-around_small">Đề xuất của trợ lý</div>
        <Path
          mode="steps"
          label="Các bước dựng (mẫu)"
          current="screens"
          steps={[
            { key: "data", label: "Dữ liệu", complete: true, href: "#" },
            { key: "screens", label: "Màn hình", complete: false, href: "#" },
            { key: "access", label: "Quyền", complete: null, href: "#" },
          ]}
        />
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Prompt trong Studio (dùng chung dáng với mặt làm việc)</p>
      <div className="slds-m-bottom_medium">
        <Prompt trigger={<button type="button" id="parity-prompt-open" className="slds-button slds-button_neutral">Mở Prompt</button>} theme="info" title="Có phiên bản mới">
          Tải lại trang để dùng phiên bản mới nhất.
        </Prompt>
      </div>

      <p className="slds-text-title_caps slds-m-bottom_x-small">Trạng thái nhúng: ngoài (trái) và trong EmbeddedSurface (phải), cùng một cây</p>
      <div className="flex flex-col gap-6 slds-m-bottom_medium" data-testid="embedded-compare">
        <div>
          <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_x-small">Ngoài nhúng (mặt khách)</p>
          <div className="slds-border_top slds-border_right slds-border_bottom slds-border_left slds-p-around_medium">
            <EmbeddedSample />
          </div>
        </div>
        <div>
          <p className="slds-text-body_small slds-text-color_weak slds-m-bottom_x-small">Trong EmbeddedSurface (mặt làm việc Studio)</p>
          <div className="slds-border_top slds-border_right slds-border_bottom slds-border_left slds-p-around_medium">
            <EmbeddedSurface>
              <EmbeddedSample />
            </EmbeddedSurface>
          </div>
        </div>
      </div>

      <FrameParts />

      <OperateParts />

      <p className="slds-text-title_caps slds-m-bottom_x-small">Parity: cùng một template ở hai chỗ (script studio-token-parity đo)</p>
      <div className="nx-parity-probe slds-m-bottom_medium" data-where="studio">
        <Template />
      </div>
      <div className="nx-parity-probe slds-m-bottom_medium" data-where="customer">
        <div className="nx-face-customer" style={brandScopeStyle("#0176d3")}>
          <Template />
        </div>
      </div>
      <div className="nx-parity-probe" data-where="frame">
        <CustomerPreviewFrame appName="Phòng khám Minh Anh">
          <Template />
        </CustomerPreviewFrame>
      </div>
    </div>
  );
}
