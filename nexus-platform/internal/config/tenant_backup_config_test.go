package config

import (
	"testing"
	"time"
)

func TestLoadTenantBackupConfig(t *testing.T) {
	defaults := TenantBackupConfig{Dir: "./tenant-backups", DownloadDeadline: 30 * time.Minute, RestoreMaxRows: 300000,
		MinFreeBytes: 2 << 30, RestoreTxTimeout: 10 * time.Minute,
		ScheduleEnabled: true, ScheduleInterval: 10 * time.Minute, HourLocal: 2, KeepPreRestore: 5, FailedRetentionDays: 7}
	tests := []struct {
		name string
		env  map[string]string
		want TenantBackupConfig
	}{
		{name: "defaults", env: map[string]string{}, want: defaults},
		{
			name: "overrides",
			env: map[string]string{"TENANT_BACKUP_DIR": "/data/tb", "TENANT_BACKUP_DOWNLOAD_DEADLINE_SEC": "60",
				"TENANT_RESTORE_MAX_ROWS": "10", "TENANT_BACKUP_MIN_FREE_BYTES": "1024", "TENANT_RESTORE_TX_TIMEOUT_SEC": "5",
				"TENANT_BACKUP_SCHEDULE_ENABLED": "off", "TENANT_BACKUP_SCHEDULE_INTERVAL_SEC": "60", "TENANT_BACKUP_HOUR_LOCAL": "0",
				"TENANT_BACKUP_KEEP_PRE_RESTORE": "3", "TENANT_BACKUP_FAILED_RETENTION_DAYS": "14"},
			want: TenantBackupConfig{Dir: "/data/tb", DownloadDeadline: time.Minute, RestoreMaxRows: 10,
				MinFreeBytes: 1024, RestoreTxTimeout: 5 * time.Second,
				ScheduleEnabled: false, ScheduleInterval: time.Minute, HourLocal: 0, KeepPreRestore: 3, FailedRetentionDays: 14},
		},
		{
			name: "typos and non-positive values degrade to the defaults",
			env: map[string]string{"TENANT_BACKUP_DOWNLOAD_DEADLINE_SEC": "abc", "TENANT_RESTORE_MAX_ROWS": "0",
				"TENANT_BACKUP_MIN_FREE_BYTES": "-5", "TENANT_RESTORE_TX_TIMEOUT_SEC": "x",
				"TENANT_BACKUP_SCHEDULE_ENABLED": "maybe", "TENANT_BACKUP_HOUR_LOCAL": "25", "TENANT_BACKUP_KEEP_PRE_RESTORE": "-1"},
			want: defaults,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"TENANT_BACKUP_DIR", "TENANT_BACKUP_DOWNLOAD_DEADLINE_SEC", "TENANT_RESTORE_MAX_ROWS",
				"TENANT_BACKUP_MIN_FREE_BYTES", "TENANT_RESTORE_TX_TIMEOUT_SEC", "TENANT_BACKUP_SCHEDULE_ENABLED",
				"TENANT_BACKUP_SCHEDULE_INTERVAL_SEC", "TENANT_BACKUP_HOUR_LOCAL", "TENANT_BACKUP_KEEP_PRE_RESTORE",
				"TENANT_BACKUP_FAILED_RETENTION_DAYS"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := LoadTenantBackupConfig(); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
