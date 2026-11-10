package voice

import (
	"bytes"
	"testing"
)

func TestOpusPacketDurationVectors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		packet  []byte
		samples int
	}{
		{"silk10", []byte{0, 1}, 480},
		{"silk40", []byte{16, 1}, 1920},
		{"silk60 provider16k", []byte{0x58, 2, 0xf9}, 2880},
		{"hybrid20x3 VBR provider24k", []byte{0x6b, 0x83, 1, 2, 3, 4, 5, 6}, 2880},
		{"celt2.5", []byte{0x80, 1}, 120},
		{"celt20x2 CBR", []byte{0xf9, 1, 2}, 1920},
		{"celt20x2 VBR", []byte{0xfa, 1, 2, 3, 4}, 1920},
		{"celt20x6 padded", []byte{0xfb, 0x46, 1, 1, 2, 3, 4, 5, 6, 0}, 5760},
		{"zero byte DTX payload", []byte{0xf8}, 960},
		{"48 minimum frames", []byte{0x83, 48}, 5760},
		{"two byte frame length", append([]byte{0xfa, 252, 0}, make([]byte, 252)...), 1920},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := OpusPacketSamples(tc.packet)
			if err != nil || n != tc.samples {
				t.Fatalf("samples=%d err=%v", n, err)
			}
		})
	}
}

func TestOpusPacketRejectsMalformedFraming(t *testing.T) {
	for _, packet := range [][]byte{nil, {1, 0}, {2}, {2, 5, 0}, {3}, {3, 0}, {0x83, 49}, {0xfb, 7}, {3, 0x41}, {3, 0x41, 255}, {3, 0x41, 2, 0}, {3, 0x82, 252}, {3, 0x82, 5, 0}, {0xfb, 2, 1}, append([]byte{0}, bytes.Repeat([]byte{1}, 1276)...), make([]byte, maxOpusPacketBytes+1)} {
		if _, err := OpusPacketSamples(packet); err == nil {
			t.Fatalf("accepted malformed packet (%d bytes): %x", len(packet), packet[:min(len(packet), 8)])
		}
	}
}
