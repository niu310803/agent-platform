//go:build !windows

package webapphost

import (
	"os/exec"
	"syscall"
)

func prepareChildJob() (func(), error) { return func() {}, nil }
func setupChild(cmd *exec.Cmd)         { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func terminateChild(cmd *exec.Cmd, force bool) {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}
