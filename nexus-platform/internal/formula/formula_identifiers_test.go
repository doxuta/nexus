package formula

import "testing"

func TestIdentifiers(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		want    []string
		wantErr bool
	}{
		{name: "arithmetic", expr: "amount * probability / 100", want: []string{"amount", "probability"}},
		{name: "inside IF", expr: "IF(qty > 1, amount, cost)", want: []string{"qty", "amount", "cost"}},
		{name: "no field", expr: "1 + 2", want: nil},
		{name: "syntax error", expr: "amount *", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Identifiers(tt.expr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Identifiers(%q) err = %v, wantErr %v", tt.expr, err, tt.wantErr)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Identifiers(%q) = %v, want %v", tt.expr, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Identifiers(%q) = %v, want %v", tt.expr, got, tt.want)
				}
			}
		})
	}
}
