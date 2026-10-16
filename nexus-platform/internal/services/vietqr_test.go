// VietQR payload tests (DONG-TIEN-KHEP-KIN p1, ../specs/016-payment-flow/spec.md §7): the CRC algorithm
// against known CCITT-FALSE vectors, TLV structure round-trip, memo sanitization, and the
// amount extraction matrix.
package services

import (
	"strings"
	"testing"
)

func TestCRC16CCITTFalse_KnownVectors(t *testing.T) {
	// "123456789" → 0x29B1 is THE published check value for CRC-16/CCITT-FALSE.
	if got := crc16CCITTFalse([]byte("123456789")); got != 0x29B1 {
		t.Fatalf("check vector = %04X, want 29B1", got)
	}
	if got := crc16CCITTFalse(nil); got != 0xFFFF {
		t.Fatalf("empty = %04X, want FFFF (init)", got)
	}
}

func TestBuildVietQRPayload_Structure(t *testing.T) {
	p := BuildVietQRPayload("970436", "0011001234567", 150000, "DH-0042")

	// Header + fixed fields in EMVCo order.
	for _, want := range []string{
		"000201",          // 00 len=02 "01"
		"010212",          // 01 len=02 "12" dynamic
		"5303704",         // 53 len=03 "704" VND
		"5406150000",      // 54 len=06 "150000"
		"5802VN",          // 58 len=02 "VN"
		"62110807DH-0042", // 62 len=11 { 08 len=07 "DH-0042" }
		"0010A000000727",  // NAPAS AID inside 38
		"QRIBFTTA",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("payload %q missing %q", p, want)
		}
	}

	// CRC self-check: recompute over everything up to and including "6304".
	idx := strings.LastIndex(p, "6304")
	if idx == -1 || len(p) != idx+8 {
		t.Fatalf("payload tail malformed: %q", p)
	}
	wantCRC := crc16CCITTFalse([]byte(p[:idx+4]))
	if got := p[idx+4:]; got != strings.ToUpper(strings.TrimLeft(got, "")) || got != fmtCRC(wantCRC) {
		t.Fatalf("crc = %s, want %s", got, fmtCRC(wantCRC))
	}

	// No memo → no 62 template at all.
	if p2 := BuildVietQRPayload("970436", "0011001234567", 5000, ""); strings.Contains(p2, "6207") || strings.Contains(p2, "6211") {
		t.Fatalf("empty memo must omit field 62: %q", p2)
	}
}

func fmtCRC(v uint16) string {
	return strings.ToUpper(strings.TrimSpace(string([]byte{hexU(v >> 12), hexU(v >> 8), hexU(v >> 4), hexU(v)})))
}
func hexU(v uint16) byte {
	d := byte(v & 0xF)
	if d < 10 {
		return '0' + d
	}
	return 'A' + d - 10
}

func TestSanitizeVietQRMemo(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Thanh toán ĐH-0042", "Thanh toan DH-0042"},
		{"  nhiều   khoảng  trắng ", "nhieu khoang trang"},
		{"emoji 🎉 & symbols!@#", "emoji symbols"},
		{"chuỗi này dài quá hai mươi lăm ký tự chắc chắn luôn", "chuoi nay dai qua hai muo"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sanitizeVietQRMemo(tt.in); got != tt.want {
			t.Errorf("sanitize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPaymentAmountOf(t *testing.T) {
	data := map[string]any{
		"total":    float64(150000),
		"fraction": float64(150000.5),
		"zero":     float64(0),
		"neg":      float64(-5),
		"text":     "abc",
		"price":    map[string]any{"amount": float64(2500000), "currencyCode": "VND"},
	}
	if v, err := paymentAmountOf(data, "total"); err != nil || v != 150000 {
		t.Fatalf("total = %d, %v", v, err)
	}
	if v, err := paymentAmountOf(data, "price"); err != nil || v != 2500000 {
		t.Fatalf("currency composite = %d, %v", v, err)
	}
	for _, bad := range []string{"fraction", "zero", "neg", "text", "ghost", ""} {
		if _, err := paymentAmountOf(data, bad); err == nil {
			t.Errorf("field %q must be rejected", bad)
		}
	}
}

func TestExpandMemoTemplate(t *testing.T) {
	data := map[string]any{"code": "DH-0042", "total": float64(150000)}
	if got := expandMemoTemplate("TT {{code}} {{total}}", data, "rec-12345678-x"); got != "TT DH-0042 150000" {
		t.Fatalf("expand = %q", got)
	}
	if got := expandMemoTemplate("{{id}}", data, "abcdefgh1234"); got != "abcdefgh" {
		t.Fatalf("id token = %q", got)
	}
	if got := expandMemoTemplate("{{ghost}}x", data, "r"); got != "x" {
		t.Fatalf("absent token = %q", got)
	}
}
