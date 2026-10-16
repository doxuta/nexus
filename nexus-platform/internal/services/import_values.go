package services

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Human cell values for CSV import (specs/061 FR-001..FR-007). A customer's spreadsheet is written
// for people: picklist LABELS, amounts with Vietnamese thousand separators, day-first dates. These
// helpers turn such a cell into the machine value ValidateRecord judges. They are import-only: the
// JSON API stays strict, a form never sends "3.200.000".

// vnWallClock is the zone a day-first datetime cell is read in. Vietnam has no DST, so a fixed
// offset equals Asia/Ho_Chi_Minh (DefaultCalendarTimezone) without depending on the host's tzdata.
// ponytail: one zone for every tenant — per-tenant zone when the product leaves Vietnam.
var vnWallClock = time.FixedZone("ICT", 7*60*60)

var (
	// A day-first date: D/M/YYYY, D-M-YYYY or D.M.YYYY, optional wall-clock time.
	dayFirstRe = regexp.MustCompile(`^(\d{1,2})([/.-])(\d{1,2})([/.-])(\d{4})(?:[ T](\d{1,2}):(\d{2})(?::(\d{2}))?)?$`)
	// A machine float with an exponent ("3.2e+09") — the only non-digit form accepted, so
	// strconv's "NaN"/"Inf"/hex floats never reach a record.
	exponentRe = regexp.MustCompile(`^[+-]?\d+(\.\d+)?[eE][+-]?\d+$`)
	// An amount with an optional currency token before or after it.
	amountRe = regexp.MustCompile(`^(\p{L}+|₫)?\s*([-+]?[\d.,\s\x{00A0}]+(?:[eE][-+]?\d+)?)\s*(\p{L}+|₫)?$`)
)

// parseHumanNumber reads a number the way a Vietnamese spreadsheet writes it (FR-003). Rules, in
// order: both '.' and ',' → the LAST one is the decimal mark; one kind repeated → thousand
// separators; a single mark followed by exactly three digits after a 1–3 digit non-zero head →
// thousand separator ("1.500" = 1500, "150.000" = 150000); otherwise a decimal mark ("85,5",
// "0.125"). Groups must be three digits. Spaces (NBSP included) are dropped.
// ponytail: "12.345" meaning twelve-point-three-four-five reads as 12345 — the price of VN-first;
// written into specs/061 §7.
func parseHumanNumber(cell string) (float64, bool) {
	s := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, cell)
	if exponentRe.MatchString(s) {
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	sign := ""
	if s != "" && (s[0] == '-' || s[0] == '+') {
		sign, s = s[:1], s[1:]
	}
	dots, commas := strings.Count(s, "."), strings.Count(s, ",")
	intPart, frac := s, ""
	switch {
	case dots > 0 && commas > 0:
		dec, grp := ",", "."
		if strings.LastIndex(s, ".") > strings.LastIndex(s, ",") {
			dec, grp = ".", ","
		}
		if strings.Count(s, dec) != 1 {
			return 0, false
		}
		i := strings.LastIndex(s, dec)
		intPart, frac = s[:i], s[i+1:]
		if !thousandGroups(intPart, grp) {
			return 0, false
		}
		intPart = strings.ReplaceAll(intPart, grp, "")
	case dots > 1 || commas > 1:
		grp := "."
		if commas > 1 {
			grp = ","
		}
		if !thousandGroups(s, grp) {
			return 0, false
		}
		intPart = strings.ReplaceAll(s, grp, "")
	case dots == 1 || commas == 1:
		sep := "."
		if commas == 1 {
			sep = ","
		}
		if thousandGroups(s, sep) {
			intPart = strings.ReplaceAll(s, sep, "")
		} else {
			i := strings.Index(s, sep)
			intPart, frac = s[:i], s[i+1:]
		}
	}
	if !allDigits(intPart) || (frac != "" && !allDigits(frac)) || (frac == "" && strings.ContainsAny(s[len(s)-1:], ".,")) {
		return 0, false
	}
	num := sign + intPart
	if frac != "" {
		num += "." + frac
	}
	f, err := strconv.ParseFloat(num, 64)
	return f, err == nil
}

