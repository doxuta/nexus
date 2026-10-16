//go:build unix

package tenantbackup

import (
	"fmt"
	"syscall"
)

// diskFreeBytes is the space an unprivileged process can still write on the filesystem holding dir.
func diskFreeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("tenantbackup.diskFreeBytes %s: %w", dir, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil // #nosec G115 -- block counts are non-negative
}
