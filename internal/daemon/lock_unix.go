//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// acquireLockHandle takes a non-blocking, exclusive advisory lock on f via
// flock(2). Works on Linux, macOS, FreeBSD, NetBSD, OpenBSD, illumos.
func acquireLockHandle(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func releaseLockHandle(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
