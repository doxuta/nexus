// help-content.ts — nguồn nội dung MỘT-CHỖ cho hệ thống trợ giúp trong sản phẩm
// (E2-IA-DESIGN-SYSTEM.md §11.3). HelpHint đọc `short`, trang /about (glossary) đọc `long`
// (fallback `short` nếu thiếu `long`). Sửa 1 chỗ, mọi nơi cập nhật.
//
// NGUYÊN TẮC: nội dung phải khớp NĂNG LỰC ĐÃ BUILD THẬT (backend nexus-platform, đối chiếu
// 2026-07-02) — không hứa tính năng chưa có. lookup/rollup/auto_number/cardinality/inverseField
// đều đã có giao diện cấu hình trong Studio field-builder (DataModelBuilder.tsx, nhóm "Nâng cao
// / Tính toán" trong dropdown Loại dữ liệu) — xem entry tương ứng bên dưới cho chi tiết field nào
// còn giới hạn (vd inverseField chỉ đặt được lúc tạo field relation, không đổi được sau).

export interface HelpEntry {
  /** Tên thuật ngữ ngắn — dùng làm tiêu đề trong glossary + aria-label. */
  term: string;
  /** 1 câu ngắn (≤ ~110 ký tự) — hiện trong tooltip HelpHint. */
  short: string;
  /** 2-4 câu chi tiết hơn — hiện trong glossary trang /about. Fallback: `short`. */
  long?: string;
}

