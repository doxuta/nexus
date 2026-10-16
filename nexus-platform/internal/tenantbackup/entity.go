package tenantbackup

import (
	"encoding/json"
	"time"
)

// Backup kinds / statuses (the CHECK constraints of migration 000086 mirror these).
const (
	KindManual     = "manual"
	KindScheduled  = "scheduled"
	KindPreRestore = "pre_restore"

	StatusRunning = "running"
	StatusReady   = "ready"
	StatusFailed  = "failed"
)

// SystemActorID is the audit actor of everything the schedule does: audit_log.actor_user_id is
// NOT NULL and the UI would show an empty cell for "". Same family as workflow.SystemActorID
// (f1000000-…) and inboundhook.SystemActorID (f2000000-…).
const SystemActorID = "f3000000-0000-0000-0000-000000000000"

// SystemActorLabel is the human label of SystemActorID.
const SystemActorLabel = "Tự sao lưu theo lịch"

// Backup is one row of sys_tenant_backups. It deliberately does NOT declare op_lock: it is a
// generated column and any INSERT/UPDATE that names it fails with MySQL error 3105 — the column is
// read by the database only. created_at / updated_at are `<-:false` so GORM leaves them to the
// column defaults (updated_at is the heartbeat: ON UPDATE CURRENT_TIMESTAMP(3) bumps it).
type Backup struct {
	ID                 string          `gorm:"column:id;primaryKey"`
	TenantID           string          `gorm:"column:tenant_id"`
	Kind               string          `gorm:"column:kind"`
	Status             string          `gorm:"column:status"`
	Note               *string         `gorm:"column:note"`
	StorageKey         *string         `gorm:"column:storage_key"`
	SizeBytes          *int64          `gorm:"column:size_bytes"`
	SHA256             *string         `gorm:"column:sha256"`
	FormatVersion      int             `gorm:"column:format_version"`
	SchemaVersion      int64           `gorm:"column:schema_version"`
	RowCount           *int64          `gorm:"column:row_count"`
	TableCount         *int            `gorm:"column:table_count"`
	FileCount          *int            `gorm:"column:file_count"`
	BlobBytes          *int64          `gorm:"column:blob_bytes"`
	Warnings           json.RawMessage `gorm:"column:warnings;type:json"`
	ErrorCode          *string         `gorm:"column:error_code"`
	ErrorDetail        *string         `gorm:"column:error_detail"`
	ScheduleSlot       *string         `gorm:"column:schedule_slot"`
	RestoreSourceID    *string         `gorm:"column:restore_source_id"`
	RestoreStatus      *string         `gorm:"column:restore_status"`
	RestoreStep        *string         `gorm:"column:restore_step"`
	RestoreCommittedAt *time.Time      `gorm:"column:restore_committed_at"`
	RestoreReport      json.RawMessage `gorm:"column:restore_report;type:json"`
	RestoreFinishedAt  *time.Time      `gorm:"column:restore_finished_at"`
	ActorTenantID      *string         `gorm:"column:actor_tenant_id"`
	CreatedBy          *string         `gorm:"column:created_by"`
	CreatedAt          time.Time       `gorm:"column:created_at;<-:false"`
	UpdatedAt          time.Time       `gorm:"column:updated_at;<-:false"`
	FinishedAt         *time.Time      `gorm:"column:finished_at"`
	DeletedAt          *time.Time      `gorm:"column:deleted_at"`
}

// TableName pins the fixed system table.
func (Backup) TableName() string { return "sys_tenant_backups" }

// Warning is one aggregated warning of a backup: a code, how many files it touched, and up to ten
// sample ids (never a value from a customer row).
type Warning struct {
	Code   string   `json:"code"`
	Count  int      `json:"count"`
	Sample []string `json:"sample,omitempty"`
}

// Warning codes of a backup (FR-013).
const (
	WarnBlobMissing     = "BLOB_MISSING"
	WarnBlobShaMismatch = "BLOB_SHA_MISMATCH"
	WarnBlobNonstandard = "BLOB_PATH_NONSTANDARD"
	WarnBlobForeign     = "BLOB_PATH_FOREIGN"
)

// ErrorView is the JSON form of a failed backup's error: the code and a Vietnamese sentence built
// from the CODE, never from error_detail.
type ErrorView struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PersonView is who made a backup: the operator's name as the workshop knows it.
type PersonView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// BackupView is the camelCase API shape of a backup (plan.md §Hợp đồng API).
type BackupView struct {
	ID            string      `json:"id"`
	TenantID      string      `json:"tenantId"`
	Kind          string      `json:"kind"`
	Status        string      `json:"status"`
	Note          *string     `json:"note"`
	SizeBytes     *int64      `json:"sizeBytes"`
	SHA256        *string     `json:"sha256"`
	SchemaVersion int64       `json:"schemaVersion"`
	FormatVersion int         `json:"formatVersion"`
	RowCount      *int64      `json:"rowCount"`
	TableCount    *int        `json:"tableCount"`
	FileCount     *int        `json:"fileCount"`
	BlobBytes     *int64      `json:"blobBytes"`
	Warnings      []Warning   `json:"warnings"`
	Error         *ErrorView  `json:"error"`
	CreatedAt     time.Time   `json:"createdAt"`
	FinishedAt    *time.Time  `json:"finishedAt"`
	CreatedBy     *PersonView `json:"createdBy"`

	Restorable          bool    `json:"restorable"`
	NotRestorableReason *string `json:"notRestorableReason"`
	// Restore is the restore record of a pre_restore backup (slice D); nil for every other row.
	Restore *RestoreView `json:"restore"`
}

// RestoreView is the restore half of a pre_restore row.
type RestoreView struct {
	SourceBackupID string           `json:"sourceBackupId"`
	Status         string           `json:"status"`
	Step           *string          `json:"step"`
	CommittedAt    *time.Time       `json:"committedAt"`
	Report         *json.RawMessage `json:"report"`
	FinishedAt     *time.Time       `json:"finishedAt"`
}

// errorMessages turns a stored error_code into the sentence the operator reads.
var errorMessages = map[string]string{
	"BACKUP_WRITE_FAILED": "Không ghi được bản sao lưu — xem nhật ký máy chủ.",
	"BACKUP_DISK_FULL":    "Hết dung lượng đĩa khi ghi bản sao lưu.",
	"BACKUP_CANCELLED":    "Bản sao lưu bị dừng do máy chủ tắt.",
	"BACKUP_ABANDONED":    "Bản sao lưu bị bỏ dở (máy chủ dừng hoặc mất nhịp tim quá 30 phút).",
}

func errorView(code *string) *ErrorView {
	if code == nil || *code == "" {
		return nil
	}
	msg, ok := errorMessages[*code]
	if !ok {
		msg = "Bản sao lưu lỗi (" + *code + ")."
	}
	return &ErrorView{Code: *code, Message: msg}
}
