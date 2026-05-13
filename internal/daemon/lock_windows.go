//go:build windows

package daemon

import (
	"os"

	"golang.org/x/sys/windows"
)

// acquireLockHandle takes a non-blocking, exclusive lock via LockFileEx —
// the Windows equivalent of flock(LOCK_EX|LOCK_NB). LOCKFILE_FAIL_IMMEDIATELY
// makes it non-blocking; without it the call would wait until the lock is
// free (or forever).
func acquireLockHandle(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,         // reserved
		1, 0,      // nNumberOfBytesToLockLow, …High — lock 1 byte (just enough)
		&ol,
	)
}

func releaseLockHandle(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
