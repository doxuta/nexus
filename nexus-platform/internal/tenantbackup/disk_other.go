//go:build !unix

package tenantbackup

import "errors"

// diskFreeBytes has no portable implementation off unix; the check is then skipped (the service
// treats an error as "unknown free space" and logs it).
func diskFreeBytes(dir string) (uint64, error) {
	return 0, errors.New("tenantbackup.diskFreeBytes: unsupported platform")
}
