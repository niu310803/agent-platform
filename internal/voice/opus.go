package voice

import "errors"

// The voice profile carries one complete RFC 6716 packet per binary message.
// This checks framing and duration, not the compressed audio's entropy coding.
const maxOpusPacketBytes = 65536

var errOpusPacket = errors.New("invalid Opus packet")

// OpusPacketSamples returns duration in the fixed 48kHz Ogg granule clock.
// No Opus decoding, transcoding or sample-rate conversion is performed.
func OpusPacketSamples(packet []byte) (int, error) {
	if len(packet) == 0 || len(packet) > maxOpusPacketBytes {
		return 0, errOpusPacket
	}
	config := packet[0] >> 3
	frameSamples := 0
	switch {
	case config < 12:
		frameSamples = [...]int{480, 960, 1920, 2880}[config&3]
	case config < 16:
		frameSamples = 480 << (config & 1)
	default:
		frameSamples = 120 << (config & 3)
	}
	offset, end, count := 1, len(packet), 1
	readSize := func() (int, error) {
		if offset >= end {
			return 0, errOpusPacket
		}
		n := int(packet[offset])
		offset++
		if n >= 252 {
			if offset >= end {
				return 0, errOpusPacket
			}
			n += 4 * int(packet[offset])
			offset++
		}
		return n, nil
	}
	var lengths []int
	switch packet[0] & 3 {
	case 0:
		lengths = []int{end - offset}
	case 1:
		count = 2
		if (end-offset)%2 != 0 {
			return 0, errOpusPacket
		}
		lengths = []int{(end - offset) / 2, (end - offset) / 2}
	case 2:
		count = 2
		n, err := readSize()
		if err != nil {
			return 0, err
		}
		lengths = []int{n, end - offset - n}
	case 3:
		if len(packet) < 2 {
			return 0, errOpusPacket
		}
		flags := packet[1]
		offset = 2
		count = int(flags & 63)
		if count == 0 || count > 48 || frameSamples*count > 5760 {
			return 0, errOpusPacket
		}
		if flags&64 != 0 {
			padding := 0
			for {
				if offset >= end {
					return 0, errOpusPacket
				}
				n := int(packet[offset])
				offset++
				if n == 255 {
					padding += 254
				} else {
					padding += n
					break
				}
			}
			end -= padding
			if end < offset {
				return 0, errOpusPacket
			}
		}
		if flags&128 == 0 {
			if (end-offset)%count != 0 {
				return 0, errOpusPacket
			}
			lengths = []int{(end - offset) / count}
		} else {
			total := 0
			for i := 0; i < count-1; i++ {
				n, err := readSize()
				if err != nil {
					return 0, err
				}
				lengths = append(lengths, n)
				total += n
			}
			lengths = append(lengths, end-offset-total)
		}
	}
	if frameSamples*count > 5760 {
		return 0, errOpusPacket
	}
	for _, n := range lengths {
		if n < 0 || n > 1275 {
			return 0, errOpusPacket
		}
	}
	return frameSamples * count, nil
}
