package config

import (
	"slices"
	"strings"
	"time"
)

// Per-customer backup defaults (specs/068-tenant-backup-restore). Every knob parses
// degrade-to-default like jobs_config.go: a typo restores a default, it never fails boot or zeroes
// a safety margin.
const (
	defaultTenantBackupDir           = "./tenant-backups"
	defaultTenantBackupDownloadSec   = 1800           // 30 min: a multi-GB download outlives the 30 s server write timeout
	defaultTenantRestoreMaxRows      = 300000         // restore-class rows per customer, in the file and live
	defaultTenantBackupMinFreeBytes  = int64(2 << 30) // 2 GiB reserve on top of the estimate (FR-070)
	defaultTenantRestoreTxTimeoutSec = 600
	defaultTenantBackupScheduleSec   = 600 // the schedule ticks every 10 min
	defaultTenantBackupHour          = 2   // 02:00 Asia/Ho_Chi_Minh
	defaultTenantBackupKeepPre       = 5
	defaultTenantBackupFailedDays    = 7
)

// TenantBackupConfig holds the TENANT_BACKUP_* / TENANT_RESTORE_* settings.
type TenantBackupConfig struct {
	// Dir is the LocalBackend root for the zips (TENANT_BACKUP_DIR) — a SECOND backend, separate
	// from the attachment store (FILE_STORAGE_DIR): the two never share a key space.
	Dir string
	// DownloadDeadline (TENANT_BACKUP_DOWNLOAD_DEADLINE_SEC) is the per-request write deadline of
	// the file download, lifted above the server-wide 30 s WriteTimeout.
	DownloadDeadline time.Duration
	// RestoreMaxRows (TENANT_RESTORE_MAX_ROWS) caps the restore-class rows of one customer.
	RestoreMaxRows int
	// MinFreeBytes (TENANT_BACKUP_MIN_FREE_BYTES) is the free-disk reserve a backup must leave.
	MinFreeBytes int64
	// RestoreTxTimeout (TENANT_RESTORE_TX_TIMEOUT_SEC) bounds the restore's data transaction.
	RestoreTxTimeout time.Duration
	// ScheduleEnabled (TENANT_BACKUP_SCHEDULE_ENABLED, default true) turns the nightly / after-deploy
	// backup ticker on; "false", "0" or "off" turns it off.
	ScheduleEnabled bool
	// ScheduleInterval (TENANT_BACKUP_SCHEDULE_INTERVAL_SEC) is the tick period; tests set 60.
	ScheduleInterval time.Duration
	// HourLocal (TENANT_BACKUP_HOUR_LOCAL, 0..23, Asia/Ho_Chi_Minh) is when the daily backup becomes due.
	HourLocal int
	// KeepPreRestore (TENANT_BACKUP_KEEP_PRE_RESTORE) is how many pre-restore backups stay per customer.
	KeepPreRestore int
	// FailedRetentionDays (TENANT_BACKUP_FAILED_RETENTION_DAYS) is how long a failed backup row stays.
	FailedRetentionDays int
}

// LoadTenantBackupConfig reads the settings from the environment.
func LoadTenantBackupConfig() TenantBackupConfig {
	return TenantBackupConfig{
		Dir:              getEnv("TENANT_BACKUP_DIR", defaultTenantBackupDir),
		DownloadDeadline: time.Duration(getEnvInt("TENANT_BACKUP_DOWNLOAD_DEADLINE_SEC", defaultTenantBackupDownloadSec)) * time.Second,
		RestoreMaxRows:   getEnvInt("TENANT_RESTORE_MAX_ROWS", defaultTenantRestoreMaxRows),
		MinFreeBytes:     getEnvInt64("TENANT_BACKUP_MIN_FREE_BYTES", defaultTenantBackupMinFreeBytes),
		RestoreTxTimeout: time.Duration(getEnvInt("TENANT_RESTORE_TX_TIMEOUT_SEC", defaultTenantRestoreTxTimeoutSec)) * time.Second,

		ScheduleEnabled:     !slices.Contains([]string{"false", "0", "off"}, strings.ToLower(strings.TrimSpace(getEnv("TENANT_BACKUP_SCHEDULE_ENABLED", "true")))),
		ScheduleInterval:    time.Duration(getEnvInt("TENANT_BACKUP_SCHEDULE_INTERVAL_SEC", defaultTenantBackupScheduleSec)) * time.Second,
		HourLocal:           hourOrDefault(int(getEnvInt64ZeroAllowed("TENANT_BACKUP_HOUR_LOCAL", defaultTenantBackupHour))), // 0 = midnight is meaningful
		KeepPreRestore:      posOrDefault(getEnvInt("TENANT_BACKUP_KEEP_PRE_RESTORE", defaultTenantBackupKeepPre), defaultTenantBackupKeepPre),
		FailedRetentionDays: posOrDefault(getEnvInt("TENANT_BACKUP_FAILED_RETENTION_DAYS", defaultTenantBackupFailedDays), defaultTenantBackupFailedDays),
	}
}

// hourOrDefault keeps a typo from scheduling backups at an hour that never comes.
func hourOrDefault(h int) int {
	if h < 0 || h > 23 {
		return defaultTenantBackupHour
	}
	return h
}

func posOrDefault(n, def int) int {
	if n < 1 {
		return def
	}
	return n
}
