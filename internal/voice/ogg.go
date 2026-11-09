package voice

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

const maxOggPageBytes = 27 + 255 + 255*255

// This is the bounded mono, mapping-family-zero voice profile, not a general
// media-file reader. In particular, chained streams and large tags are rejected.
type OggOpusInfo struct {
	InputSampleRate uint32 `json:"inputSampleRate"`
	PreSkip         uint16 `json:"preSkip48k"`
	Gain            int16  `json:"outputGainQ8"`
	Pages           int    `json:"pages"`
	Packets         int    `json:"packets"`
	DecodedSamples  uint64 `json:"decodedSamples48k"`
	EndGranule      uint64 `json:"endGranule48k"`
	EOS             bool   `json:"eos"`
}

type oggOpusReader struct {
	info             OggOpusInfo
	buffer, pending  []byte
	serial, sequence uint32
	headers          int
	audioSeen        bool
	lastGranule      uint64
	onPacket         func([]byte, int) error
}

func oggCRC(page []byte) uint32 {
	var crc uint32
	for i, value := range page {
		if i >= 22 && i < 26 {
			value = 0
		}
		crc ^= uint32(value) << 24
		for bit := 0; bit < 8; bit++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// InspectOpusOgg rejects invalid CRC/framing/timestamps and incomplete streams.
// It is also used by the paid probe, so format support is based on actual bytes.
func InspectOpusOgg(data []byte) (OggOpusInfo, error) {
	r := &oggOpusReader{}
	err := r.write(data)
	if err == nil {
		err = r.finish()
	}
	return r.info, err
}

func (r *oggOpusReader) write(data []byte) error {
	for len(data) > 0 {
		if r.info.EOS {
			return errors.New("Ogg data after EOS")
		}
		n := min(len(data), maxOggPageBytes-len(r.buffer))
		if n == 0 {
			return errors.New("Ogg page buffer exceeded")
		}
		r.buffer = append(r.buffer, data[:n]...)
		data = data[n:]
		for len(r.buffer) >= 27 {
			if !bytes.Equal(r.buffer[:4], []byte("OggS")) || r.buffer[4] != 0 || r.buffer[5]&^byte(7) != 0 {
				return errors.New("invalid Ogg header")
			}
			count := int(r.buffer[26])
			if len(r.buffer) < 27+count {
				break
			}
			size := 27 + count
			for _, lace := range r.buffer[27:size] {
				size += int(lace)
			}
			if len(r.buffer) < size {
				break
			}
			if err := r.page(r.buffer[:size]); err != nil {
				return err
			}
			r.buffer = r.buffer[size:]
			if r.info.EOS && (len(r.buffer) > 0 || len(data) > 0) {
				return errors.New("Ogg data after EOS")
			}
		}
	}
	return nil
}

func (r *oggOpusReader) page(page []byte) error {
	flags := page[5]
	granule := binary.LittleEndian.Uint64(page[6:14])
	serial := binary.LittleEndian.Uint32(page[14:18])
	sequence := binary.LittleEndian.Uint32(page[18:22])
	if binary.LittleEndian.Uint32(page[22:26]) != oggCRC(page) {
		return errors.New("Ogg CRC mismatch")
	}
	if r.info.Pages == 0 {
		if flags != 2 || sequence != 0 {
			return errors.New("Ogg BOS required")
		}
		r.serial = serial
	} else if flags&2 != 0 || serial != r.serial {
		return errors.New("unsupported chained or multiplexed Ogg stream")
	}
	if sequence != r.sequence {
		return errors.New("Ogg page sequence gap")
	}
	if (flags&1 != 0) != (len(r.pending) > 0) {
		return errors.New("Ogg packet continuation mismatch")
	}
	r.sequence++
	r.info.Pages++
	count := int(page[26])
	position := 27 + count
	pageSamples := uint64(0)
	completed := 0
	var packets [][]byte
	var durations []int
	for i, lace := range page[27:position] {
		size := int(lace)
		if len(r.pending)+size > maxOpusPacketBytes {
			return errors.New("Ogg packet or tags exceed voice limit")
		}
		r.pending = append(r.pending, page[position:position+size]...)
		position += size
		if lace == 255 {
			continue
		}
		packet := r.pending
		r.pending = nil
		completed++
		switch r.headers {
		case 0:
			if r.info.Pages != 1 || i != count-1 || completed != 1 || len(packet) != 19 || !bytes.Equal(packet[:8], []byte("OpusHead")) || packet[8] != 1 || packet[9] != 1 || packet[18] != 0 || granule != 0 {
				return errors.New("unsupported Opus identification header")
			}
			r.info.PreSkip = binary.LittleEndian.Uint16(packet[10:12])
			r.info.InputSampleRate = binary.LittleEndian.Uint32(packet[12:16])
			r.info.Gain = int16(binary.LittleEndian.Uint16(packet[16:18]))
			r.headers++
		case 1:
			if i != count-1 || granule != 0 || !validOpusTags(packet) {
				return errors.New("invalid Opus comment header")
			}
			r.headers++
		default:
			samples, err := OpusPacketSamples(packet)
			if err != nil {
				return err
			}
			pageSamples += uint64(samples)
			packets = append(packets, packet)
			durations = append(durations, samples)
		}
	}
	if r.info.Pages == 1 && r.headers != 1 {
		return errors.New("incomplete Opus identification header")
	}
	if len(packets) == 0 {
		if completed == 0 && granule != ^uint64(0) {
			return errors.New("Ogg unfinished packet must have no granule")
		}
		if flags&4 != 0 {
			return errors.New("Ogg EOS without audio packet")
		}
		return nil
	}
	if flags&4 != 0 && len(r.pending) != 0 {
		return errors.New("unfinished Ogg packet at EOS")
	}
	if granule == ^uint64(0) {
		return errors.New("missing Ogg audio granule")
	}
	expected := r.lastGranule + pageSamples
	if !r.audioSeen {
		if flags&4 == 0 && granule < pageSamples {
			return errors.New("invalid initial Ogg granule")
		}
		if flags&4 != 0 && granule < uint64(r.info.PreSkip) {
			return errors.New("Ogg EOS precedes pre-skip")
		}
	} else if flags&4 != 0 {
		// End trimming may discard samples from the last completed packet only.
		if granule > expected || granule < expected-uint64(durations[len(durations)-1]) || granule < uint64(r.info.PreSkip) {
			return errors.New("invalid Ogg EOS trim")
		}
	} else if granule != expected {
		return fmt.Errorf("invalid Ogg granule on page %d: got %d, expected %d", sequence, granule, expected)
	}
	r.audioSeen = true
	r.lastGranule = granule
	r.info.DecodedSamples += pageSamples
	r.info.Packets += len(packets)
	r.info.EndGranule = granule
	r.info.EOS = flags&4 != 0
	for i, packet := range packets {
		if r.onPacket != nil {
			if err := r.onPacket(packet, durations[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *oggOpusReader) finish() error {
	if len(r.buffer) != 0 || len(r.pending) != 0 || r.headers != 2 || !r.audioSeen || !r.info.EOS {
		return errors.New("truncated Ogg Opus stream")
	}
	return nil
}

func validOpusTags(packet []byte) bool {
	if len(packet) < 16 || !bytes.Equal(packet[:8], []byte("OpusTags")) {
		return false
	}
	position := 8
	readLength := func() (int, bool) {
		if position+4 > len(packet) {
			return 0, false
		}
		n := uint64(binary.LittleEndian.Uint32(packet[position : position+4]))
		position += 4
		if n > uint64(len(packet)-position) {
			return 0, false
		}
		return int(n), true
	}
	n, ok := readLength()
	if !ok {
		return false
	}
	position += n
	if position+4 > len(packet) {
		return false
	}
	count := uint64(binary.LittleEndian.Uint32(packet[position : position+4]))
	position += 4
	if count > uint64((len(packet)-position)/4) {
		return false
	}
	for i := uint64(0); i < count; i++ {
		n, ok = readLength()
		if !ok {
			return false
		}
		position += n
	}
	return true
}

// One-stream muxer for endpoint-encoded Opus. It holds at most one packet so
// finish can set EOS on the actual last packet without inventing silence.
type OpusOggWriter struct {
	write    func([]byte) error
	sequence uint32
	granule  uint64
	pending  []byte
	samples  int
	preSkip  uint16
	closed   bool
}

func NewOpusOggWriter(sampleRate int, preSkip uint16, write func([]byte) error) (*OpusOggWriter, error) {
	if write == nil || sampleRate != 16000 && sampleRate != 24000 {
		return nil, errors.New("unsupported Opus input rate")
	}
	w := &OpusOggWriter{write: write, preSkip: preSkip}
	head := append([]byte("OpusHead"), 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	binary.LittleEndian.PutUint16(head[10:12], preSkip)
	binary.LittleEndian.PutUint32(head[12:16], uint32(sampleRate))
	if err := w.packet(head, 2, 0); err != nil {
		return nil, err
	}
	tags := append([]byte("OpusTags"), 0, 0, 0, 0, 0, 0, 0, 0)
	if err := w.packet(tags, 0, 0); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *OpusOggWriter) Write(packet []byte) error {
	if w.closed {
		return errors.New("Opus stream closed")
	}
	samples, err := OpusPacketSamples(packet)
	if err != nil {
		return err
	}
	if w.pending != nil {
		w.granule += uint64(w.samples)
		if err = w.packet(w.pending, 0, w.granule); err != nil {
			return err
		}
	}
	w.pending = bytes.Clone(packet)
	w.samples = samples
	return nil
}

func (w *OpusOggWriter) Finish() error {
	if w.closed {
		return errors.New("Opus stream closed")
	}
	w.closed = true
	if w.pending == nil {
		return errors.New("empty Opus stream")
	}
	w.granule += uint64(w.samples)
	if w.granule < uint64(w.preSkip) {
		w.pending = nil
		return errors.New("Opus stream ends before pre-skip")
	}
	err := w.packet(w.pending, 4, w.granule)
	w.pending = nil
	return err
}

func (w *OpusOggWriter) packet(packet []byte, flags byte, granule uint64) error {
	// A packet can continue across pages; an exact 255-byte multiple needs a
	// terminating zero lace, including when that terminator needs another page.
	laces := make([]byte, len(packet)/255+1)
	for i := range laces {
		laces[i] = 255
	}
	laces[len(laces)-1] = byte(len(packet) % 255)
	continued := byte(0)
	for len(laces) > 0 {
		n := min(255, len(laces))
		segment := laces[:n]
		size := 0
		for _, v := range segment {
			size += int(v)
		}
		pageFlags := continued
		pageGranule := ^uint64(0)
		if continued == 0 {
			pageFlags |= flags & 2
		}
		if n == len(laces) {
			pageFlags |= flags & 4
			pageGranule = granule
		}
		page := make([]byte, 27+n+size)
		copy(page, "OggS")
		page[5] = pageFlags
		page[26] = byte(n)
		binary.LittleEndian.PutUint64(page[6:14], pageGranule)
		binary.LittleEndian.PutUint32(page[14:18], 1)
		binary.LittleEndian.PutUint32(page[18:22], w.sequence)
		copy(page[27:], segment)
		copy(page[27+n:], packet[:size])
		binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))
		if err := w.write(page); err != nil {
			w.closed = true
			return err
		}
		w.sequence++
		packet = packet[size:]
		laces = laces[n:]
		continued = 1
	}
	return nil
}
