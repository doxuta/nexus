// Ledger of every create/edit/confirm dialog a Studio builder still opens (specs/073 T092, FR-034, D-UI-27).
// Generated from `grep -rnE "<(Modal|Prompt|Dialog|ConfirmDialog)\b"` over the folders the gate scans (2026-10-09),
// NOT from the seven items of UI-ARCH 5.7 (those are a subset). One row per dialog:
//   [file under nexus-web, label, kind, owner slice]
// studio-no-create-modal.test.tsx closes the ledger: the number of dialogs grep finds in a file must EQUAL the rows
// of that file, so a new dialog is red, and a removed dialog must also remove its row (rows only ever go away).
// `owner`: F2 = khu Dựng, F3 = trình sửa màn/sơ đồ, F4 = trợ lý, F5 = Chạy thử/Giao, F6 = khu Vận hành,
// kh7 = the old screen is deleted.
export type DialogKind = "tạo" | "sửa" | "xác nhận";
export type DialogRow = readonly [file: string, label: string, kind: DialogKind, owner: string];

export const DIALOG_SCAN_DIRS = ["components/studio", "components/screens/admin"];
export const DIALOG_SCAN_FILES = ["components/apps/CreateAppDialog.tsx"];
export const DIALOG_RE = /<(?:Modal|Prompt|Dialog|ConfirmDialog)\b/g;

const S = "components/studio/";
const A = "components/screens/admin/";

export const DIALOG_LEDGER: readonly DialogRow[] = [
  [`${S}HandoverWizard.tsx`, "Bàn giao cho khách", "tạo", "kh7"],
  [`${S}PackUpgradePanel.tsx`, "Áp nâng cấp gói", "xác nhận", "F5"],
  [`${A}EmailLog.tsx`, "Gửi lại thư của luật", "xác nhận", "F6"],
  [`${S}TenantRestoreDialog.tsx`, "Kiểm tra khôi phục", "xác nhận", "F6"],
  [`${S}workspaceShared.tsx`, "Xác nhận bằng cách gõ lại mã", "xác nhận", "F2"],
  [`${S}VerticalLibrary.tsx`, "Cài mẫu ngành", "tạo", "kh7"],
  [`${S}VerticalLibrary.tsx`, "Sao lưu trước khi cài", "xác nhận", "kh7"],
  [`${S}VerticalLibrary.tsx`, "Nâng cấp gói từ tệp", "xác nhận", "F5"],
  [`${S}PicklistDependency.tsx`, "Phụ thuộc danh sách chọn", "sửa", "F2"],
  [`${S}DraftBar.tsx`, "Lần đầu có thay đổi nháp", "xác nhận", "F5"],
  [`${S}PublishReview.tsx`, "Xuất bản dưới 768px (Modal theo UI-ARCH 5.3)", "xác nhận", "F5"],
  [`${A}RolesList.tsx`, "Bỏ thay đổi chưa lưu", "xác nhận", "F6"], // RolesList is the customer face (/admin/roles) only since f2-access-matrix
  [`${A}RoleEditor.tsx`, "Xoá vai trò", "xác nhận", "F6"], // Studio role page confirms in the page (inlineDelete); only /admin/roles keeps this Prompt
  [`${A}RolesList.tsx`, "Tạo vai trò", "tạo", "F6"],
  [`${A}SsoProviders.tsx`, "Tin cậy nhà cung cấp đăng nhập", "xác nhận", "F6"],
  [`${A}SsoProviders.tsx`, "Thêm nhà cung cấp đăng nhập", "tạo", "F6"],
  [`${A}UserSessionsCard.tsx`, "Kết thúc phiên", "xác nhận", "F6"],
  [`${A}UserSessionsCard.tsx`, "Xác nhận kết thúc mọi phiên", "xác nhận", "F6"],
  ["components/apps/CreateAppDialog.tsx", "Tạo hoặc sửa app", "tạo", "F2"],
  [`${A}TeamsList.tsx`, "Tạo nhóm", "tạo", "F6"],
  [`${A}TeamsList.tsx`, "Xoá nhóm", "xác nhận", "F6"],
  [`${A}UserList.tsx`, "Mời người dùng", "tạo", "F6"],
  [`${A}UserList.tsx`, "Thêm người dùng", "tạo", "F6"],
  [`${A}UserList.tsx`, "Thêm người dùng (chủ khách)", "tạo", "F6"],
  [`${A}UserList.tsx`, "Xoá người dùng", "xác nhận", "F6"],
  // One Pane: a Modal on the customer face (/admin/credentials) and a section in the page inside the Studio frame (u7-khoa-ket-noi).
  [`${A}NamedCredentials.tsx`, "Tạo, sửa, đổi khoá, nhập khoá, khoá đang dùng (chỉ mặt khách)", "tạo", "F6"],
];
