package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"time"

	platform "agent-platform"
)

func listenerURL(addr net.Addr) string {
	host, port, _ := net.SplitHostPort(addr.String())
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
func writeReady(path, apiURL, appURL string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(map[string]any{"pid": os.Getpid(), "apiUrl": apiURL, "appUrl": appURL, "platformVersion": platform.Version()})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".ready-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func watchParent(ctx context.Context, pid int, stop func()) {
	alive := parentProbe(pid)
	defer alive.close()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if !alive.running() {
			stop()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
