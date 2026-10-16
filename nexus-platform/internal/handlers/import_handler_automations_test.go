package handlers

import (
	"net/http"
	"testing"
)

// specs/048 S7b: "Chạy tự động hoá" arrives as the form field runAutomations; absent = false, so
// a plain import never fires emails/webhooks by surprise.
func TestImportHandler_RunAutomationsFormField(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]string
		want  bool
	}{
		{"absent", nil, false},
		{"true", map[string]string{"runAutomations": "true"}, true},
		{"false", map[string]string{"runAutomations": "false"}, false},
		{"junk is not true", map[string]string{"runAutomations": "yes"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &capturingImportSvc{}
			rec := postImportForm(t, svc, tt.extra)
			if rec.Code != http.StatusOK || svc.in == nil {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if svc.in.RunAutomations != tt.want {
				t.Fatalf("RunAutomations = %v; want %v", svc.in.RunAutomations, tt.want)
			}
		})
	}
}
