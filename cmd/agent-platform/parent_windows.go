package main

import "golang.org/x/sys/windows"

type parentHandle struct{ handle windows.Handle }

func parentProbe(pid int) parentHandle {
	h, _ := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	return parentHandle{h}
}
func (p parentHandle) running() bool {
	if p.handle == 0 {
		return false
	}
	state, err := windows.WaitForSingleObject(p.handle, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}
func (p parentHandle) close() {
	if p.handle != 0 {
		windows.CloseHandle(p.handle)
	}
}
