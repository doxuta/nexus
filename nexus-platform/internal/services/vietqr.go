// VietQR payload builder (DONG-TIEN-KHEP-KIN p1, ../specs/016-payment-flow/spec.md §4): the EMVCo TLV
// string every VN banking app scans for a NAPAS-247 transfer-to-account, generated LOCALLY —
// no external API. The QR image itself is rendered client-side from this payload.
package services

import (
	"fmt"
	"strings"
	"unicode"
)

// vietQRTLV renders one ID-LEN-VALUE segment. Lengths are two decimal digits per EMVCo; every
// value this builder emits is ≤ 99 bytes by construction (memo capped at 25).
func vietQRTLV(id, value string) string {
	return fmt.Sprintf("%s%02d%s", id, len(value), value)
}

// BuildVietQRPayload assembles the NAPAS IBFT-to-account payload:
//
//	00 "01" · 01 "12" (dynamic) · 38 {00 A000000727, 01 {00 bin, 01 account}, 02 QRIBFTTA}
//	· 53 "704" · 54 amount · 58 "VN" · 62 {08 memo} (omitted when memo empty)
//	· 63 CRC-16/CCITT-FALSE over everything incl. the trailing "6304", uppercase hex.
//
// amount is whole VND (no decimals — VND has no minor unit); memo arrives pre-sanitized.
func BuildVietQRPayload(bankBin, accountNumber string, amount int64, memo string) string {
	account := vietQRTLV("00", bankBin) + vietQRTLV("01", accountNumber)
	napas := vietQRTLV("00", "A000000727") + vietQRTLV("01", account) + vietQRTLV("02", "QRIBFTTA")

	var b strings.Builder
	b.WriteString(vietQRTLV("00", "01"))
	b.WriteString(vietQRTLV("01", "12"))
	b.WriteString(vietQRTLV("38", napas))
	b.WriteString(vietQRTLV("53", "704"))
	b.WriteString(vietQRTLV("54", fmt.Sprintf("%d", amount)))
	b.WriteString(vietQRTLV("58", "VN"))
	if memo != "" {
		b.WriteString(vietQRTLV("62", vietQRTLV("08", memo)))
	}
	b.WriteString("6304")
	payload := b.String()
	return payload + fmt.Sprintf("%04X", crc16CCITTFalse([]byte(payload)))
}

// crc16CCITTFalse is CRC-16/CCITT-FALSE (poly 0x1021, init 0xFFFF, no reflect, no xorout) —
// the checksum EMVCo mandates for QR payloads.
func crc16CCITTFalse(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, d := range data {
		crc ^= uint16(d) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// sanitizeVietQRMemo makes a transfer note banks won't mangle: Vietnamese diacritics stripped
// to ASCII, anything outside [A-Za-z0-9 .-] dropped, whitespace collapsed, capped at 25 chars
// (the tightest common purpose limit — spec §4).
func sanitizeVietQRMemo(s string) string {
	var b strings.Builder
	for _, r := range s {
		r = stripVNDiacritic(r)
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if len(out) > 25 {
		out = strings.TrimSpace(out[:25])
	}
	return out
}

// stripVNDiacritic maps Vietnamese letters to their base ASCII letter (đ→d included);
// everything else passes through untouched.
func stripVNDiacritic(r rune) rune {
	lower := unicode.ToLower(r)
	var base rune
	switch {
	case strings.ContainsRune("àáảãạăằắẳẵặâầấẩẫậ", lower):
		base = 'a'
	case strings.ContainsRune("èéẻẽẹêềếểễệ", lower):
		base = 'e'
	case strings.ContainsRune("ìíỉĩị", lower):
		base = 'i'
	case strings.ContainsRune("òóỏõọôồốổỗộơờớởỡợ", lower):
		base = 'o'
	case strings.ContainsRune("ùúủũụưừứửữự", lower):
		base = 'u'
	case strings.ContainsRune("ỳýỷỹỵ", lower):
		base = 'y'
	case lower == 'đ':
		base = 'd'
	default:
		return r
	}
	if unicode.IsUpper(r) {
		return unicode.ToUpper(base)
	}
	return base
}
