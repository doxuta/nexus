package auth

import "time"

// APIKey is one machine-identity credential row (sys_api_keys, migration 000045 — APIKEY-1).
// A key authenticates a SERVER as a (tenant_id, role_id) principal: the composite verifier maps
// a presented `nxk_...` Bearer credential to Claims{TenantID, RoleID, IsAdmin:false}, so the key
// inherits the tenant's existing per-action ACL / field-ACL / row-scope stack unchanged.
//
// Secret handling mirrors RefreshToken / SSOHandoffToken: only the SHA-256 hex of the full raw
// key is persisted (KeyHash, `json:"-"` so no handler can ever serialize it); the raw key is
// returned ONCE at creation. KeyPrefix is the public identification slice shown in the admin
// list — deliberately NOT part of the secret's effective entropy budget (see services.newSecret).
type APIKey struct {
	ID        string `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID  string `gorm:"type:char(36)" json:"tenantId"`
	RoleID    string `gorm:"type:char(36)" json:"roleId"`
	Name      string `gorm:"type:varchar(100)" json:"name"`
	KeyPrefix string `gorm:"type:varchar(16)" json:"keyPrefix"`
	// KeyHash is the SHA-256 hex digest of the full raw key. NEVER serialized (json:"-") and
	// never returned by any handler — the unique index uq_sys_api_keys_hash rides on it.
	KeyHash    string     `gorm:"type:char(64);uniqueIndex:uq_sys_api_keys_hash" json:"-"`
	CreatedBy  string     `gorm:"type:char(36)" json:"createdBy,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// TableName pins the fixed system table.
func (APIKey) TableName() string { return "sys_api_keys" }
