package services

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/crypto"
)

// Master-key rotation (../specs/050-named-credentials S7, plan D-15). An operator makes the new key
// the active one and moves the old one to CREDENTIAL_ENC_RETIRED_KEYS; on boot ResealCredentials
// re-seals every row still sealed under a retired key, so the retired key can then be dropped.

// credentialResealStore is the persistence surface of the reseal — satisfied by
// *credential.Repository. TenantIDsSealedUnderOtherKey is the one system-level read (which tenants
// still have work); every other call is pinned to one tenant.
type credentialResealStore interface {
	TenantIDsSealedUnderOtherKey(ctx context.Context, activeKeyID string) ([]string, error)
	ListSealedUnderOtherKey(ctx context.Context, tenantID, activeKeyID string) ([]credential.Credential, error)
	// Reseal writes c's envelopes and key id only while the row still carries key_id = fromKeyID
	// (compare-and-swap); false = the row moved on (a rotation won) and nothing was written.
	Reseal(ctx context.Context, tenantID, fromKeyID string, c *credential.Credential) (bool, error)
}

// ResealCredentials re-seals, tenant by tenant, every live row sealed under a held-but-not-active
// key. A row that cannot be opened (its key is not held, or the envelope is damaged) is skipped
// and logged — Doctor C12 reports it; nothing is ever written for it. Returns the rows re-sealed.
func ResealCredentials(ctx context.Context, store credentialResealStore, keys *crypto.Keyring, logger *zap.Logger) (int, error) {
	if keys == nil {
		return 0, nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	active := keys.ActiveID()
	tenants, err := store.TenantIDsSealedUnderOtherKey(ctx, active)
	if err != nil {
		return 0, fmt.Errorf("services.ResealCredentials: %w", err)
	}
	total := 0
	var errs []error // one tenant's DB error must not strand the tenants queued after it
	for _, tenantID := range tenants {
		rows, err := store.ListSealedUnderOtherKey(ctx, tenantID, active)
		if err != nil {
			logger.Error("services.ResealCredentials: list failed — tenant skipped", zap.String("tenant_id", tenantID), zap.Error(err))
			errs = append(errs, fmt.Errorf("tenant %s: %w", tenantID, err))
			continue
		}
		done := 0
		for i := range rows {
			c := rows[i]
			from := *c.KeyID
			if !keys.Has(from) {
				logger.Warn("services.ResealCredentials: row sealed under a key this server does not hold — left as is",
					zap.String("tenant_id", tenantID), zap.String("credential_id", c.ID))
				continue
			}
			if err := resealRow(keys, &c); err != nil {
				logger.Warn("services.ResealCredentials: row cannot be re-sealed — left as is",
					zap.String("tenant_id", tenantID), zap.String("credential_id", c.ID), zap.Error(err))
				continue
			}
			ok, err := store.Reseal(ctx, tenantID, from, &c)
			if err != nil {
				logger.Error("services.ResealCredentials: reseal write failed — tenant skipped",
					zap.String("tenant_id", tenantID), zap.String("credential_id", c.ID), zap.Error(err))
				errs = append(errs, fmt.Errorf("tenant %s credential %s: %w", tenantID, c.ID, err))
				break
			}
			if ok {
				done++
			}
		}
		total += done
		logger.Info("services.ResealCredentials: reseal done",
			zap.String("tenant_id", tenantID), zap.Int("resealed", done), zap.Int("candidates", len(rows)))
	}
	if err := errors.Join(errs...); err != nil {
		return total, fmt.Errorf("services.ResealCredentials: %w", err)
	}
	return total, nil
}

// resealRow opens every envelope of c and seals it again under the active key, in place. All three
// slots succeed or c is left unusable to the caller (it is then not written).
func resealRow(keys *crypto.Keyring, c *credential.Credential) error {
	slots := []struct {
		slot credential.Slot
		env  **string
	}{
		{credential.SlotSecret, &c.SecretEnc},
		{credential.SlotPrevSecret, &c.PrevSecretEnc},
		{credential.SlotExtraHeaders, &c.ExtraHeadersEnc},
	}
	next := make([]*string, len(slots))
	for i, s := range slots {
		cur := *s.env
		if cur == nil {
			continue
		}
		aad := credential.AAD(c.TenantID, c.ID, s.slot)
		pt, err := keys.OpenAAD(*cur, aad)
		if err != nil {
			return fmt.Errorf("open %s: %w", s.slot, err)
		}
		env, err := keys.SealAAD(pt, aad)
		if err != nil {
			return fmt.Errorf("seal %s: %w", s.slot, err)
		}
		next[i] = &env
	}
	for i, s := range slots {
		if next[i] != nil {
			*s.env = next[i]
		}
	}
	id := keys.ActiveID()
	c.KeyID = &id
	return nil
}

// CredentialKeyFinding names one vault row for Doctor C11/C12 (never a secret).
type CredentialKeyFinding struct {
	ID, Name, KeyID string
}

// KeyHealth sorts the tenant's live rows by what the keyring can do with them: Retired = sealed
// under a held key that is no longer the active one (the reseal has not reached it yet);
// Undecryptable = at least one envelope does not open (key not held — including the same label on
// a different key — or a damaged envelope). Opening each envelope is the evidence; the key id on
// the row alone is not. A nil keyring (vault inert) holds nothing, so every sealed row is
// undecryptable.
func (s *CredentialService) KeyHealth(ctx context.Context, tenantID string) (retired, undecryptable []CredentialKeyFinding, err error) {
	rows, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("services.CredentialService.KeyHealth: %w", err)
	}
	for i := range rows {
		c := &rows[i]
		if c.KeyID == nil || *c.KeyID == "" {
			continue
		}
		f := CredentialKeyFinding{ID: c.ID, Name: c.Name, KeyID: *c.KeyID}
		if !openAllSlots(s.keys, c) {
			undecryptable = append(undecryptable, f)
			continue
		}
		if *c.KeyID != s.keys.ActiveID() {
			retired = append(retired, f)
		}
	}
	return retired, undecryptable, nil
}

// openAllSlots reports whether every envelope of c opens under keys (a nil keyring opens nothing).
func openAllSlots(keys *crypto.Keyring, c *credential.Credential) bool {
	for _, s := range []struct {
		env  *string
		slot credential.Slot
	}{{c.SecretEnc, credential.SlotSecret}, {c.PrevSecretEnc, credential.SlotPrevSecret}, {c.ExtraHeadersEnc, credential.SlotExtraHeaders}} {
		if s.env == nil {
			continue
		}
		if _, err := keys.OpenAAD(*s.env, credential.AAD(c.TenantID, c.ID, s.slot)); err != nil {
			return false
		}
	}
	return true
}
