// doctor_credential_keys.go — specs/050 S7: C11 and C12, the two report-only Doctor checks about the
// vault's master key. Neither fixes anything: C11 clears by itself once the boot reseal reaches the
// row, and C12 is a missing key only the operator can put back.
package services

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/doctor"
)

// DoctorCheckCredentialRetiredKey (C11): rows still sealed under a retired master key.
const DoctorCheckCredentialRetiredKey = "C11_CREDENTIAL_RETIRED_KEY"

// DoctorCheckCredentialUndecryptable (C12): rows no key this server holds can open.
const DoctorCheckCredentialUndecryptable = "C12_CREDENTIAL_UNDECRYPTABLE"

// doctorCredentialKeyHealth is the slice of *CredentialService C11/C12 read.
type doctorCredentialKeyHealth interface {
	KeyHealth(ctx context.Context, tenantID string) (retired, undecryptable []CredentialKeyFinding, err error)
}

// WithCredentialKeyChecks registers C11 and C12 over the vault. Chainable.
func (s *DoctorService) WithCredentialKeyChecks(h doctorCredentialKeyHealth) *DoctorService {
	s.keyHealth = h
	s.runner = doctor.NewRunner(s.checks())
	return s
}

// credentialKeyChecks builds C11 and C12 (empty when unwired).
func (s *DoctorService) credentialKeyChecks() []doctor.Check {
	if s.keyHealth == nil {
		return nil
	}
	detect := func(id string, pick func(retired, bad []CredentialKeyFinding) []CredentialKeyFinding, detail func(CredentialKeyFinding) string) func(context.Context, string) ([]doctor.Finding, error) {
		return func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			retired, bad, err := s.keyHealth.KeyHealth(ctx, tenantID)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.%s detect: %w", id, err)
			}
			out := make([]doctor.Finding, 0)
			for _, f := range pick(retired, bad) {
				if len(out) > doctor.MaxFindingsPerCheck {
					break
				}
				out = append(out, doctor.Finding{Key: f.ID, Detail: detail(f)})
			}
			return out, nil
		}
	}
	return []doctor.Check{
		{
			ID: DoctorCheckCredentialRetiredKey, Severity: doctor.SeverityInfo, ReportOnly: true,
			Detect: detect("C11", func(r, _ []CredentialKeyFinding) []CredentialKeyFinding { return r },
				func(f CredentialKeyFinding) string {
					return fmt.Sprintf("Khoá kết nối %q vẫn niêm phong bằng khoá mã hoá chủ cũ (%s) — chờ lần khởi động kế tiếp niêm phong lại; chưa gỡ khoá cũ khỏi CREDENTIAL_ENC_RETIRED_KEYS.", f.Name, keyLabel(f.KeyID))
				}),
		},
		{
			ID: DoctorCheckCredentialUndecryptable, Severity: doctor.SeverityCritical, ReportOnly: true,
			Detect: detect("C12", func(_, b []CredentialKeyFinding) []CredentialKeyFinding { return b },
				func(f CredentialKeyFinding) string {
					return fmt.Sprintf("Khoá kết nối %q không mở được: máy chủ này không giữ khoá mã hoá chủ %q (hoặc bản ghi hỏng). Quy tắc dùng khoá này sẽ không gửi. Đặt lại đúng khoá vào CREDENTIAL_ENC_KEY / CREDENTIAL_ENC_RETIRED_KEYS, hoặc xoay khoá để nhập bí mật mới.", f.Name, keyLabel(f.KeyID))
				}),
		},
	}
}

// keyLabel is the operator-chosen label of a "<label>.<fingerprint>" key id.
func keyLabel(keyID string) string {
	label, _, _ := strings.Cut(keyID, ".")
	return label
}
