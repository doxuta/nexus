// lib/errorMessages.ts — Central backend error-code → Vietnamese UI message map.
// Source: docs/ux-spec/7-crosscut.md G.5c + api-conventions.md Error Codes catalog.
// Used by all API call catch blocks instead of raw ex.message.
// Pattern: t_error(ex) → human-readable Vietnamese string.

import { ApiError } from "@/lib/api";

// code → Vi message. Fallback to ex.message when code is absent or unrecognised.
const ERROR_MESSAGES: Record<string, string> = {
  // Auth / session
  INVALID_CREDENTIALS: "Thông tin đăng nhập không đúng.",
  UNAUTHORIZED: "Phiên đăng nhập hết hạn. Vui lòng đăng nhập lại.",
  INVALID_REFRESH: "Phiên đăng nhập hết hạn. Vui lòng đăng nhập lại.",
  FORBIDDEN: "Bạn không có quyền thực hiện thao tác này.",
  RATE_LIMITED: "Quá nhiều lần thử. Vui lòng thử lại sau.",
  NEED_LOGIN: "Vui lòng đăng nhập để tiếp tục.",
  // PASSWORD_TOO_WEAK (RM-17, 422) is DELIBERATELY unmapped: the server message names the
  // exact violated rule (min-length / all-whitespace / 72-byte bcrypt bound) in Vietnamese,
  // and errorMessage() falls back to it — a generic entry here would shadow that specificity.

  // Record validation (422)
  RECORD_REQUIRED_MISSING: "Thiếu thông tin bắt buộc.",
  // RECORD_REQUIRED_LOCKED (queue #62, 422) is DELIBERATELY unmapped, like PASSWORD_TOO_WEAK: the
  // server names the locked field and the admin's type-aware way out — a required field this role
  // may not write. "Tạo nhiều dòng" has no column for that field, so the name is the whole message.
  RECORD_UNKNOWN_FIELD: "Trường dữ liệu không hợp lệ.",
  RECORD_INVALID_TYPE: "Kiểu dữ liệu không đúng.",
  RECORD_INVALID_RANGE: "Giá trị nằm ngoài phạm vi cho phép.",
  RECORD_TOO_LONG: "Nội dung vượt quá độ dài tối đa.",
  RECORD_INVALID_PATTERN: "Nội dung không đúng định dạng yêu cầu.",
  RECORD_INVALID_PRECISION: "Số thập phân vượt quá độ chính xác cho phép.",
  RECORD_INVALID_PICKLIST: "Giá trị không nằm trong danh sách cho phép.",
  RECORD_RELATION_INVALID: "Bản ghi liên kết không hợp lệ hoặc không tồn tại.",
  RECORD_POLYMORPHIC_TARGET_INVALID: "Bản ghi liên kết đa hình không hợp lệ (ngoài danh sách entity cho phép, hoặc không tồn tại).",
  RECORD_INVALID_DATE: "Ngày/giờ không đúng định dạng.",
  RECORD_FORMULA_READONLY: "Trường công thức do hệ thống tự tính, không thể nhập.",
  RECORD_LOOKUP_READONLY: "Trường tra cứu do hệ thống tự tính, không thể nhập.",
  RECORD_ROLLUP_READONLY: "Trường tổng hợp do hệ thống tự tính, không thể nhập.",
  RECORD_AUTONUMBER_READONLY: "Trường số tự sinh do hệ thống tự gán, không thể nhập.",
  RECORD_RELATION_ALREADY_BOUND: "Bản ghi liên kết đã được gán cho một bản ghi khác (quan hệ 1-1).",
  FIELD_FORBIDDEN: "Bạn không có quyền ghi trường này.",
  // specs/048: the save rules could not be read, so the server refused rather than let an unchecked
  // write through (503 + Retry-After). RECORD_HOOK_VETO is DELIBERATELY unmapped —
  // ponytail: the rule's own sentence IS the message; lib/saveErrors reads it from the violations.
  RECORD_HOOKS_UNAVAILABLE: "Hệ thống bận, thử lại sau ít giây.",

  // Record conflict (409)
  RECORD_DUPLICATE: "Giá trị đã tồn tại, vi phạm ràng buộc duy nhất.",
  RECORD_VERSION_CONFLICT: "Bản ghi đã được sửa đổi bởi người khác. Vui lòng tải lại và thử lại.",
  CROSS_TENANT_SHARED_UNIQUE: "Giá trị này đã được sử dụng bởi tổ chức khác.",

  // Bulk create (ENG-BATCH #1)
  BULK_EMPTY: "Danh sách dòng đang trống — thêm ít nhất 1 dòng.",
  BULK_TOO_LARGE: "Vượt quá số dòng tối đa cho phép trong một lần gửi.",
  BULK_INVALID_MODE: "Chế độ gửi không hợp lệ (chỉ hỗ trợ toàn bộ-hoặc-không / từng dòng).",
  BULK_ATOMIC_ABORTED: "Dòng này hợp lệ nhưng bị huỷ theo vì có dòng khác trong lô bị lỗi.",
  BULK_ATOMIC_UNAVAILABLE: "Chế độ toàn bộ-hoặc-không hiện chưa khả dụng trên máy chủ.",

  // Preview as a role (specs/064 S9-1/S9-2)
  PREVIEW_READ_ONLY: "Đang xem thử — không ghi được thay đổi.",
  PREVIEW_FORBIDDEN: "Chức năng này không dùng được khi đang xem thử.",
  PREVIEW_ROLE_NOT_FOUND: "Không tìm thấy vai này trong không gian hiện tại.",
  PREVIEW_DRAFT_UNAVAILABLE: "Dự án chưa có bản nháp để xem.",
  PREVIEW_NOT_ACTIVE: "Không có phiên xem thử nào đang chạy.",
  PREVIEW_ENDED: "Phiên xem thử đã kết thúc.",

  // Builder workspace session (specs/062-customer-workspace). User-facing words: "không gian", never
  // "tenant" / "workspace session" (FR-019). TENANT_NOT_FOUND's server sentence says "workspace".
  WORKSPACE_SESSION_NOT_ALLOWED: "Về Nexus để làm việc này.",
  WORKSPACE_ENDED: "Phiên dựng đã kết thúc.",
  WORKSPACE_SESSION_ENDED: "Phiên dựng đã kết thúc.",
  WORKSPACE_SESSION_NOT_FOUND: "Không có phiên dựng nào đang mở.",
  WORKSPACE_IS_HOME: "Đây là không gian của chính bạn — không cần mở phiên dựng.",
  WORKSPACE_SLUG_REQUIRED: "Thiếu mã của khách.",
  TENANT_NOT_FOUND: "Không tìm thấy khách này.",

  // Customer owner role change on Giao cho khách (specs/077 f5-handoff-owner). ROLE_NOT_ASSIGNABLE comes
  // from the owner's own /org-admin route; PUT /admin/users/:id answers only ROLE_INVALID.
  ROLE_INVALID: "Vai này không còn trong dự án. Tải lại trang rồi chọn lại.",
  ROLE_NOT_ASSIGNABLE: "Vai này do người dựng giữ, không gán được.",

  // Users (specs/078 u7-nguoi-dung-danh-sach). PASSWORD_TOO_WEAK stays unmapped (see above).
  LAST_ADMIN: "Không khoá được quản trị viên cuối cùng.",
  SELF_DELETE_FORBIDDEN: "Bạn không xoá được chính mình.",
  USER_NOT_ACTIVE: "Mở khoá trước khi đặt lại mật khẩu.",
  USER_ALREADY_EXISTS: "Email này đã có người dùng.",
  USER_STATUS_INVALID: "Trạng thái này không hợp lệ.",
  USER_NOT_FOUND: "Người này đã bị xoá.",
  // Defensive: only a non-operator admin gets it (065 B2); the builder never does.
  USER_NOT_MANAGEABLE: "Bạn không quản lý được tài khoản này.",

  // Sample data (specs/064 S9-4)
  SAMPLE_ENTITY_NOT_FOUND: "Không tìm thấy bảng dữ liệu đã chọn.",
  SAMPLE_BUSY: "Đang sinh hoặc xoá dữ liệu mẫu, thử lại sau.",
  SAMPLE_PER_ENTITY_INVALID: "Số dòng mỗi bảng phải từ 1 đến 20.",
  SAMPLE_PACK_UNAVAILABLE: "Bảng này không có dữ liệu mẫu trong gói đã cài.",
  SAMPLE_TOO_MANY_ENTITIES: "Chọn tối đa 30 bảng mỗi lần.",

  // Field config (422)
  FIELD_TEXT_MAXLENGTH_INVALID: "Độ dài tối đa không hợp lệ.",
  FIELD_TEXT_PATTERN_INVALID: "Pattern regex không hợp lệ.",
  FIELD_NUMBER_RANGE_INVALID: "Khoảng giá trị số không hợp lệ.",
  FIELD_NUMBER_STEP_INVALID: "Bước nhảy số phải lớn hơn 0.",
  FIELD_CURRENCY_RANGE_INVALID: "Khoảng tiền tệ không hợp lệ.",
  FIELD_DEFAULT_NOT_SUPPORTED: "Kiểu trường này không hỗ trợ giá trị mặc định.",
  FIELD_RELATION_TARGET_REQUIRED: "Trường quan hệ cần chỉ định đối tượng liên kết.",
  FIELD_RELATION_TARGET_INVALID: "Tên đối tượng liên kết không hợp lệ.",
  FIELD_RELATION_TARGET_NOT_FOUND: "Đối tượng liên kết không tồn tại.",
  FIELD_ALREADY_EXISTS: "Trường đã tồn tại trong đối tượng này.",
  FIELD_TYPE_IMMUTABLE: "Không thể thay đổi kiểu trường sau khi tạo.",

  // Field config — lookup (ENG-COMPUTED #2, api-conventions §5.1)
  FIELD_LOOKUP_VIA_REQUIRED: "Trường tra cứu (lookup) cần chọn field quan hệ (via) làm cầu nối.",
  FIELD_LOOKUP_VIA_UNKNOWN: "Field quan hệ (via) đã chọn không tồn tại trên đối tượng này.",
  FIELD_LOOKUP_VIA_NOT_RELATION: "Field đã chọn cho \"via\" không phải kiểu Quan hệ.",
  FIELD_LOOKUP_VIA_MUST_BE_TO_ONE: "Field quan hệ này không hỗ trợ tra cứu — lookup chỉ dùng được với quan hệ 1 phía (to-one).",
  FIELD_LOOKUP_SOURCE_FIELD_REQUIRED: "Trường tra cứu (lookup) cần chọn field cần lấy trên đối tượng đích.",
  FIELD_LOOKUP_SOURCE_FIELD_UNKNOWN: "Field cần lấy không tồn tại trên đối tượng đích.",

  // Field config — rollup (ENG-COMPUTED #3, api-conventions §5.2)
  FIELD_ROLLUP_CHILD_ENTITY_REQUIRED: "Trường tổng hợp (rollup) cần chọn đối tượng con.",
  FIELD_ROLLUP_CHILD_ENTITY_INVALID: "Tên đối tượng con không hợp lệ.",
  FIELD_ROLLUP_CHILD_ENTITY_NOT_FOUND: "Đối tượng con không tồn tại.",
  FIELD_ROLLUP_METRIC_INVALID: "Chỉ số tổng hợp không hợp lệ (chỉ nhận đếm/tổng/trung bình/nhỏ nhất/lớn nhất).",
  FIELD_ROLLUP_VIA_RELATION_REQUIRED: "Trường tổng hợp cần chọn 1 trong 2 cách nối: field quan hệ (1-N) hoặc quan hệ N-N.",
  FIELD_ROLLUP_RELATION_AMBIGUOUS: "Chỉ được chọn MỘT cách nối tới đối tượng con — field quan hệ (1-N) hoặc quan hệ N-N, không thể cả hai.",
  FIELD_ROLLUP_RELATIONSHIP_REQUIRED: "Tên quan hệ N-N không hợp lệ hoặc rỗng.",
  FIELD_ROLLUP_VIA_RELATION_UNKNOWN: "Field quan hệ đã chọn không tồn tại trên đối tượng con.",
  FIELD_ROLLUP_VIA_RELATION_NOT_RELATION: "Field đã chọn trên đối tượng con không phải kiểu Quan hệ.",
  FIELD_ROLLUP_VIA_RELATION_WRONG_TARGET: "Field quan hệ đã chọn không trỏ ngược lại đối tượng hiện tại.",
  FIELD_ROLLUP_RELATIONSHIP_NOT_FOUND: "Quan hệ N-N đã chọn không tồn tại.",
  FIELD_ROLLUP_RELATIONSHIP_WRONG_ENTITIES: "Quan hệ N-N đã chọn không nối đúng cặp đối tượng (hiện tại ↔ đối tượng con).",
  FIELD_ROLLUP_AGG_FIELD_REQUIRED: "Cần chọn field cần tính trên đối tượng con (trừ khi chỉ số là đếm).",
  FIELD_ROLLUP_AGG_FIELD_UNKNOWN: "Field cần tính không tồn tại trên đối tượng con.",
  FIELD_ROLLUP_AGG_FIELD_TYPE_INVALID: "Field cần tính phải là kiểu số, tiền tệ, hoặc đánh giá.",
  FIELD_FORMULA_NESTED: "Không thể trỏ tới một trường tính toán khác (công thức/tra cứu/tổng hợp) — chặn tính-toán-lồng-tính-toán.",
  FIELD_FORMULA_TYPE_MISMATCH: "Công thức sai kiểu — phép so sánh chỉ dùng làm điều kiện của IF, và kết quả phải là số.",

  // Field config — auto_number (ENG #6, api-conventions §7)
  FIELD_AUTONUMBER_PREFIX_INVALID: "Tiền tố không hợp lệ (tối đa 20 ký tự).",
  FIELD_AUTONUMBER_PADDING_INVALID: "Độ đệm số không hợp lệ (0 đến 12).",
  RECORD_AUTONUMBER_UNAVAILABLE: "Số tự sinh tạm thời không khả dụng trên máy chủ — thử lại sau.",
  RECORD_ROLLUP_UNAVAILABLE: "Trường tổng hợp tạm thời không khả dụng — đối tượng/quan hệ liên quan có thể đã bị xoá.",
  RECORD_ROLLUP_TOO_MANY_CHILDREN: "Quá nhiều bản ghi liên kết (trên 5000) để tính chính xác — thu hẹp phạm vi trước.",

  // Field config — relation cardinality/inverseField (ENG-REL2 #4/#5, api-conventions §6)
  FIELD_RELATION_CARDINALITY_INVALID: "Bậc quan hệ không hợp lệ (chỉ nhận 1-N hoặc 1-1).",
  FIELD_RELATION_CARDINALITY_CONFLICT: "Dữ liệu hiện có đã vi phạm ràng buộc 1-1 (nhiều bản ghi con cùng trỏ 1 bản ghi cha) — không thể đổi sang 1-1.",
  FIELD_RELATION_INVERSE_INVALID: "Tên inverse field không hợp lệ.",
  FIELD_RELATION_INVERSE_TAKEN: "Tên inverse field đã được dùng bởi một quan hệ khác trên đối tượng cha — chọn tên khác hoặc để trống.",
  FIELD_RELATION_OPTIONS_IMMUTABLE: "Không thể đổi đối tượng đích / bậc quan hệ / inverse field sau khi tạo — xoá trường và tạo lại nếu cần trỏ chỗ khác.",
  FIELD_RELATION_UNIQUE_UNSUPPORTED: "Trường quan hệ không thể đặt duy nhất (unique) — dùng bậc quan hệ 1-1 thay thế.",
  FIELD_POLYMORPHIC_NO_TARGETS: "Trường quan hệ đa hình cần ít nhất một entity đích.",
  FIELD_POLYMORPHIC_TARGET_NOT_FOUND: "Một trong các entity đích không tồn tại.",
  FIELD_POLYMORPHIC_NAME_TOO_LONG: "Tên trường quan hệ đa hình quá dài (tối đa 21 ký tự).",
  FIELD_POLYMORPHIC_NOT_UNIQUE: "Trường quan hệ đa hình không thể đặt duy nhất (unique).",
  FIELD_POLYMORPHIC_NOT_INDEXED: "Trường quan hệ đa hình không thể đánh chỉ mục (index).",

  // Field config — unique/indexed không hỗ trợ trên field/computed/file (mirror, config-time)
  FIELD_FILE_NOT_UNIQUE: "Trường tệp/hình ảnh không thể đặt duy nhất (unique).",
  FIELD_FILE_NOT_INDEXED: "Trường tệp/hình ảnh không thể đánh chỉ mục (index).",
  FIELD_COMPUTED_NOT_UNIQUE: "Trường tính toán (chỉ đọc) không thể đặt duy nhất (unique).",
  FIELD_COMPUTED_NOT_INDEXED: "Trường tính toán (chỉ đọc) không thể đánh chỉ mục (index).",

  // System / routing
  ENTITY_NOT_FOUND: "Đối tượng không tồn tại trong hệ thống.",
  RECORD_NOT_FOUND: "Bản ghi không tồn tại hoặc đã bị xoá.",
  // Permanent delete (specs/056): the screen builds the exact sentence from `details` (purgeRefusal); these are the fallbacks.
  RECORD_PURGE_LIVE_CHILDREN: "Còn bản ghi đang dùng bản ghi này. Xoá chúng vào thùng rác trước.",
  RECORD_NOT_IN_RECYCLE_BIN: "Bản ghi này không còn trong thùng rác.",
  RECORD_PURGE_TOO_LARGE: "Quá nhiều bản ghi cho một lần xoá vĩnh viễn; không xoá gì cả.",
  RECORD_WRITE_CONFLICT: "Hệ thống đang bận, bấm lại.",
  REQUEST_TOO_LARGE: "Dữ liệu gửi lên quá lớn.",
  PAGE_TOO_DEEP: "Trang quá sâu. Vui lòng dùng tìm kiếm để thu hẹp kết quả.",
  INVALID_CURSOR: "Con trỏ phân trang không hợp lệ. Vui lòng tải lại.",
  CURSOR_REQUIRES_INDEXED_SORT: "Trường sắp xếp này chưa được đánh chỉ mục — không thể dùng cursor mode.",
  // queue #317 (api-conventions §4.4b): a column the caller's role may not view answers EXACTLY like a
  // column that does not exist, so one sentence has to cover both — it must not say which it is.
  LIST_UNKNOWN_FILTER: "Không lọc được theo cột này (cột không có hoặc bạn không được xem). Bỏ bộ lọc đó rồi thử lại.",
  SORT_NOT_ALLOWED: "Không sắp xếp được theo cột này.",

  // Aggregate / report
  AGGREGATE_REQUIRES_INDEXED: "Field nhóm (groupBy) cần được đánh index trước khi báo cáo có thể chạy.",

  // Workflow / approvals (W6 — api-conventions §9)
  WORKFLOW_NOT_FOUND: "Workflow không tồn tại hoặc đã bị xoá.",
  WORKFLOW_TEST_RECORD_REQUIRED: "Chọn một bản ghi để chạy thử.",
  // Queue #79 — the builder pins these onto the action's calendar dropdown (details.field =
  // "actions[i].calendarId"). The raw server sentence carries the calendar UUID and engine words;
  // these name the control that fixes it.
  SET_DEADLINE_CALENDAR_NOT_FOUND: "Lịch làm việc ghim cho hành động Đặt hạn xử lý không còn tồn tại (đã bị xoá). Ở ô «Lịch làm việc» của hành động đó, chọn lịch khác hoặc «Lịch mặc định của tổ chức» rồi lưu lại.",
  SET_DEADLINE_CALENDAR_INVALID: "Lịch làm việc chỉ dùng được với đơn vị «giờ làm việc» — đơn vị khác không đi theo lịch nào. Bỏ lịch ghim, hoặc đổi đơn vị sang giờ làm việc.",
  APPROVAL_INSTANCE_NOT_FOUND: "Yêu cầu phê duyệt không tồn tại hoặc bạn không có quyền xem.",
  APPROVAL_PROCESS_NOT_FOUND: "Quy trình phê duyệt không tồn tại hoặc đã bị xoá.",
  // specs/043 S2 (FR-027): the server refuses a blank reason — also ones the screen's own check lets by.
  APPROVAL_REJECT_REASON_REQUIRED: "Cần ghi lý do khi từ chối — người gửi cần biết vì sao hồ sơ bị trả.",
  // specs/043 S3b (409): a second tab / a delegation / an SLA escalation got in first — nothing of this click was applied.
  // specs/043 S4 (409): one person decides a many-seat step once; an admin without a seat uses «Quyết thay».
  APPROVAL_ALREADY_DECIDED_BY_ACTOR: "Bạn đã quyết ở bước này rồi — mỗi người chỉ quyết một lần trên một bước, kể cả khi bạn giữ hai ghế.",
  APPROVAL_ADMIN_OVERRIDE_REQUIRED: "Bước này có nhiều người cùng duyệt và bạn không phải một trong số họ. Muốn quyết thay cả bước, dùng «Quyết thay» và ghi lý do.",
  APPROVAL_DELEGATE_TARGET_SEATED: "Người này đã có ghế hoặc đã bỏ phiếu ở bước này — mỗi người chỉ bỏ một phiếu, nên không giao thêm ghế cho họ được. Chọn người khác.",
  APPROVAL_OVERRIDE_NOTE_REQUIRED: "Quyết thay cần ghi lý do — đó là dấu vết duy nhất về việc vì sao các ghế bị bỏ qua.",
  APPROVAL_CONFLICT: "Phiếu này vừa có người xử lý hoặc đã chuyển sang bước khác — chưa có gì của lần bấm này được ghi. Xem trạng thái mới rồi quyết định tiếp.",

  // Vertical pack / deploy (W8 — api-conventions §12)
  VERTICAL_MANIFEST_INVALID_VERSION: "Phiên bản (version) trong manifest của pack không đúng định dạng semver (vd 1.2.0).",
  VERTICAL_MANIFEST_INVALID_SLUG: "Slug trong manifest không hợp lệ (chỉ a-z, 0-9, gạch ngang; tối đa 150 ký tự).",
  VERTICAL_PACK_EMPTY: "Pack rỗng — không có đối tượng nào để cài.",
  VERTICAL_PACK_CYCLE: "Pack có quan hệ vòng giữa các đối tượng — không thể xác định thứ tự cài.",
  VERTICAL_PACK_REQUIRED: "Cần cung cấp nội dung pack hoặc chọn một bộ dựng sẵn.",
  VERTICAL_BUILTIN_NOT_FOUND: "Không tìm thấy bộ dựng sẵn với tên đã chọn.",
  VERTICAL_INSTALLS_FAILED: "Không đọc được nhật ký cài đặt. Vui lòng thử lại.",

  // Dashboard / widget config (EPIC 6 — admin CRUD + grid layout schema)
  DASHBOARD_NOT_FOUND: "Dashboard không tồn tại hoặc đã bị xoá.",
  DASHBOARD_NAME_REQUIRED: "Tên dashboard không được để trống.",
  DASHBOARD_ID_REQUIRED: "Thiếu id dashboard.",
  WIDGET_LAYOUT_INVALID: "Không lưu được bố cục widget — kích thước hoặc vị trí không hợp lệ.",
  WIDGET_TYPE_INVALID: "Loại widget không hợp lệ (chỉ số/cột/đường/bảng/phễu).",
  WIDGET_METRIC_INVALID: "Chỉ số widget không hợp lệ (chỉ đếm/tổng/trung bình/nhỏ nhất/lớn nhất).",
  WIDGET_ENTITY_REQUIRED: "Widget cần chọn đối tượng.",
  WIDGET_BUCKET_INVALID: "Nhóm thời gian không hợp lệ (chỉ ngày/tuần/tháng/quý/năm).",
  WIDGET_BUCKET_REQUIRES_GROUPBY: "Nhóm thời gian cần một field ngày để nhóm theo.",

  // AI Builder (EPIC 9 M2 — spec ../specs/020-e9-ai-builder/spec.md §6)
  AI_DISABLED: "AI Builder đang tắt trên máy chủ — hãy dựng thủ công trong Mô hình dữ liệu.",
  AI_GATEWAY_ERROR: "Cổng LLM gặp lỗi. Vui lòng thử lại sau giây lát.",
  AI_TIMEOUT: "LLM phản hồi quá chậm. Vui lòng thử lại.",
  AI_CHAT_FAILED: "Không gọi được AI Builder. Vui lòng thử lại.",
  INVALID_BODY: "Nội dung gửi lên không hợp lệ.",

  // Saved views (W9b/W9c — api-conventions §13.2)
  VIEW_NOT_FOUND: "View không tồn tại hoặc đã bị xoá.",
  VIEW_FORBIDDEN: "Chỉ quản trị viên mới được tạo/sửa/xoá view dùng chung.",
  VIEW_NAME_REQUIRED: "Tên view không được để trống.",
  VIEW_NAME_TOO_LONG: "Tên view quá dài (tối đa 150 ký tự).",
  VIEW_CONFIG_INVALID: "Cấu hình view không hợp lệ.",
  VIEW_CONFIG_TOO_LARGE: "Cấu hình view quá lớn (tối đa 64 KiB).",
  VIEW_ENTITY_REQUIRED: "Thiếu đối tượng cho view.",
  // Relation map (specs/075 L4a) — business wording, no engine vocabulary.
  RELATIONSHIP_HAS_DEPENDENTS: "Chưa gỡ được liên kết vì còn trường tổng hợp dựa vào nó. Gỡ các trường đó trước.",
  RELATIONSHIP_NOT_FOUND: "Liên kết đã được gỡ trước đó.",
  RELVIEW_INVALID: "Cách hiện danh sách liên quan chưa hợp lệ. Kiểm tra lại các ô rồi lưu lại.",
  RELVIEW_COLUMN_UNKNOWN: "Có cột không còn tồn tại trong bảng chi tiết. Bỏ chọn cột đó rồi lưu lại.",
  RELVIEW_SORT_NOT_SORTABLE: "Không sắp xếp theo trường này được. Chọn trường khác.",
};

