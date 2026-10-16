package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// mkFindings builds n sequential findings.
func mkFindings(n int) []Finding {
	out := make([]Finding, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Finding{Key: fmt.Sprintf("k%03d", i), Detail: "d"})
	}
	return out
}

func TestRunner_Scan(t *testing.T) {
	tests := []struct {
		name          string
		detect        func(context.Context, string) ([]Finding, error)
		wantCount     int
		wantTruncated bool
		wantErr       bool
	}{
		{
			name:      "check sạch trả count 0 + items rỗng (không null)",
			detect:    func(context.Context, string) ([]Finding, error) { return nil, nil },
			wantCount: 0,
		},
		{
			name:      "findings dưới cap giữ nguyên",
			detect:    func(context.Context, string) ([]Finding, error) { return mkFindings(3), nil },
			wantCount: 3,
		},
		{
			name:          "cap+1 findings → truncate về cap + báo truncated",
			detect:        func(context.Context, string) ([]Finding, error) { return mkFindings(MaxFindingsPerCheck + 1), nil },
			wantCount:     MaxFindingsPerCheck,
			wantTruncated: true,
		},
		{
			name:    "detect lỗi → scan fail-loud",
			detect:  func(context.Context, string) ([]Finding, error) { return nil, errors.New("boom") },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRunner([]Check{{ID: "T1", Severity: SeverityInfo, AutoFixSafe: true, Detect: tt.detect}})
			results, err := r.Scan(context.Background(), "tenant-a")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Scan: want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("results = %d; want 1", len(results))
			}
			res := results[0]
			if res.CheckID != "T1" || res.Count != tt.wantCount || res.Truncated != tt.wantTruncated {
				t.Fatalf("result = %+v; want count %d truncated %v", res, tt.wantCount, tt.wantTruncated)
			}
			if res.Items == nil {
				t.Fatalf("Items nil — must marshal as [], never null")
			}
		})
	}
}

func TestRunner_Fix_ValidationBeforeMutation(t *testing.T) {
	mutated := false
	r := NewRunner([]Check{
		{
			ID: "SAFE", AutoFixSafe: true,
			Detect: func(context.Context, string) ([]Finding, error) { return mkFindings(1), nil },
			Fix:    func(context.Context, string, Finding) error { mutated = true; return nil },
		},
		{
			ID: "CONFIRM_ME", AutoFixSafe: false,
			Detect: func(context.Context, string) ([]Finding, error) { return mkFindings(1), nil },
			Fix:    func(context.Context, string, Finding) error { mutated = true; return nil },
		},
	})

	t.Run("checkId lạ → ErrUnknownCheck, KHÔNG chạy fix nào", func(t *testing.T) {
		mutated = false
		_, err := r.Fix(context.Background(), "tenant-a", []string{"SAFE", "NOPE"}, false)
		if !errors.Is(err, ErrUnknownCheck) {
			t.Fatalf("err = %v; want ErrUnknownCheck", err)
		}
		if !strings.Contains(err.Error(), "NOPE") {
			t.Fatalf("err %q phải nêu id lạ", err)
		}
		if mutated {
			t.Fatalf("fix đã chạy dù request bị reject — phải validate TRƯỚC mutation")
		}
	})

	t.Run("needs-confirm không confirm → ErrConfirmRequired, KHÔNG chạy fix nào", func(t *testing.T) {
		mutated = false
		_, err := r.Fix(context.Background(), "tenant-a", []string{"SAFE", "CONFIRM_ME"}, false)
		if !errors.Is(err, ErrConfirmRequired) {
			t.Fatalf("err = %v; want ErrConfirmRequired", err)
		}
		if mutated {
			t.Fatalf("fix đã chạy dù request bị reject")
		}
	})

	t.Run("needs-confirm với confirm=true → chạy", func(t *testing.T) {
		mutated = false
		results, err := r.Fix(context.Background(), "tenant-a", []string{"CONFIRM_ME"}, true)
		if err != nil {
			t.Fatalf("Fix: %v", err)
		}
		if !mutated || results[0].Fixed != 1 {
			t.Fatalf("results = %+v, mutated = %v; want 1 fixed", results, mutated)
		}
	})
}

