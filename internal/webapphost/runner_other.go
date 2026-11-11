//go:build !linux

package webapphost

import "os"

func lockGeneration(path string) (*os.File, error) { return claimGeneration(path) }
func ownsLoopbackPort(int, string) bool            { return false }
func processTreeRSS(int) uint64                    { return 0 }
