package voice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func oggFixture(t *testing.T, packets ...[]byte) [][]byte {
	t.Helper()
	var pages [][]byte
	w, err := NewOpusOggWriter(16000, 312, func(page []byte) error { pages = append(pages, bytes.Clone(page)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range packets {
		if err = w.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Finish(); err != nil {
		t.Fatal(err)
	}
	return pages
}

func rewriteOggCRC(page []byte) { binary.LittleEndian.PutUint32(page[22:26], oggCRC(page)) }

func TestOggFixedHeaderCRCAndGranules(t *testing.T) {
	// Independent captured OpusHead vector: serial=0, preskip=312, rate=16000.
	page := []byte{'O', 'g', 'g', 'S', 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0x49, 0x2a, 0xf6, 0xa0, 1, 19, 'O', 'p', 'u', 's', 'H', 'e', 'a', 'd', 1, 1, 0x38, 1, 0x80, 0x3e, 0, 0, 0, 0, 0}
	if got := oggCRC(page); got != 0xa0f62a49 {
		t.Fatalf("CRC=%08x", got)
	}
	packets := [][]byte{{0x58, 1}, {0xf8, 1}, {0xf8, 2}}
	pages := oggFixture(t, packets...)
	data := bytes.Join(pages, nil)
	info, err := InspectOpusOgg(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.PreSkip != 312 || info.InputSampleRate != 16000 || info.DecodedSamples != 4800 || info.EndGranule != 4800 || !info.EOS || info.Packets != 3 {
		t.Fatalf("bad clock or header: %+v", info)
	}
	for _, chunk := range []int{1, 2, 26, 27, 28, 255, 4096, len(data)} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			var received [][]byte
			r := &oggOpusReader{onPacket: func(p []byte, _ int) error { received = append(received, bytes.Clone(p)); return nil }}
			for offset := 0; offset < len(data); offset += chunk {
				if err := r.write(data[offset:min(offset+chunk, len(data))]); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.finish(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytes.Join(received, nil), bytes.Join(packets, nil)) {
				t.Fatal("packet boundaries lost")
			}
		})
	}
}

func TestOggLacingCrossPageAndExactMultiple(t *testing.T) {
	// A legal one-frame packet with padding spans two pages. The second page
	// completes the packet; the first carries granule=-1 and no EOS.
	packet := []byte{0xfb, 0x41}
	for i := 0; i < 255; i++ {
		packet = append(packet, 255)
	}
	packet = append(packet, 0)
	packet = append(packet, make([]byte, 255*254)...)
	if _, err := OpusPacketSamples(packet); err != nil {
		t.Fatal(err)
	}
	pages := oggFixture(t, packet)
	if len(pages) != 4 || pages[2][5] != 0 || pages[3][5] != 5 || binary.LittleEndian.Uint64(pages[2][6:14]) != ^uint64(0) {
		t.Fatal("invalid continuation flags/granule")
	}
	if _, err := InspectOpusOgg(bytes.Join(pages, nil)); err != nil {
		t.Fatal(err)
	}
	// 255 bytes require a zero lace, not a continued packet.
	pages = oggFixture(t, append([]byte{0xf8}, make([]byte, 254)...))
	last := pages[len(pages)-1]
	if last[26] != 2 || last[27] != 255 || last[28] != 0 {
		t.Fatal("missing zero lace")
	}
	if _, err := InspectOpusOgg(bytes.Join(pages, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestOggRejectsMalformedAndTruncatedStreams(t *testing.T) {
	for name, change := range map[string]func([][]byte) [][]byte{
		"CRC":                  func(p [][]byte) [][]byte { p[2][len(p[2])-1] ^= 1; return p },
		"sequence":             func(p [][]byte) [][]byte { p[2][18]++; rewriteOggCRC(p[2]); return p },
		"serial":               func(p [][]byte) [][]byte { p[2][14]++; rewriteOggCRC(p[2]); return p },
		"continuation":         func(p [][]byte) [][]byte { p[2][5] |= 1; rewriteOggCRC(p[2]); return p },
		"unsupported mapping":  func(p [][]byte) [][]byte { p[0][len(p[0])-1] = 1; rewriteOggCRC(p[0]); return p },
		"tags length overflow": func(p [][]byte) [][]byte { p[1][36] = 255; rewriteOggCRC(p[1]); return p },
		"no EOS":               func(p [][]byte) [][]byte { p[len(p)-1][5] = 0; rewriteOggCRC(p[len(p)-1]); return p },
		"after EOS":            func(p [][]byte) [][]byte { return append(p, p[len(p)-1]) },
		"tail truncated":       func(p [][]byte) [][]byte { p[len(p)-1] = p[len(p)-1][:len(p[len(p)-1])-1]; return p },
		"EOS before pre-skip": func(p [][]byte) [][]byte {
			binary.LittleEndian.PutUint64(p[len(p)-1][6:14], 1)
			rewriteOggCRC(p[len(p)-1])
			return p
		},
		"granule gap": func(p [][]byte) [][]byte {
			binary.LittleEndian.PutUint64(p[3][6:14], 9999)
			rewriteOggCRC(p[3])
			return p
		},
	} {
		t.Run(name, func(t *testing.T) {
			pages := oggFixture(t, []byte{0xf8, 1}, []byte{0xf8, 2}, []byte{0xf8, 3})
			if _, err := InspectOpusOgg(bytes.Join(change(pages), nil)); err == nil {
				t.Fatal("invalid stream accepted")
			}
		})
	}
}

func TestOggEOSAllowsOnlyValidTrim(t *testing.T) {
	pages := oggFixture(t, []byte{0xf8, 1}, []byte{0xf8, 2})
	last := pages[len(pages)-1]
	binary.LittleEndian.PutUint64(last[6:14], 1800)
	rewriteOggCRC(last)
	info, err := InspectOpusOgg(bytes.Join(pages, nil))
	if err != nil || info.DecodedSamples != 1920 || info.EndGranule != 1800 {
		t.Fatalf("trim: %+v %v", info, err)
	}
}

func TestOggWriterPropagatesFailureAndRefusesReuse(t *testing.T) {
	n := 0
	failure := errors.New("sink failed")
	w, err := NewOpusOggWriter(16000, 0, func([]byte) error {
		n++
		if n > 2 {
			return failure
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Write([]byte{0xf8, 1}); err != nil {
		t.Fatal(err)
	}
	if err = w.Write([]byte{0xf8, 2}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err = w.Write([]byte{0xf8, 3}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatal("resumed broken stream")
	}
}
