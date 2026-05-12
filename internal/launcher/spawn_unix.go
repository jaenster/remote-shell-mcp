//go:build !windows

package launcher

import (
	"os/exec"
	"syscall"
	"time"
)

const defaultStartTimeout = 10 * time.Second

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
