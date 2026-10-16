package services

// specs/050 S7 (T097): Doctor C11 (rows still sealed under a retired-but-held key) and C12 (rows no
// held key opens). C12 tries to OPEN each envelope — a label is not evidence of a usable key.

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/crypto"
	"nexus/platform/internal/doctor"
)

func keyChecks(t *testing.T, svc *CredentialService) (c11, c12 doctor.Check) {
	t.Helper()
	s := &DoctorService{}
	s.WithCredentialKeyChecks(svc)
	for _, c := range s.credentialKeyChecks() {
		switch c.ID {
		case DoctorCheckCredentialRetiredKey:
			c11 = c
		case DoctorCheckCredentialUndecryptable:
			c12 = c
		}
	}
	if c11.Detect == nil || c12.Detect == nil {
		t.Fatal("C11/C12 not registered")
	}
	return c11, c12
}

func TestDoctorC11_ReportsRetiredKeyRows(t *testing.T) {
	oldK, newK := resealKey(t), resealKey(t)
	oldRing, _ := crypto.NewKeyring("v1", oldK, nil)
	ring, _ := crypto.NewKeyring("v2", newK, map[string][]byte{"v1": oldK})
	st := newFakeCredentialStore()
	a := sealedRow(t, oldRing, "t1", "a", "x")
	a.Name = "partner-a"
	b := sealedRow(t, ring, "t1", "b", "y")
	b.Name = "partner-b"
	other := sealedRow(t, oldRing, "t2", "o", "z")
	st.rows["a"], st.rows["b"], st.rows["o"] = a, b, other
	c11, c12 := keyChecks(t, NewCredentialService(st, ring, false))

	got, err := c11.Detect(context.Background(), "t1")
	if err != nil || len(got) != 1 || got[0].Key != "a" || !strings.Contains(got[0].Detail, "partner-a") {
		t.Fatalf("C11 = %+v err=%v, want exactly partner-a", got, err)
	}
	if !c11.ReportOnly || c11.AutoFixSafe {
		t.Error("C11 must be report-only")
	}
	if got, _ := c12.Detect(context.Background(), "t1"); len(got) != 0 {
		t.Errorf("C12 = %+v, want none (retired key is still held)", got)
	}
}

func TestDoctorC12_ReportsUndecryptable(t *testing.T) {
	envK, lostK := resealKey(t), resealKey(t)
	lostRing, _ := crypto.NewKeyring("v1", lostK, nil) // prod's "v1"
	ring, _ := crypto.NewKeyring("v1", envK, nil)      // this env's "v1": same label, other key
	st := newFakeCredentialStore()
	bad := sealedRow(t, lostRing, "t1", "bad", "x")
	bad.Name = "stripe-prod"
	good := sealedRow(t, ring, "t1", "good", "y")
	good.Name = "fine"
	st.rows["bad"], st.rows["good"] = bad, good
	_, c12 := keyChecks(t, NewCredentialService(st, ring, false))
	got, err := c12.Detect(context.Background(), "t1")
	if err != nil || len(got) != 1 || got[0].Key != "bad" || !strings.Contains(got[0].Detail, "stripe-prod") {
		t.Fatalf("C12 = %+v err=%v, want exactly stripe-prod", got, err)
	}
	if c12.Severity != doctor.SeverityCritical || !c12.ReportOnly || c12.AutoFixSafe {
		t.Error("C12 must be critical + report-only")
	}

	// Inert vault (no CREDENTIAL_ENC_KEY): every sealed row is unopenable and still reported.
	_, c12 = keyChecks(t, NewCredentialService(st, nil, false))
	if got, _ := c12.Detect(context.Background(), "t1"); len(got) != 2 {
		t.Fatalf("inert vault C12 = %d findings, want 2", len(got))
	}
}
