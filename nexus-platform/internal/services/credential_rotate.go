package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/credential"
)

// Key rotation — the vault's only door for changing a secret, the extra headers or the host of a
// stored key (../specs/050-named-credentials S4, plan D-12/D-13). The new material is on the very
// next send: ResolveAuth reads the row on every send and keeps no cache.

const (
	// credentialDefaultGraceHours: how long the previous hmac key keeps signing V2-Previous when the
	// caller does not say — long enough for a partner to deploy the new key.
	credentialDefaultGraceHours = 24
	// credentialMaxGraceHours bounds an explicit grace to one week.
	credentialMaxGraceHours = 168
)

// CredentialRotation is the POST /admin/credentials/:id/rotate payload. Pointer / nil fields
// distinguish "absent" from "empty": ExtraHeaders nil keeps the stored set, an empty map clears it.
type CredentialRotation struct {
	Secret         *string           // new secret; "" counts as absent
	GenerateSecret bool              // hmac only: Nexus mints the key and returns it ONCE
	ExtraHeaders   map[string]string // present ⇒ replaces the whole set; absent ⇒ kept (dropped on a host change)
	GraceHours     *int              // hmac only, 0–168, default 24 (a host change keeps no grace)
	BaseURL        *string           // a different scheme/host/port is allowed here, and only here
	Username       *string           // basic only
	ActorID        string
}

// Rotate replaces the sealed material of a key inside one SELECT … FOR UPDATE transaction: it opens
// what must be kept, seals everything under the ACTIVE master key (so key_id stays true for the
// whole row), and — for an hmac key with a grace — keeps the old key sealed as the previous one.
// It returns the view and the Nexus-generated secret ("" unless GenerateSecret) — the only secret
// that ever leaves the vault, returned here once.
func (s *CredentialService) Rotate(ctx context.Context, tenantID, id string, in CredentialRotation) (*CredentialView, string, error) {
	if err := s.configured(); err != nil {
		return nil, "", err
	}
	id = strings.TrimSpace(id)
	var before CredentialView
	var generated string
	err := s.repo.WithTxForUpdate(ctx, tenantID, id, func(c *credential.Credential) error {
		before = s.view(c, 0)
		var err error
		generated, err = s.applyRotation(c, in)
		return err
	})
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		return nil, "", err
	case errors.Is(err, credential.ErrNotFound):
		return nil, "", credentialNotFound()
	case err != nil:
		return nil, "", fmt.Errorf("services.CredentialService.Rotate: %w", err)
	}
	// The after-image is the row as stored (updated_at comes from the database).
	saved, err := s.load(ctx, tenantID, id)
	if err != nil {
		return nil, "", err
	}
	usages, err := s.repo.Usages(ctx, tenantID, id)
	if err != nil {
		return nil, "", fmt.Errorf("services.CredentialService.Rotate: %w", err)
	}
	// usageCount is derived, not stored, and a rotation does not change it: keep it out of the audit
	// diff (same as Update) and put it on the returned view only.
	after := s.view(saved, 0)
	emitMetaAuditFor(ctx, s.auditor, tenantID, audit.NamedCredentialMeta, id, audit.ActionRotate, before, after)
	after.UsageCount = ruleCount(usages)
	s.logger.Info("services.CredentialService: credential rotated",
		zap.String("tenant_id", tenantID), zap.String("credential_id", id), zap.String("auth_type", string(saved.AuthType)),
		zap.String("status", string(after.Status)), zap.Bool("generated", generated != ""), zap.String("user_id", in.ActorID))
	return &after, generated, nil
}

