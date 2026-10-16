// doctor_currency_check.go — queue #63: D2, the report-only sweep for live records whose stored
// currency code differs from the field's configured options.currencyCode
// (../specs/008-nexus-doctor/spec.md §2.2).
//
// Such rows predate the Q7 write guard (queue #7) or were made by editing a field's configured code
// while it already held data, before queue #157 refused that edit (guardCurrencyCodeChange —
// 422 FIELD_CURRENCY_CODE_IN_USE). Every money reader (aggregate, rollup, report) sums
// their `.amount` and labels the total with the configured code, so each one is a silently wrong
// total. There is NO automated fix by design (Q7): relabelling USD 100 as VND 100 is the very error
// being reported, and there is no FX source to convert with. The fix is a human one — retype the
// amount in the configured currency (the write path keeps a same-amount resend from relabelling,
// see keepLegacyCurrencyValues) or correct the data at its source.
package services

import (
	"context"
	"fmt"
	"sort"

	"nexus/platform/internal/doctor"
)

// DoctorCheckCurrencyCodeMismatch is the D2 check id (D = record data vs. what the metadata
// promises about it; D1 is the pivot reconcile).
const DoctorCheckCurrencyCodeMismatch = "D2_CURRENCY_CODE_MISMATCH"

func (s *DoctorService) checkCurrencyCodeMismatch() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckCurrencyCodeMismatch,
		Severity:    doctor.SeverityWarn, // money totals over the field are wrong until a human fixes the rows
		AutoFixSafe: false,
		ReportOnly:  true, // Q7: never relabel, never convert — the fix is a human decision
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			ents, err := s.store.ListActiveEntities(ctx, tenantID, doctorSchemaScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.D2 detect: %w", err)
			}
			tables := make([]string, 0, len(ents))
			for _, e := range ents {
				if t, ok := objTableOf(e.Name); ok {
					tables = append(tables, t)
				}
			}
			exists, err := s.store.ListExistingTables(ctx, tables)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.D2 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0)
			for _, e := range ents {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				if t, ok := objTableOf(e.Name); !ok || !exists[t] {
					continue // table gone — A6's class; querying it would fail the whole scan
				}
				fields, err := s.meta.ListFields(ctx, tenantID, e.ID)
				if err != nil {
					return nil, fmt.Errorf("services.Doctor.D2 detect: list fields %s: %w", e.Name, err)
				}
				for _, f := range fields {
					opts, err := parseOptionsJSON(f.Options)
					if err != nil {
						continue // unreadable options: no configured code to compare against
					}
					codes := configuredCurrencyCodes(f.Name, f.FieldType, opts)
					paths := make([]string, 0, len(codes))
					for p := range codes {
						paths = append(paths, p)
					}
					sort.Strings(paths) // stable finding order across scans
					for _, path := range paths {
						if len(findings) > doctor.MaxFindingsPerCheck {
							break
						}
						want := codes[path]
						rows, err := s.schema.ListCurrencyCodeMismatches(ctx, e.Name, tenantID, path, want,
							doctor.MaxFindingsPerCheck+1-len(findings))
						if err != nil {
							return nil, fmt.Errorf("services.Doctor.D2 detect: %s.%s: %w", e.Name, path, err)
						}
						for _, row := range rows {
							findings = append(findings, doctor.Finding{
								Key: e.Name + "." + path + ":" + row.ID,
								Detail: fmt.Sprintf("bản ghi %s của %s.%s mang mã tiền tệ %q nhưng trường cấu hình %q — tổng và báo cáo đang cộng số tiền này như %s. Không sửa tự động (không đổi nhãn, không quy đổi): gõ lại số tiền theo %s hoặc sửa dữ liệu gốc",
									row.ID, e.Name, path, row.Code, want, want, want),
							})
						}
					}
				}
			}
			return findings, nil
		},
	}
}