func TestRunner_Fix_ReportOnlyRefusedEvenWithConfirm(t *testing.T) {
	mutated := false
	r := NewRunner([]Check{
		{
			ID: "SAFE", AutoFixSafe: true,
			Detect: func(context.Context, string) ([]Finding, error) { return mkFindings(1), nil },
			Fix:    func(context.Context, string, Finding) error { mutated = true; return nil },
		},
		{
			// Report-only: no Fix at all — the runner must refuse before ever needing one.
			ID: "REPORT_ME", ReportOnly: true, Severity: SeverityInfo,
			Detect: func(context.Context, string) ([]Finding, error) { return mkFindings(2), nil },
		},
	})

	// Scan carries the wire flag the FE renders ("chỉ báo cáo, sửa tay").
	results, err := r.Scan(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !results[1].ReportOnly || results[0].ReportOnly {
		t.Fatalf("scan reportOnly flags = [%v %v]; want [false true]", results[0].ReportOnly, results[1].ReportOnly)
	}

	// Fix naming a report-only check → whole request refused, even with confirm=true,
	// before any other check's fix runs.
	_, err = r.Fix(context.Background(), "tenant-a", []string{"SAFE", "REPORT_ME"}, true)
	if !errors.Is(err, ErrReportOnly) {
		t.Fatalf("err = %v; want ErrReportOnly", err)
	}
	if !strings.Contains(err.Error(), "REPORT_ME") {
		t.Fatalf("err %q phải nêu id report-only", err)
	}
	if mutated {
		t.Fatalf("fix đã chạy dù request bị reject — phải validate TRƯỚC mutation")
	}
}

func TestRunner_Fix_CountsAndContinuesOnItemFailure(t *testing.T) {
	fixCalls := 0
	r := NewRunner([]Check{{
		ID: "MIX", AutoFixSafe: true,
		Detect: func(context.Context, string) ([]Finding, error) {
			return []Finding{{Key: "ok"}, {Key: "clean"}, {Key: "bad"}, {Key: "ok2"}}, nil
		},
		Fix: func(_ context.Context, _ string, f Finding) error {
			fixCalls++
			switch f.Key {
			case "clean":
				return ErrAlreadyClean
			case "bad":
				return errors.New("db hiccup")
			}
			return nil
		},
	}})
	results, err := r.Fix(context.Background(), "tenant-a", []string{"MIX", "MIX"}, false) // dup id gộp 1
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("dup checkIds phải gộp — results = %d", len(results))
	}
	res := results[0]
	if res.Fixed != 2 || res.Skipped != 1 || res.Failed != 1 {
		t.Fatalf("counts = %+v; want fixed 2 skipped 1 failed 1", res)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "bad") || !strings.Contains(res.Errors[0], "db hiccup") {
		t.Fatalf("Errors = %v; want một entry 'bad: db hiccup'", res.Errors)
	}
	if fixCalls != 4 {
		t.Fatalf("fixCalls = %d; item fail phải CONTINUE, không dừng batch", fixCalls)
	}
}

func TestRunner_Fix_DetectErrorRecordedRunContinues(t *testing.T) {
	r := NewRunner([]Check{
		{
			ID: "BROKEN", AutoFixSafe: true,
			Detect: func(context.Context, string) ([]Finding, error) { return nil, errors.New("table gone") },
		},
		{
			ID: "NEXT", AutoFixSafe: true,
			Detect: func(context.Context, string) ([]Finding, error) { return mkFindings(1), nil },
			Fix:    func(context.Context, string, Finding) error { return nil },
		},
	})
	results, err := r.Fix(context.Background(), "tenant-a", []string{"BROKEN", "NEXT"}, false)
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d; want 2 (run continues past broken check)", len(results))
	}
	if results[0].Failed != 1 || len(results[0].Errors) != 1 {
		t.Fatalf("broken check result = %+v; want detect error recorded", results[0])
	}
	if results[1].Fixed != 1 {
		t.Fatalf("next check result = %+v; want 1 fixed", results[1])
	}
}
