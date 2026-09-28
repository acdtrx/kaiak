//go:build unix

package state

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking flock on f. The kernel releases it when
// the file is closed or the process exits, however it exits, so a crashed gateway
// leaves no stale lock behind. errLocked: another open file holds it.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
