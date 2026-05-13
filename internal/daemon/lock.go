package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Lock struct {
	path string
	file *os.File
}

// AcquireLock takes an exclusive, non-blocking lock on path. The platform-
// specific locking primitive lives in lock_unix.go (flock) / lock_windows.go
// (LockFileEx) — same intent, same failure mode (already-held → error).
func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := acquireLockHandle(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon already running (lock held on %s)", path)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{path: path, file: f}, nil
}

func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = releaseLockHandle(l.file)
	_ = l.file.Close()
	_ = os.Remove(l.path)
}

func ReadPid(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(data))
}

func IsListening(addr string, timeout time.Duration) bool {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func WaitUntilListening(addr string, total time.Duration) error {
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		if IsListening(addr, 200*time.Millisecond) {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return errors.New("timed out waiting for daemon to listen on " + addr)
}

func DefaultPaths() (lockPath, statePath, tokenPath string, err error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", "", "", err
	}
	dir := filepath.Join(cfg, "remote-shell-mcp")
	return filepath.Join(dir, "daemon.lock"),
		filepath.Join(dir, "state.json"),
		filepath.Join(dir, "daemon.token"), nil
}
