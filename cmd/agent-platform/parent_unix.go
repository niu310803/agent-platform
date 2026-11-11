//go:build !windows

package main

import "syscall"

type parentHandle struct{ pid int }

func parentProbe(pid int) parentHandle { return parentHandle{pid} }
func (p parentHandle) running() bool {
	err := syscall.Kill(p.pid, 0)
	return err == nil || err == syscall.EPERM
}
func (p parentHandle) close() {}
