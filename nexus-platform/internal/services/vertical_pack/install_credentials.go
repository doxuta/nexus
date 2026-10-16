package vertical_pack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/services"
)

// Vault key slots in a pack (specs/050 S6). A pack names the keys its outbound_webhook rules use
// (Pack.Credentials, PackWorkflowAction.CredentialName); it never carries a secret or an id.

// credentialGate refuses, before any write, a pack whose key references cannot be satisfied: a rule
// naming a key the pack does not declare, a declared key with a bad name or auth type, or a pack
// that needs the vault on a server that has none. Install is past rollback by Pass 8, so these are
// decidable-from-the-pack failures that must not wait for it. Dry-run goes through the same gate.
func (s *Service) credentialGate(pack *Pack) error {
	declared := make(map[string]bool, len(pack.Credentials))
	for _, c := range pack.Credentials {
		name := strings.TrimSpace(c.Name)
		if err := credential.ValidateName(name); err != nil {
			return &services.ValidationError{Code: "CREDENTIAL_NAME_INVALID",
				Msg:     fmt.Sprintf("khoá kết nối %q của gói có tên không hợp lệ", name),
				Details: map[string]any{"credential": name}}
		}
		if err := s.validateSlotMetadata(c, name); err != nil {
			return err
		}
		declared[name] = true
	}
	for _, pw := range pack.Workflows {
		for _, a := range pw.Actions {
			if n := strings.TrimSpace(a.CredentialName); n != "" && !declared[n] {
				return &services.ValidationError{Code: "PACK_CREDENTIAL_UNKNOWN",
					Msg:     fmt.Sprintf("quy tắc %q của gói dùng khoá kết nối %q mà gói không khai báo", strings.TrimSpace(pw.Name), n),
					Details: map[string]any{"credential": n, "workflow": pw.Name}}
			}
		}
	}
	if len(declared) > 0 && (s.vault == nil || !s.vault.Configured()) {
		return &services.ValidationError{Code: "CREDENTIALS_NOT_CONFIGURED",
			Msg:     "Gói cần Kho khoá kết nối nhưng máy chủ chưa bật kho này (thiếu khoá mã hoá chủ)",
			Details: map[string]any{}}
	}
	return nil
}

// validateSlotMetadata mirrors CredentialService.validateMetadata's non-secret checks (auth type,
// base URL, header name) so a hand-edited or foreign-env pack fails here, before any write, not at
// vault.Create in Pass 8 (past rollback).
func (s *Service) validateSlotMetadata(c PackCredential, name string) error {
	bad := func(code, msg string) error {
		return &services.ValidationError{Code: code,
			Msg: fmt.Sprintf("khoá kết nối %q của gói: %s", name, msg), Details: map[string]any{"credential": name}}
	}
	at := credential.AuthType(strings.TrimSpace(c.AuthType))
	switch at {
	case credential.AuthHeader, credential.AuthBasic, credential.AuthHMAC, credential.AuthURLToken:
	default:
		return bad("CREDENTIAL_AUTH_TYPE_INVALID", "kiểu xác thực phải là header, basic, hmac hoặc url_token")
	}
	allowHTTP := s.vault != nil && s.vault.AllowsInsecureHTTP()
	if credential.ValidateBaseURL(strings.TrimSpace(c.BaseURL), allowHTTP) != nil {
		return bad("CREDENTIAL_BASE_URL_INVALID", "địa chỉ gốc phải là https (http chỉ cho máy nội bộ), không kèm tài khoản, query hay #")
	}
	if at == credential.AuthURLToken && credential.ValidateURLTokenBase(strings.TrimSpace(c.BaseURL)) != nil {
		return bad("CREDENTIAL_BASE_URL_INVALID", "địa chỉ gốc của khoá trong đường dẫn phải chứa đúng một chỗ "+credential.SecretPlaceholder)
	}
	if at == credential.AuthHeader && strings.TrimSpace(c.HeaderName) != "" && credential.ValidateHeaderName(strings.TrimSpace(c.HeaderName)) != nil {
		return bad("CREDENTIAL_HEADER_INVALID", "tên header không hợp lệ hoặc thuộc nhóm dành riêng")
	}
	return nil
}

