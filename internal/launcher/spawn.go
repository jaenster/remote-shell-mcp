package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/daemon"
)

func EnsureDaemon(addr, daemonBinary string, extraArgs []string) error {
	// Use a finite probe so non-default unreachable addresses don't block
	// indefinitely on connect() to RFC1918 / off-network hosts.
	if daemon.IsListening(addr, 200*time.Millisecond) {
		return nil
	}
	bin, err := resolveDaemonBinary(daemonBinary)
	if err != nil {
		return err
	}
	args := append([]string{"-addr", addr}, extraArgs...)
	cmd := exec.Command(bin, args...)
	devNull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon %s: %w", bin, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release daemon process: %w", err)
	}
	return daemon.WaitUntilListening(addr, defaultStartTimeout)
}

func resolveDaemonBinary(override string) (string, error) {
	if override != "" {
		if filepath.IsAbs(override) {
			if _, err := os.Stat(override); err != nil {
				return "", err
			}
			return override, nil
		}
		if p, err := exec.LookPath(override); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("remote-shell-mcpd"); err == nil {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	candidate := filepath.Join(dir, "remote-shell-mcpd")
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	base := strings.TrimSuffix(filepath.Base(exe), filepath.Ext(exe))
	if base == "remote-shell-mcp" {
		candidate = filepath.Join(dir, "remote-shell-mcpd")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("could not locate remote-shell-mcpd binary; set --daemon-binary or put it on PATH")
}
