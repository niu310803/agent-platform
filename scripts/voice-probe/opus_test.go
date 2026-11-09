package main

import (
	"agent-platform/internal/voice"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeOpusPacesByPacketDurationAndRejectsTruncation(t *testing.T) {
	var out bytes.Buffer
	w, err := voice.NewOpusOggWriter(16000, 312, func(p []byte) error { _, e := out.Write(p); return e })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range [][]byte{{0x58, 1}, {0xf8, 2}} {
		if err = w.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Finish(); err != nil {
		t.Fatal(err)
	}
	chunks, duration, err := probeInput(out.Bytes(), "opus")
	if err != nil || duration != 80*time.Millisecond || len(chunks) != 4 || chunks[2].until != 60*time.Millisecond || chunks[3].until != 80*time.Millisecond {
		t.Fatalf("duration=%v chunks=%v err=%v", duration, chunks, err)
	}
	if !bytes.Equal(bytes.Join([][]byte{chunks[0].data, chunks[1].data, chunks[2].data, chunks[3].data}, nil), out.Bytes()) {
		t.Fatal("changed audio or invented silence")
	}
	if _, _, err = probeInput(out.Bytes()[:out.Len()-1], "opus"); err == nil {
		t.Fatal("truncation accepted")
	}
	path := filepath.Join(t.TempDir(), "oversize")
	if err = os.WriteFile(path, make([]byte, (2<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readProbeAudio(path); err == nil {
		t.Fatal("oversized file accepted")
	}
}
