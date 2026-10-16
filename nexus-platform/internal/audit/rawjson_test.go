package audit

import (
	"encoding/json"
	"testing"
)

// TestRawJSON_Scan pins the W7 audit-v2 regression: a NULL diff column must scan to nil instead
// of erroring (the pre-fix json.RawMessage field made any page containing a NULL-diff row fail
// the whole query → live 500 AUDIT_QUERY_FAILED). Table-driven per the workspace test rules.
func TestRawJSON_Scan(t *testing.T) {
	tests := []struct {
		name    string
		src     any
		want    string // "" means nil
		wantErr bool
	}{
		{name: "SQL NULL scans to nil (the limit-bug fix)", src: nil, want: ""},
		{name: "bytes copied verbatim", src: []byte(`{"a":1}`), want: `{"a":1}`},
		{name: "string accepted", src: `{"b":2}`, want: `{"b":2}`},
		{name: "unsupported type rejected", src: 42, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r RawJSON
			err := r.Scan(tt.src)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Scan(%T) = nil error; want error", tt.src)
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan(%T): %v", tt.src, err)
			}
			if tt.want == "" && r != nil {
				t.Fatalf("Scan(nil) = %q; want nil", r)
			}
			if tt.want != "" && string(r) != tt.want {
				t.Fatalf("Scan = %q; want %q", r, tt.want)
			}
		})
	}

	// The []byte case must COPY (driver buffers are reused between rows).
	buf := []byte(`{"x":1}`)
	var r RawJSON
	if err := r.Scan(buf); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	buf[2] = 'y'
	if string(r) != `{"x":1}` {
		t.Fatalf("Scan aliased the driver buffer: got %q", r)
	}
}

// TestRawJSON_MarshalJSON proves the HTTP envelope is byte-identical to the old
// json.RawMessage field: nil → null, non-nil → verbatim.
func TestRawJSON_MarshalJSON(t *testing.T) {
	type env struct {
		Diff RawJSON `json:"diff"`
	}
	tests := []struct {
		name string
		diff RawJSON
		want string
	}{
		{name: "nil marshals as null", diff: nil, want: `{"diff":null}`},
		{name: "raw object forwarded verbatim", diff: RawJSON(`{"before":{"a":1}}`), want: `{"diff":{"before":{"a":1}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(env{Diff: tt.diff})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tt.want {
				t.Fatalf("marshal = %s; want %s", b, tt.want)
			}
		})
	}
}
