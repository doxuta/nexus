package sampledata

// Embedded Vietnamese sample vocabulary. Plain Go slices: no file IO, no dependency, and the same
// index always yields the same word, so a batch id fully determines the rows.

var (
	familyNames = []string{"Nguyễn", "Trần", "Lê", "Phạm", "Hoàng", "Huỳnh", "Phan", "Vũ", "Võ", "Đặng", "Bùi", "Đỗ", "Hồ", "Ngô", "Dương", "Lý"}

	maleMiddle   = []string{"Văn", "Hữu", "Minh", "Quốc", "Đức", "Anh", "Thanh", "Gia"}
	femaleMiddle = []string{"Thị", "Ngọc", "Thu", "Mai", "Thanh", "Kim", "Phương", "Hồng"}

	maleGiven   = []string{"An", "Bình", "Cường", "Dũng", "Hải", "Hiếu", "Hùng", "Khoa", "Long", "Nam", "Phúc", "Quang", "Sơn", "Tài", "Tuấn", "Việt"}
	femaleGiven = []string{"Anh", "Chi", "Dung", "Giang", "Hà", "Hạnh", "Hoa", "Lan", "Linh", "Ngân", "Nhung", "Thảo", "Trang", "Uyên", "Vy", "Yến"}

	companyKinds  = []string{"Công ty TNHH", "Công ty Cổ phần", "Doanh nghiệp tư nhân", "Công ty TNHH MTV"}
	companyBrands = []string{"An Phát", "Hòa Bình", "Minh Long", "Việt Tiến", "Thành Công", "Phú Gia", "Đại Nam", "Hưng Thịnh", "Sao Mai", "Thiên Phú", "Bảo Minh", "Kim Long", "Tân Hoàng", "Nam Việt", "Ngọc Lan", "Thái Dương"}

	serviceNames = []string{"Tư vấn tổng quát", "Gói chăm sóc cơ bản", "Gói chăm sóc nâng cao", "Bảo trì định kỳ", "Kiểm tra sức khỏe tổng quát", "Dịch vụ giao tận nơi", "Gói tư vấn chuyên sâu", "Đào tạo kỹ năng", "Lắp đặt thiết bị", "Vệ sinh và bảo dưỡng", "Gói hỗ trợ 24/7", "Khóa học nhập môn"}

	streets   = []string{"Lê Lợi", "Nguyễn Huệ", "Trần Hưng Đạo", "Hai Bà Trưng", "Lý Thường Kiệt", "Phan Chu Trinh", "Điện Biên Phủ", "Võ Văn Tần", "Cách Mạng Tháng Tám", "Nguyễn Trãi", "Hoàng Diệu", "Pasteur"}
	districts = []string{"Quận 1", "Quận 3", "Quận Bình Thạnh", "Quận Hải Châu", "Quận Ba Đình", "Quận Hoàn Kiếm", "Quận Cầu Giấy", "Quận Ninh Kiều", "Quận Thanh Khê"}
	cities    = []string{"TP. Hồ Chí Minh", "Hà Nội", "Đà Nẵng", "Cần Thơ", "Hải Phòng", "Huế"}

	sentences = []string{
		"Khách hàng yêu cầu gọi lại vào buổi chiều.",
		"Đã gửi báo giá, đang chờ phản hồi.",
		"Cần xác nhận lại lịch hẹn trước một ngày.",
		"Ưu tiên xử lý trong tuần này.",
		"Khách hàng thân thiết, giảm giá theo chương trình.",
		"Đã trao đổi qua điện thoại, hẹn gặp trực tiếp.",
		"Chờ bổ sung thêm thông tin từ phía khách hàng.",
		"Hoàn tất theo đúng thỏa thuận ban đầu.",
	}
)