// ensureCredentialSlots maps each declared key NAME to the target tenant's key id: a live key of the
// same name is reused; otherwise an empty slot (needs_secret) is created. Every slot that still has
// no secret is said aloud in res.Warnings — a rule on an empty slot sends nothing until the admin
// enters the secret, and a silent half-working install is the failure this campaign exists to stop.
func (s *Service) ensureCredentialSlots(ctx context.Context, tenantID, userID string, pack *Pack, res *InstallResult) (map[string]string, error) {
	if len(pack.Credentials) == 0 || s.vault == nil {
		return nil, nil
	}
	have, err := s.vault.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.Install: list credentials: %w", err)
	}
	byName := make(map[string]services.CredentialView, len(have))
	for _, v := range have {
		byName[v.Name] = v
	}
	ids := make(map[string]string, len(pack.Credentials))
	for _, pc := range pack.Credentials {
		name := strings.TrimSpace(pc.Name)
		if _, done := ids[name]; done {
			continue
		}
		if v, ok := byName[name]; ok {
			ids[name] = v.ID
			if string(v.AuthType) != strings.TrimSpace(pc.AuthType) {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"khoá kết nối %q đã có sẵn ở tổ chức này với kiểu xác thực khác gói (%s, gói dùng %s) — giữ khoá đang có, kiểm tra lại ở Quản trị › Kho khoá kết nối.",
					name, v.AuthType, pc.AuthType))
			}
			if v.BaseURL != strings.TrimSpace(pc.BaseURL) {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"khoá kết nối %q đã có sẵn ở tổ chức này với địa chỉ gốc khác gói (%s, gói dùng %s) — các quy trình của gói sẽ gọi địa chỉ của khoá đang có; kiểm tra lại ở Quản trị › Kho khoá kết nối.",
					name, v.BaseURL, pc.BaseURL))
			}
			if v.Status == credential.StatusNeedsSecret {
				res.Warnings = append(res.Warnings, needsSecretWarning(pc))
			}
			continue
		}
		in := services.CredentialInput{
			Name: name, Label: pc.Label, BaseURL: pc.BaseURL, AuthType: strings.TrimSpace(pc.AuthType), ActorID: userID,
		}
		if pc.AuthType == string(credential.AuthHeader) {
			// A header key keeps the pack's header exactly (empty name/prefix would fall back to
			// Authorization / Bearer, which is not what the author wrote).
			in.HeaderName, in.HeaderPrefix = &pc.HeaderName, &pc.HeaderPrefix
			if pc.HeaderName == "" {
				in.HeaderName = nil
			}
		}
		if pc.Username != "" {
			in.Username = &pc.Username
		}
		v, _, err := s.vault.Create(ctx, tenantID, in)
		if err != nil {
			var ve *services.ValidationError
			if errors.As(err, &ve) {
				return nil, err
			}
			return nil, fmt.Errorf("vertical_pack.Install: create credential %q: %w", name, err)
		}
		ids[name] = v.ID
		res.Warnings = append(res.Warnings, needsSecretWarning(pc))
	}
	return ids, nil
}

func needsSecretWarning(pc PackCredential) string {
	msg := fmt.Sprintf("khoá kết nối %q (%s) chưa có bí mật — Nhập bí mật ở Quản trị › Kho khoá kết nối; tới lúc đó các quy trình dùng khoá này chưa gọi được đối tác.",
		pc.Name, pc.BaseURL)
	if len(pc.ExtraHeaderNames) > 0 {
		msg += " Gói cũng dùng thêm header: " + strings.Join(pc.ExtraHeaderNames, ", ") + "."
	}
	return msg
}
