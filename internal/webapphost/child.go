package webapphost

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// RunChild watches the inherited control pipe, including when Platform dies.
// Node cannot create subprocesses; the Windows launcher may create its Node
// child, so the watchdog itself joins a kill-on-close Job before spawning it.
func RunChild(args []string) error {
	if len(args) < 4 {
		return fmt.Errorf("invalid WebApp child arguments")
	}
	ms, err := strconv.Atoi(args[0])
	if err != nil || ms < 1 || ms > 30000 {
		return fmt.Errorf("invalid shutdown timeout")
	}
	release, err := prepareChildJob()
	if err != nil {
		return err
	}
	defer release()
	lease, err := lockGeneration(args[1])
	if err != nil {
		return err
	}
	defer lease.Close()
	cmd := exec.Command(args[2], args[3:]...)
	cmd.Env = os.Environ()
	cmd.Dir = ""
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	setupChild(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	eof := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(eof) }()
	select {
	case err := <-done:
		terminateChild(cmd, true)
		return err
	case <-eof:
	}
	terminateChild(cmd, false)
	select {
	case <-done:
		terminateChild(cmd, true)
		return nil
	case <-time.After(time.Duration(ms) * time.Millisecond):
	}
	terminateChild(cmd, true)
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		return fmt.Errorf("webapp_stop_incomplete")
	}
}