// thousandGroups reports whether s is a 1–3 digit non-zero head followed by groups of exactly
// three digits, all separated by sep ("3.200.000" with "."). A lone "0" head is a decimal ("0.500").
func thousandGroups(s, sep string) bool {
	parts := strings.Split(s, sep)
	if len(parts) < 2 || len(parts[0]) < 1 || len(parts[0]) > 3 || parts[0][0] == '0' || !allDigits(parts[0]) {
		return false
	}
	for _, p := range parts[1:] {
		if len(p) != 3 || !allDigits(p) {
			return false
		}
	}
	return true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseHumanCurrency reads an amount with an optional currency token (FR-004): an ISO code before
// or after the number ("VND 1.000", "100 usd") or a Vietnamese dong sign ("đ", "₫", "VNĐ"). ok is
// false for anything else. code is "" when the cell names none — the caller supplies the field's.
func parseHumanCurrency(cell string) (amount float64, code string, ok bool) {
	m := amountRe.FindStringSubmatch(strings.TrimSpace(cell))
	if m == nil || (m[1] != "" && m[3] != "") {
		return 0, "", false
	}
	amount, ok = parseHumanNumber(m[2])
	if !ok {
		return 0, "", false
	}
	token := m[1] + m[3]
	switch strings.ToLower(token) {
	case "":
		return amount, "", true
	case "đ", "₫", "vnđ", "vnd":
		return amount, "VND", true
	}
	if len(token) == 3 && allASCIILetters(token) {
		return amount, strings.ToUpper(token), true
	}
	return 0, "", false
}

func allASCIILetters(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i] | 0x20
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return true
}

// humanBools are the yes/no words a Vietnamese sheet uses (FR-005); "x" is a ticked box.
var humanBools = map[string]bool{
	"true": true, "1": true, "yes": true, "y": true, "có": true, "co": true, "đúng": true, "x": true,
	"false": false, "0": false, "no": false, "n": false, "không": false, "khong": false, "sai": false,
}

// parseDayFirst reads a day-first date (FR-006) or, when withTime, a day-first date with an
// optional wall-clock time in Vietnam (FR-007). matched is false when the cell is not day-first at
// all (the caller passes it on untouched — "2026-10-15" is judged by ValidateRecord as before);
// valid is false for a day-first cell naming a day that does not exist ("31/02/2026").
func parseDayFirst(cell string, withTime bool) (out string, matched, valid bool) {
	m := dayFirstRe.FindStringSubmatch(strings.TrimSpace(cell))
	if m == nil || m[2] != m[4] {
		return "", false, false
	}
	if !withTime {
		m[6] = "" // a date column exported with "0:00" still names a day; the time is dropped
	}
	day, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[3])
	year, _ := strconv.Atoi(m[5])
	hour, minute, sec := 0, 0, 0
	if m[6] != "" {
		hour, _ = strconv.Atoi(m[6])
		minute, _ = strconv.Atoi(m[7])
		if m[8] != "" {
			sec, _ = strconv.Atoi(m[8])
		}
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, 0, vnWallClock)
	if t.Day() != day || int(t.Month()) != month || t.Hour() != hour || t.Minute() != minute || t.Second() != sec || year <= 0 {
		return "", true, false
	}
	if withTime {
		return t.Format(time.RFC3339), true, true
	}
	return t.Format("2006-01-02"), true, true
}

// resolveChoice maps a picklist cell to the stored code (FR-001): the exact code first, then a
// case- and space-insensitive match on codes and options.valueLabels. ambiguous = one spelling
// names two codes (never guessed). configured is false when the field declares no values — the
// caller passes the cell on for ValidateRecord to judge, as before.
func resolveChoice(options map[string]any, cell string) (code string, found, ambiguous, configured bool) {
	values, _ := options["values"].([]any)
	if len(values) == 0 {
		return "", false, false, false
	}
	s := strings.TrimSpace(cell)
	for _, v := range values {
		if str, ok := v.(string); ok && str == s {
			return str, true, false, true
		}
	}
	key := strings.ToLower(s)
	labels, _ := options["valueLabels"].(map[string]any)
	for _, v := range values {
		str, ok := v.(string)
		if !ok {
			continue
		}
		label, _ := labels[str].(string)
		if strings.ToLower(strings.TrimSpace(str)) != key && strings.ToLower(strings.TrimSpace(label)) != key {
			continue
		}
		if found && code != str {
			return "", false, true, true
		}
		code, found = str, true
	}
	return code, found, false, true
}

// choiceHint lists up to ten choices as a person reads them (label, else code) for an error.
func choiceHint(options map[string]any) string {
	values, _ := options["values"].([]any)
	labels, _ := options["valueLabels"].(map[string]any)
	out := make([]string, 0, 10)
	for _, v := range values {
		str, ok := v.(string)
		if !ok {
			continue
		}
		if len(out) == 10 {
			out = append(out, "…")
			break
		}
		if label, _ := labels[str].(string); strings.TrimSpace(label) != "" {
			str = label
		}
		out = append(out, str)
	}
	return strings.Join(out, ", ")
}