/**
 * Resolve an error to a human-readable Vietnamese message.
 * Priority: code map → ex.message → fallback string.
 */
export function errorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const mapped = err.code ? ERROR_MESSAGES[err.code] : null;
    // Use || not ?? because Error.message defaults to "" (empty string) when undefined is passed.
    return mapped || err.message || "Đã xảy ra lỗi. Vui lòng thử lại.";
  }
  if (err instanceof Error) return err.message || "Đã xảy ra lỗi. Vui lòng thử lại.";
  return "Đã xảy ra lỗi. Vui lòng thử lại.";
}

// Screen (layout) save sentences for the Studio only (specs/075 FR-107). They are NOT in ERROR_MESSAGES on purpose:
// errorMessage() prefers a mapped sentence over the server's own, and LAYOUT_INVALID carries ~100 distinct, specific
// reasons that other screens (AI Builder card, old LayoutBuilder) still show as the server wrote them.
const STUDIO_ERRORS: Record<string, string> = {
  LAYOUT_INVALID: "Màn này chưa lưu được: còn trường bắt buộc chưa nằm trên form hoặc có thiết lập không hợp lệ.",
  LAYOUT_VISIBLE_WHEN_INVALID: "Điều kiện \"Chỉ hiện khi\" chưa hợp lệ: trường điều khiển phải nằm trên form, không là chính nó, không tạo vòng, và trường bắt buộc không được ẩn.",
  LAYOUT_NOT_FOUND: "Màn này không còn nữa. Đã được đưa về dạng tự sinh ở nơi khác.",
  ALREADY_DELIVERED: "App này đã giao cho khách rồi.",
  NOTHING_TO_PUBLISH: "Chưa có gì để giao.",
  TENANT_MAINTENANCE: "Khách này đang bảo trì nên tạm khoá ghi. Thử lại sau.",
  // Relation map form (specs/075 FR-107): the engine's own sentences say "inverse"/"đối tượng"/"unique" — never shown in the Studio.
  FIELD_ALREADY_EXISTS: "Tên này đã được dùng ở bảng chi tiết. Chọn tên khác.",
  FIELD_RELATION_TARGET_REQUIRED: "Chọn bảng chính cho liên kết.",
  FIELD_RELATION_TARGET_INVALID: "Bảng chính đã chọn không dùng được. Chọn lại.",
  FIELD_RELATION_TARGET_NOT_FOUND: "Bảng chính đã chọn không còn nữa. Chọn lại.",
  FIELD_RELATION_CARDINALITY_INVALID: "Kiểu liên kết này chưa hỗ trợ.",
  FIELD_RELATION_CARDINALITY_CONFLICT: "Dữ liệu hiện có không hợp với kiểu Một–một.",
  FIELD_RELATION_INVERSE_INVALID: "Tên nhìn từ bảng chính chỉ dùng chữ thường không dấu, số và gạch dưới, bắt đầu bằng chữ.",
  FIELD_RELATION_INVERSE_TAKEN: "Tên này đã được một liên kết khác dùng ở bảng chính. Chọn tên khác hoặc để trống.",
  FIELD_RELATION_OPTIONS_IMMUTABLE: "Không đổi được bảng đích hoặc kiểu liên kết. Gỡ liên kết rồi tạo mới.",
  FIELD_RELATION_UNIQUE_UNSUPPORTED: "Muốn mỗi bên chỉ một bản ghi, chọn kiểu Một–một.",
  FIELD_REQUIRED_HAS_VISIBLE_WHEN: "Trường đang có điều kiện hiện nên chưa bắt buộc được. Bỏ điều kiện trước.",
};

/**
 * Studio variant of errorMessage (specs/075 FR-107): only the dictionary sentence for a server code, else one fixed
 * sentence. NEVER err.message, which can carry engine words or internal codes.
 */
export function strictErrorMessage(err: unknown): string {
  const code = err instanceof ApiError ? err.code : "";
  return (code && (STUDIO_ERRORS[code] ?? ERROR_MESSAGES[code])) || "Không lưu được. Thử lại, hoặc báo người quản trị xưởng.";
}

/**
 * Resolve just the code string from an error (for badge display).
 */
export function errorCode(err: unknown): string | null {
  if (err instanceof ApiError) return err.code ?? null;
  return null;
}
