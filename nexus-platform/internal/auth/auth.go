package auth

import (
	"errors"

	"github.com/gin-gonic/gin"
)

// GetContextTenantID reads the tenant id that upstream middleware (A2) places in the
// gin context after verifying the access token. It is the single read-point so handlers
// never reach for a client-controlled header/query (spec §4: tenant-from-claim).
func GetContextTenantID(c *gin.Context) (string, error) {
	tenantID, exists := c.Get("tenantID")
	if !exists {
		return "", errors.New("auth: tenant ID not found in context")
	}
	id, ok := tenantID.(string)
	if !ok {
		return "", errors.New("auth: tenant ID in context has unexpected type")
	}
	return id, nil
}