// applyRotation edits c (the locked row) in place and returns the generated secret, if any. The
// caller rolls the transaction back on any error, so a refusal never leaves a half-replaced row.
func (s *CredentialService) applyRotation(c *credential.Credential, in CredentialRotation) (string, error) {
	now := s.now()
	secret, generated := "", ""
	if in.Secret != nil {
		secret = *in.Secret
	}
	if in.GenerateSecret {
		if c.AuthType != credential.AuthHMAC {
			return "", invalidField("CREDENTIAL_GENERATE_UNSUPPORTED", "generateSecret", "Chỉ khoá HMAC mới để Nexus tự sinh")
		}
		if secret != "" {
			return "", invalidField("CREDENTIAL_GENERATE_UNSUPPORTED", "generateSecret", "Chọn một: tự nhập khoá hoặc để Nexus tự sinh")
		}
		var err error
		if generated, err = newCredentialSecret(); err != nil {
			return "", fmt.Errorf("generate secret: %w", err)
		}
		secret = generated
	}
	hasSecret, extraSent := secret != "", in.ExtraHeaders != nil
	if !hasSecret && !extraSent {
		return "", invalidField("CREDENTIAL_SECRET_REQUIRED", "secret", "Xoay khoá cần ít nhất một thứ để thay: khoá mới, tự sinh khoá, hoặc header bổ sung")
	}
	if g := in.GraceHours; g != nil && (c.AuthType != credential.AuthHMAC || *g < 0 || *g > credentialMaxGraceHours) {
		return "", invalidField("CREDENTIAL_GRACE_INVALID", "graceHours", "Thời gian ân hạn chỉ dành cho khoá HMAC, từ 0 đến %d giờ", credentialMaxGraceHours)
	}

	if in.GraceHours != nil && !hasSecret {
		return "", invalidField("CREDENTIAL_GRACE_INVALID", "graceHours", "Ân hạn chỉ đi kèm khoá mới — không có khoá mới để giữ khoá cũ làm khoá ân hạn")
	}

	oldBase := c.BaseURL
	if in.BaseURL != nil {
		c.BaseURL = strings.TrimSpace(*in.BaseURL)
	}
	if in.Username != nil && c.AuthType == credential.AuthBasic {
		c.Username = strings.TrimSpace(*in.Username)
	}
	if err := s.validateMetadata(c, nil, nil, in.Username); err != nil {
		return "", err
	}
	moved := !credential.SameOrigin(oldBase, c.BaseURL)
	if moved && !hasSecret {
		return "", invalidField("CREDENTIAL_SECRET_REQUIRED", "secret", "Đổi sang máy chủ khác phải kèm khoá mới — khoá cũ không được gửi sang máy chủ mới")
	}
	if moved && in.GraceHours != nil && *in.GraceHours > 0 {
		return "", invalidField("CREDENTIAL_GRACE_INVALID", "graceHours", "Đổi sang máy chủ khác không có ân hạn — khoá cũ không được gửi sang máy chủ mới")
	}
	if err := validateCredentialSecret(c, secret, false); err != nil {
		return "", err
	}

	// What survives the rotation is opened from the old row; an unopenable envelope fails the whole
	// rotation, nothing is half-replaced.
	var extra map[string]string
	switch {
	case extraSent:
		extra = in.ExtraHeaders
		if err := validateExtraHeaders(c, extra); err != nil {
			return "", err
		}
	case moved: // headers sealed for the old host stay behind
	case c.ExtraHeadersEnc != nil:
		raw, err := s.openForRotate(c, *c.ExtraHeadersEnc, credential.SlotExtraHeaders)
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(raw, &extra); err != nil {
			return "", s.rotateUnavailable(c, credential.SlotExtraHeaders, err)
		}
	}
	prev, prevExpires := "", (*time.Time)(nil)
	switch {
	case hasSecret:
		grace := credentialDefaultGraceHours
		if in.GraceHours != nil {
			grace = *in.GraceHours
		}
		if c.AuthType == credential.AuthHMAC && c.SecretEnc != nil && !moved && grace > 0 {
			old, err := s.openForRotate(c, *c.SecretEnc, credential.SlotSecret)
			if err != nil {
				return "", err
			}
			exp := now.Add(time.Duration(grace) * time.Hour)
			prev, prevExpires = string(old), &exp
		}
	case c.SecretEnc != nil:
		kept, err := s.openForRotate(c, *c.SecretEnc, credential.SlotSecret)
		if err != nil {
			return "", err
		}
		secret = string(kept)
		if c.PrevSecretEnc != nil && c.PrevExpiresAt != nil && c.PrevExpiresAt.After(now) {
			old, err := s.openForRotate(c, *c.PrevSecretEnc, credential.SlotPrevSecret)
			if err != nil {
				return "", err
			}
			prev, prevExpires = string(old), c.PrevExpiresAt
		}
	}

	// Re-seal all of it under the active master key, each envelope bound to its own slot.
	c.SecretEnc, c.SecretHint, c.PrevSecretEnc, c.PrevExpiresAt = nil, "", nil, nil
	c.ExtraHeadersEnc, c.ExtraHeaderNames, c.KeyID = nil, nil, nil
	if err := s.seal(c, secret, extra); err != nil {
		return "", err
	}
	if prev != "" {
		env, err := s.keys.SealAAD([]byte(prev), credential.AAD(c.TenantID, c.ID, credential.SlotPrevSecret))
		if err != nil {
			return "", fmt.Errorf("seal previous secret: %w", err)
		}
		c.PrevSecretEnc, c.PrevExpiresAt = &env, prevExpires
	}
	c.RotatedAt, c.UpdatedBy = &now, in.ActorID
	return generated, nil
}

// openForRotate opens one envelope of the row being rotated; a failure (master key absent, AAD
// mismatch, tampering) is CREDENTIAL_KEY_UNAVAILABLE with the way out, the cause only logged.
func (s *CredentialService) openForRotate(c *credential.Credential, env string, slot credential.Slot) ([]byte, error) {
	pt, err := s.keys.OpenAAD(env, credential.AAD(c.TenantID, c.ID, slot))
	if err != nil {
		return nil, s.rotateUnavailable(c, slot, err)
	}
	return pt, nil
}

func (s *CredentialService) rotateUnavailable(c *credential.Credential, slot credential.Slot, cause error) error {
	s.logger.Warn("services.CredentialService: rotation cannot open the stored key",
		zap.String("tenant_id", c.TenantID), zap.String("credential_id", c.ID), zap.String("slot", string(slot)), zap.Error(cause))
	return &ValidationError{
		Code: "CREDENTIAL_KEY_UNAVAILABLE",
		Msg: "Không mở được khoá đang lưu (khoá mã hoá của máy chủ không khớp) — nhập lại khoá mới kèm ân hạn 0 giờ " +
			"và đặt lại header bổ sung (gửi extraHeaders, có thể rỗng)",
		Details: map[string]any{"field": string(slot)},
	}
}
