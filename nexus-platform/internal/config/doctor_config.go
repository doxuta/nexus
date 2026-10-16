package config

import "time"

// defaultDoctorScanIntervalSec keeps the Nexus Doctor's scheduled scan OFF by default (0 =
// disabled) — the scheduled scan is an OPT-IN operational feature (spec §4 DOCTOR-OPS): a
// deployment turns it on deliberately (e.g. 86400 for a daily sweep), it is never surprise
// background load.
const defaultDoctorScanIntervalSec = 0

// DoctorConfig holds the scheduled-scan configuration read from the environment. The
// scheduler ticks every ScanInterval (0 = disabled), runs a READ-ONLY doctor scan for every
// active tenant, and notifies each tenant's admins in-app when findings exist. It NEVER
// fixes anything (spec §3: "tự khám định kỳ, người chỉ duyệt").
type DoctorConfig struct {
	ScanInterval time.Duration // 0 = scheduled scan disabled
}

// Enabled reports whether the background doctor scheduler should run.
func (c DoctorConfig) Enabled() bool { return c.ScanInterval > 0 }

// LoadDoctorConfig reads DOCTOR_SCAN_INTERVAL_SEC with degrade-to-default semantics (like
// the AI_* knobs, unlike the fail-fast janitor knobs): an unset, unparsable or non-positive
// value falls back to the default 0 (OFF) — a typo can only leave the OPT-IN feature off,
// never fail boot or invent a cadence.
func LoadDoctorConfig() DoctorConfig {
	sec := getEnvInt("DOCTOR_SCAN_INTERVAL_SEC", defaultDoctorScanIntervalSec)
	return DoctorConfig{ScanInterval: time.Duration(sec) * time.Second}
}