export const HELP_CONTENT = {
  // ── Metadata cốt lõi ──────────────────────────────────────────────────────
  entity: {
    term: "Entity (đối tượng)",
    short: "Một loại dữ liệu nghiệp vụ, ví dụ Khách hàng hay Ticket — như một bảng trong hệ thống.",
    long: "Entity là đơn vị dữ liệu nền tảng của Nexus — mỗi entity ứng với 1 bảng obj_* trong cơ sở dữ liệu. Team Nexus định nghĩa entity mới trong Studio (Mô hình dữ liệu) mà không cần deploy lại code; engine tự sinh bảng, API và giao diện cho entity đó.",
  },
  field: {
    term: "Field (trường)",
    short: "Một cột dữ liệu bên trong entity, ví dụ 'Số điện thoại' hay 'Ngày ký hợp đồng'.",
    long: "Field thuộc về 1 entity, có kiểu dữ liệu (văn bản, số, ngày, quan hệ…), ràng buộc (bắt buộc, duy nhất) và tuỳ chọn hiển thị riêng. Thêm field mới trong Studio không cần sửa code — engine tự thêm cột hoặc lưu vào cột JSON tuỳ kiểu.",
  },
  record: {
    term: "Record (bản ghi)",
    short: "Một dòng dữ liệu cụ thể của entity, ví dụ 1 khách hàng hay 1 ticket cụ thể.",
    long: "Mỗi record có id (UUID), version (dùng cho khoá lạc quan — xem '409 conflict'), và dữ liệu các field. Record thuộc về đúng 1 tenant, không bao giờ lẫn sang tenant khác.",
  },
  tenant: {
    term: "Tenant (tổ chức)",
    short: "Không gian dữ liệu riêng của 1 khách/tổ chức — dữ liệu giữa các tenant tách biệt hoàn toàn.",
    long: "Nexus là nền tảng đa tổ chức (multi-tenant): mọi bảng dữ liệu đều lọc theo tenant_id, không tenant nào nhìn thấy dữ liệu tenant khác — kể cả thao tác quản trị. Mỗi khách của Nexus là 1 tenant (hoặc 1 cụm tenant riêng khi triển khai on-prem).",
  },
  teamSet: {
    term: "Team-set",
    short: "Tập hợp các nhóm (team) được gán quyền xem 1 bản ghi cụ thể — cơ chế chia sẻ theo nhóm.",
    long: "Mỗi record có thể gắn 1 team_set_id. Ai thuộc team trong team-set đó (và có quyền 'team' trở lên trên entity) mới xem/sửa được record — đây là tầng phân quyền row-level, tách biệt với ACL theo entity/hành động.",
  },
  layout: {
    term: "Layout (bố cục)",
    short: "Cấu hình form/danh sách/chi tiết — engine đọc layout để tự render UI, không hardcode theo entity.",
    long: "Layout Engine là lớp giữa metadata và giao diện: cùng 1 bộ component (LayoutList/LayoutForm/LayoutDetail) render mọi entity, dựa trên cấu hình section + thứ tự field lưu trong sys_layouts. Sửa layout trong Studio thay đổi ngay giao diện App khách, không cần code lại màn hình.",
  },

  // ── Field types & computed ────────────────────────────────────────────────
  fieldType: {
    term: "Loại dữ liệu (field type)",
    short: "Quyết định cách nhập & hiển thị field (văn bản/số/ngày/quan hệ…) — không đổi được sau khi tạo.",
    long: "Nexus hỗ trợ nhiều loại field: văn bản, số, ngày/giờ, tiền tệ, chọn một/nhiều, quan hệ, đánh giá sao, trường ghép, công thức… Vì kiểu dữ liệu ảnh hưởng đến cách lưu trữ, không thể đổi loại field sau khi đã tạo — phải tạo field mới nếu cần đổi kiểu.",
  },
  relationCardinality: {
    term: "Bậc quan hệ (cardinality)",
    short: "1-1 (một-một), 1-N (một-nhiều) hoặc N-N (nhiều-nhiều) — số bản ghi liên kết được ở mỗi phía.",
    long: "Field kiểu 'Quan hệ' trỏ tới 1 entity khác tạo cạnh 1-N (mỗi record cha có nhiều record con). Quan hệ N-N tạo riêng qua bảng trung gian (nút 'Thêm N:N' trong Sơ đồ quan hệ, hoặc kéo-thả từ node này sang node khác). 1-1 tương đương 1-N cộng ràng buộc duy nhất trên field.",
  },
  inverseField: {
    term: "Inverse field (trường nghịch đảo)",
    short: "Tên gọi phía 'cha' để xem ngược danh sách record con — engine tự đăng ký khi tạo quan hệ.",
    long: "Khi tạo field 'Quan hệ' trỏ sang entity khác, backend tự đăng ký 1 dòng registry mang cả tên field phía con lẫn inverseField phía cha (dùng khi expand ngược). Studio cho đặt tên tuỳ chọn ngay lúc tạo field relation ('để trống = engine tự đặt'); sau khi tạo, giá trị này KHÔNG đổi được nữa (bất biến — cần trỏ chỗ khác thì xoá field và tạo lại).",
  },
  lookup: {
    term: "Lookup (trường tra cứu)",
    short: "Trường tự lấy giá trị từ record liên quan qua 1 quan hệ, ví dụ hiển thị ngành của khách hàng ngay trên Ticket.",
    long: "Lookup là field tính toán (computed, chỉ đọc) — luôn resolve giá trị mới nhất khi đọc, không lưu vào record. Studio field-builder (nhóm 'Nâng cao / Tính toán'): chọn field 'Quan hệ' làm cầu nối (via, phải là quan hệ to-one) rồi chọn field trên entity đích cần mirror (sourceField).",
  },
  rollup: {
    term: "Rollup (trường tổng hợp)",
    short: "Trường tự tính tổng/đếm/trung bình từ các record con qua 1 quan hệ 1-N hoặc N-N.",
    long: "Ví dụ: tổng giá trị các Đơn hàng của 1 Khách hàng. Studio field-builder: chọn entity con, chỉ số (đếm/tổng/trung bình/nhỏ nhất/lớn nhất), field cần tính (số, tiền tệ, đánh giá, hoặc công thức ra số/tiền như 'Thành tiền' = số lượng × đơn giá; bỏ qua nếu đếm), và MỘT trong hai cách nối — qua field quan hệ 1-N sẵn có trên entity con, hoặc qua một quan hệ N-N đã đăng ký.",
  },
  formula: {
    term: "Formula (công thức)",
    short: "Trường tính toán từ biểu thức phía server, ví dụ 'price * quantity' — luôn chỉ đọc.",
    long: "Formula field không lưu giá trị nhập tay — engine tính lại từ biểu thức mỗi lần bản ghi được lưu. Biểu thức dùng + - * / ( ) trên field số/tiền tệ, và IF(điều kiện, giá trị nếu đúng, giá trị nếu sai) với so sánh > >= < <= = != — vd hoa hồng bậc thang IF(amount >= 100000000, amount * 0.05, amount * 0.03). Có thể dùng cả trường tra cứu (số/tiền) và trường tổng hợp — vd Thành tiền = Số lượng × Đơn giá tra cứu từ bảng dịch vụ; công thức kiểu này tính lại mỗi lần xem nên luôn theo giá hiện hành (nhưng không dùng để lọc/cộng gộp). Kết quả là số hoặc tiền tệ.",
  },
  autoNumber: {
    term: "Auto-number (số tự sinh)",
    short: "Trường tự sinh mã tăng dần khi tạo record, ví dụ 'HD-000123' (tiền tố + số đệm).",
    long: "Cấu hình gồm tiền tố (prefix, tối đa 20 ký tự) và độ dài đệm số 0 (padding, 0-12). Giá trị được server gán đúng 1 lần khi tạo record (không đổi lại được khi sửa) — Studio field-builder có ô xem trước định dạng (vd 'INV-00042') ngay khi gõ.",
  },
  picklist: {
    term: "Picklist (chọn một)",
    short: "Trường chọn đúng 1 giá trị trong danh sách cố định, ví dụ trạng thái Mới/Đang xử lý/Hoàn tất.",
  },
  multiSelect: {
    term: "Multi-select (chọn nhiều)",
    short: "Trường chọn nhiều giá trị cùng lúc trong 1 danh sách cố định.",
  },
  composite: {
    term: "Composite (trường ghép)",
    short: "1 field chứa nhiều field con đơn giản, ví dụ 'Họ tên' gồm Họ + Tên đệm + Tên.",
    long: "Composite là atomic-write: khi gửi dữ liệu, nếu 1 field con bắt buộc bị thiếu, cả field ghép báo lỗi (không lưu một phần). Chỉ chấp nhận field con kiểu scalar (text/number/date/boolean).",
  },
  currencyField: {
    term: "Currency (tiền tệ)",
    short: "Trường tiền tệ — lưu kèm mã tiền (VND, USD…), hỗ trợ min/max/precision riêng theo field.",
  },
  relatedEntity: {
    term: "Entity đích",
    short: "Entity mà field 'Quan hệ' trỏ tới — engine tự tạo cạnh 1-N và đăng ký inverse field phía đó.",
    long: "Chọn entity đích khi tạo field kiểu 'Quan hệ'. Sau khi lưu, entity đích sẽ xuất hiện trong Sơ đồ quan hệ (Studio) với 1 cạnh 1:N nối tới entity hiện tại.",
  },
  fieldUnique: {
    term: "Duy nhất (unique)",
    short: "Không cho phép 2 record có cùng giá trị ở field này trong cùng tenant — vi phạm báo lỗi trùng.",
    long: "Bật 'Duy nhất' tạo unique index trên cột tương ứng. Ghi trùng giá trị sẽ bị chặn ở tầng CSDL và trả về lỗi RECORD_DUPLICATE (409) thay vì lưu đè.",
  },

  // ── ACL 3 tầng ────────────────────────────────────────────────────────────
  aclLevels: {
    term: "Mức truy cập",
    short: "4 mức: Không có / Chỉ mình (owner) / Theo nhóm (team) / Tất cả — áp cho từng entity × hành động.",
    long: "Ma trận đọc theo hàng 'Tất cả entity' (✶) làm mặc định; entity cụ thể ghi đè khi có giá trị riêng (ô chữ nghiêng = đang kế thừa từ ✶, chưa có giá trị riêng).",
  },
  aclOwner: {
    term: "Chỉ mình (owner)",
    short: "Chỉ người tạo (owner) của record mới được thực hiện hành động này.",
  },
  aclTeam: {
    term: "Theo nhóm (team)",
    short: "Chỉ thành viên team trong team-set của record mới được thực hiện hành động này.",
  },
  aclAll: {
    term: "Tất cả (all)",
    short: "Mọi user trong tenant đều thực hiện được, không giới hạn theo người tạo hay team.",
  },
  aclNone: {
    term: "Không có (none)",
    short: "Không ai được phép thực hiện hành động này trên entity, bất kể vai trò.",
  },
  aclModule: {
    term: "Phân quyền module (entity-level)",
    short: "Quyền xem/tạo/sửa/xoá theo TỪNG entity — tầng phân quyền thô nhất, kiểm tra trước business logic.",
    long: "Đây là tầng ACL đầu tiên trong chuỗi Auth → Tenant → ACL → Business logic: request bị chặn ở đây trước khi chạm tới dữ liệu, nếu role không có quyền trên entity/hành động tương ứng.",
  },
  aclFieldLevel: {
    term: "Phân quyền field-level",
    short: "Ẩn/chỉ-đọc riêng từng trường theo vai trò — chỉnh trên lưới này, lưu là áp dụng ngay.",
    long: "Chọn vai trò + entity rồi đặt từng ô: Ẩn (field không trả về cho role đó), Chỉ đọc (đọc được nhưng ghi bị từ chối), hoặc Toàn quyền. Bấm Lưu là engine áp dụng ngay cho mọi request kế tiếp — không cần user đăng nhập lại. Lưu ý: quản trị viên (isAdmin) luôn bỏ qua field-ACL nên hãy kiểm tra bằng tài khoản thường.",
  },
  aclRowLevel: {
    term: "Phân quyền row-level",
    short: "Kiểm soát TỪNG bản ghi cụ thể qua team-set — tầng phân quyền chi tiết nhất, cộng thêm vào module-level.",
  },

  // ── Trạng thái record ─────────────────────────────────────────────────────
  recordConflict409: {
    term: "Xung đột phiên bản (409)",
    short: "Người khác đã lưu bản ghi này trước bạn — khoá lạc quan (optimistic lock) chặn ghi đè im lặng.",
    long: "Mỗi record có version tăng dần theo mỗi lần sửa; khi lưu, form gửi kèm version bạn đang xem. Nếu version trên máy chủ đã mới hơn, backend từ chối bằng lỗi 409 thay vì âm thầm ghi đè — bạn cần tải lại bản mới nhất rồi nhập lại thay đổi của mình.",
  },
  softDelete: {
    term: "Xoá mềm (soft delete)",
    short: "Bản ghi bị đánh dấu đã xoá (deletedAt) chứ không xoá vật lý — nằm trong Thùng rác.",
    long: "Xoá 1 record chỉ set cột deleted_at, dữ liệu vẫn còn trong CSDL và không xuất hiện ở danh sách/tìm kiếm thường. Bản ghi đã xoá mềm hiện trong Thùng rác của entity; nút khôi phục sẽ kích hoạt khi API restore hoàn tất.",
  },
  cursorPagination: {
    term: "Phân trang cursor",
    short: "Cách tải trang sâu nhanh (O(1)) bằng con trỏ mã hoá, thay vì đếm offset — dùng cho danh sách rất dài.",
    long: "Khác với phân trang offset (page/limit — mặc định), cursor dùng giá trị opaque trả về từ trang trước (nextCursor) để lấy trang kế tiếp, tránh COUNT(*) toàn bảng khi danh sách lớn.",
  },

  // ── Bảo mật đăng nhập ─────────────────────────────────────────────────────
  mfa: {
    term: "MFA (xác thực 2 lớp)",
    short: "Yêu cầu thêm mã OTP/TOTP khi đăng nhập, ngoài mật khẩu — bật được theo từng user.",
  },
  sso: {
    term: "SSO (đăng nhập một lần)",
    short: "Đăng nhập qua nhà cung cấp danh tính bên ngoài (SAML/OIDC) thay vì tài khoản riêng của Nexus.",
  },

  // ── Vận hành nghiệp vụ ────────────────────────────────────────────────────
  workflow: {
    term: "Workflow (tự động hoá)",
    short: "Tự động hoá theo sự kiện: KHI bản ghi thay đổi → NẾU điều kiện đúng → THÌ thực hiện hành động.",
    long: "Hành động hỗ trợ: cập nhật field, gửi email, gửi thông báo, gọi webhook ngoài (HMAC ký). Chạy bất đồng bộ, fail-open (lỗi workflow không chặn thao tác chính của user).",
  },
  approval: {
    term: "Quy trình phê duyệt",
    short: "Luồng duyệt nhiều bước tuần tự trên 1 bản ghi trước khi hành động được coi là hoàn tất.",
  },
  reporting: {
    term: "Báo cáo",
    short: "Truy vấn tổng hợp (tổng/đếm/trung bình, nhóm theo field) dựng từ cùng lớp metadata với dữ liệu nghiệp vụ.",
  },
  dashboard: {
    term: "Dashboard",
    short: "Bảng ghép nhiều widget báo cáo để xem nhanh sức khoẻ nghiệp vụ, không cần công cụ BI riêng.",
  },
  importCsv: {
    term: "Nhập dữ liệu CSV",
    short: "Tải file CSV lên, ánh xạ cột → field, xem trước lỗi trước khi ghi thật vào hệ thống.",
  },
  bulkCreateMode: {
    term: "Chế độ gửi (tạo hàng loạt)",
    short: "Toàn bộ-hoặc-không: 1 dòng lỗi huỷ cả lô. Từng dòng: dòng đúng vẫn tạo dù dòng khác lỗi.",
    long: "\"Toàn bộ hoặc không\" (atomic, mặc định) validate hết mọi dòng trước — chỉ 1 dòng sai là KHÔNG dòng nào được ghi, gửi lại y nguyên hoàn toàn an toàn (không tạo trùng). \"Từng dòng\" (partial) ghi độc lập từng dòng hợp lệ — nhanh có kết quả từng phần, nhưng nếu gặp lỗi hạ tầng giữa chừng (mất kết nối/timeout) một vài dòng CÓ THỂ đã được tạo mà không rõ dòng nào; nên kiểm tra danh sách trước khi gửi lại để tránh trùng dữ liệu.",
  },

  // ── Hai khuôn mặt / vị thế sản phẩm ───────────────────────────────────────
  studioFace: {
    term: "Studio (khuôn mặt Builder)",
    short: "Nơi team Nexus dựng app cho khách bằng metadata — công cụ nội bộ, không phải tính năng cho khách.",
  },
  appFace: {
    term: "App khách (khuôn mặt vận hành)",
    short: "Ứng dụng nghiệp vụ đã dựng xong — nơi người dùng cuối làm việc hàng ngày với dữ liệu thật.",
  },
  vertical: {
    term: "Vertical",
    short: "Một bộ metadata đóng gói sẵn cho 1 ngành, ví dụ CRM, LMS, Ticketing — ráp nhanh rồi tuỳ biến 10%.",
  },
} as const satisfies Record<string, HelpEntry>;

export type HelpKey = keyof typeof HELP_CONTENT;

/** Lookup an toàn — trả undefined nếu key lạ (component tự xử lý, không throw). */
export function getHelp(key: string): HelpEntry | undefined {
  return (HELP_CONTENT as Record<string, HelpEntry>)[key];
}

/** Toàn bộ entry, kiểu rộng `HelpEntry[]` — dùng cho glossary (trang /about §11.1 điểm 5). */
export function allHelpEntries(): HelpEntry[] {
  return Object.values(HELP_CONTENT) as HelpEntry[];
}
