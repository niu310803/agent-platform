package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"agent-platform/internal/voice"
)

type pacedAudio struct {
	data  []byte
	until time.Duration
}

func probeInput(audio []byte, format string) ([]pacedAudio, time.Duration, error) {
	var chunks []pacedAudio
	if format == "pcm" {
		all := append(bytes.Clone(audio), make([]byte, 38400)...)
		for offset := 0; offset < len(all); offset += 3200 {
			end := min(offset+3200, len(all))
			chunks = append(chunks, pacedAudio{all[offset:end], time.Duration(end) * time.Second / 32000})
		}
		return chunks, time.Duration(len(audio)) * time.Second / 32000, nil
	}
	info, err := voice.InspectOpusOgg(audio)
	if err != nil {
		return nil, 0, err
	}
	// Probe fixtures start at granule zero and contain endpoint-encoded packets.
	if info.EndGranule != info.DecodedSamples {
		return nil, 0, errors.New("probe requires an untrimmed zero-origin Ogg fixture")
	}
	var until time.Duration
	for offset := 0; offset < len(audio); {
		count := int(audio[offset+26])
		size := 27 + count
		for _, n := range audio[offset+27 : offset+27+count] {
			size += int(n)
		}
		granule := binary.LittleEndian.Uint64(audio[offset+6 : offset+14])
		if granule != ^uint64(0) {
			until = time.Duration(granule) * time.Second / 48000
		}
		chunks = append(chunks, pacedAudio{audio[offset : offset+size], until})
		offset += size
	}
	return chunks, time.Duration(info.DecodedSamples) * time.Second / 48000, nil
}

func inspectOggFile(path string) error {
	data, err := readProbeAudio(path)
	if err != nil {
		return errors.New("unable to read Ogg capture")
	}
	info, err := voice.InspectOpusOgg(data)
	report := map[string]any{"ogg": info}
	if err != nil {
		report["error"] = err.Error()
	}
	_ = json.NewEncoder(os.Stdout).Encode(report)
	return err
}

// This explicit probe operation remuxes already-encoded fixture packets. It is
// not a runtime repair path for unsupported provider output.
func (p probe) opusASR(ctx context.Context, model, path, out string) (map[string]any, error) {
	data, err := readProbeAudio(path)
	if err != nil {
		return nil, errors.New("unable to read bounded packet fixture")
	}
	var input struct {
		PreSkip uint16   `json:"preSkip48k"`
		Packets [][]byte `json:"packets"`
	}
	if err = json.Unmarshal(data, &input); err != nil || len(input.Packets) == 0 || len(input.Packets) > 12000 {
		return nil, errors.New("invalid packet fixture")
	}
	var audio bytes.Buffer
	w, err := voice.NewOpusOggWriter(16000, input.PreSkip, func(page []byte) error {
		if audio.Len()+len(page) > 2<<20 {
			return errors.New("probe audio exceeds byte limit")
		}
		_, err := audio.Write(page)
		return err
	})
	if err != nil {
		return nil, err
	}
	samples := 0
	for _, packet := range input.Packets {
		n, err := voice.OpusPacketSamples(packet)
		if err != nil {
			return nil, err
		}
		samples += n
		if samples > 30*48000 {
			return nil, errors.New("probe audio exceeds 30 seconds")
		}
		if err = w.Write(packet); err != nil {
			return nil, err
		}
	}
	if err = w.Finish(); err != nil {
		return nil, err
	}
	info, err := voice.InspectOpusOgg(audio.Bytes())
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(out, 0700); err != nil {
		return nil, errors.New("unable to create output directory")
	}
	if err = os.WriteFile(filepath.Join(out, "asr.ogg"), audio.Bytes(), 0600); err != nil {
		return nil, errors.New("unable to save Ogg fixture")
	}
	result, err := p.asr(ctx, model, audio.Bytes(), "opus")
	result["input_ogg"] = info
	return result, err
}

func readProbeAudio(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	if len(data) > 2<<20 {
		return nil, errors.New("probe input exceeds byte limit")
	}
	return data, err
}
