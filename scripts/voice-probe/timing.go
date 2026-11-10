package main

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
)

//go:embed browser-timing.js
var browserTimingScript string

type timingPoint struct {
	Category         string   `json:"category"`
	Clock            string   `json:"clock"`
	TimeOrigin       float64  `json:"timeOrigin"`
	SessionID        string   `json:"sessionId"`
	Turn             int      `json:"turn"`
	Stage            string   `json:"stage"`
	AtMS             *float64 `json:"atMs"`
	Purpose          string   `json:"purpose"`
	Outcome          string   `json:"outcome"`
	Source           string   `json:"source"`
	ClockSource      string   `json:"clockSource"`
	Build            string   `json:"build"`
	Host             string   `json:"host"`
	Mode             string   `json:"mode"`
	Codec            string   `json:"codec"`
	ConnectionReused *bool    `json:"connectionReused"`
	StartupRetried   *bool    `json:"startupRetried"`
}

type timingStats struct {
	Count int      `json:"count"`
	P50   *float64 `json:"p50Ms,omitempty"`
	P95   *float64 `json:"p95Ms,omitempty"`
}

type timingSample struct {
	SessionID    string   `json:"sessionId"`
	Turn         int      `json:"turn"`
	Build        string   `json:"build"`
	Host         string   `json:"host"`
	Mode         string   `json:"mode"`
	Codec        string   `json:"codec"`
	Outcome      string   `json:"outcome"`
	PlayClock    string   `json:"playClock,omitempty"`
	EndToPlay    *float64 `json:"speechEndToPlayMs,omitempty"`
	ManualToPlay *float64 `json:"manualStopToPlayMs,omitempty"`
	Silence      *float64 `json:"interruptToLocalSilenceMs,omitempty"`
	Detection    *float64 `json:"candidateToDetectionMs,omitempty"`
	Issues       []string `json:"issues,omitempty"`
}

type timingReport struct {
	Scope              string                    `json:"scope"`
	Samples            []*timingSample           `json:"samples"`
	Outcomes           map[string]int            `json:"outcomes"`
	EndToPlay          timingStats               `json:"speechEndToPlay"`
	ManualToPlay       timingStats               `json:"manualStopToPlay"`
	Silence            timingStats               `json:"interruptToLocalSilence"`
	Detection          timingStats               `json:"candidateToDetection"`
	ServerStageCounts  map[string]int            `json:"serverStageCounts"`
	OrphanClientEvents int                       `json:"orphanClientEvents"`
	ErrorRate          *float64                  `json:"errorRateOfStartedTurns,omitempty"`
	Connections        map[string]map[string]int `json:"providerConnections"`
}

func summarizeTimings(input io.Reader) (timingReport, error) {
	report := timingReport{Scope: "client-clock samples only; output timestamps/context estimates are not an acoustic measurement; missing/failed samples retained; error rate denominator is all started turns", Outcomes: map[string]int{}, ServerStageCounts: map[string]int{}, Connections: map[string]map[string]int{}}
	groups := map[string][]timingPoint{}
	order := []string{}
	scanner := bufio.NewScanner(io.LimitReader(input, 64<<20+1))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	lines, bytes := 0, 0
	for scanner.Scan() {
		lines++
		line := scanner.Text()
		bytes += len(line) + 1
		if lines > 50000 || bytes > 64<<20 {
			return report, fmt.Errorf("timing capture exceeds bounded input limit")
		}
		// Accept the existing Go log prefix without treating its wall clock as data.
		pos := strings.IndexByte(line, '{')
		if pos < 0 {
			continue
		}
		var p timingPoint
		if err := json.Unmarshal([]byte(line[pos:]), &p); err != nil {
			return report, fmt.Errorf("invalid timing JSON at line %d", lines)
		}
		if p.Category != "voice.timing" {
			continue
		}
		if p.Stage == "probeOverflow" {
			return report, fmt.Errorf("probe overflow: capture is incomplete; do not report percentiles")
		}
		if p.SessionID == "" || p.AtMS == nil || math.IsNaN(*p.AtMS) || math.IsInf(*p.AtMS, 0) || *p.AtMS < 0 {
			return report, fmt.Errorf("invalid timing identity/clock at line %d", lines)
		}
		if p.Clock == "server_session_monotonic" {
			report.ServerStageCounts[p.Stage]++
			if p.Stage == "asrFinal" || p.Stage == "asrFailed" || p.Stage == "ttsFinished" {
				counts := report.Connections[p.Stage]
				if counts == nil {
					counts = map[string]int{}
					report.Connections[p.Stage] = counts
				}
				kind := "unknown"
				if p.ConnectionReused != nil {
					kind = "cold"
					if *p.ConnectionReused {
						kind = "warm"
					}
				}
				counts[kind]++
				if p.StartupRetried != nil && *p.StartupRetried {
					counts["startupRetried"]++
				}
			}
			continue // Never subtract server and client clocks.
		}
		if p.Clock != "client_performance" || p.TimeOrigin <= 0 || p.Turn <= 0 {
			return report, fmt.Errorf("invalid client clock at line %d", lines)
		}
		key := fmt.Sprintf("%s:%d", p.SessionID, p.Turn)
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], p)
	}
	if err := scanner.Err(); err != nil {
		return report, fmt.Errorf("unable to read bounded timing capture")
	}
	var e2e, manual, silence, detection []float64
	for _, key := range order {
		points := groups[key]
		var start *timingPoint
		for i := range points {
			if points[i].Stage == "listenStart" {
				start = &points[i]
				break
			}
		}
		if start == nil {
			report.OrphanClientEvents += len(points)
			continue
		}
		sample := &timingSample{SessionID: start.SessionID, Turn: start.Turn, Build: start.Build, Host: start.Host, Mode: start.Mode, Codec: start.Codec, Outcome: "incomplete"}
		report.Samples = append(report.Samples, sample)
		valid := true
		for i, p := range points {
			if p.Build != start.Build || p.Host != start.Host || p.Mode != start.Mode || p.Codec != start.Codec {
				return report, fmt.Errorf("mixed build/host/mode/codec within turn: use separate captures for each comparison cohort")
			}
			if p.Stage == "listenStart" && &points[i] != start {
				valid = false
				sample.Issues = append(sample.Issues, "duplicate listenStart")
			}
			if p.TimeOrigin != start.TimeOrigin {
				valid = false
				sample.Issues = append(sample.Issues, "mixed client clock origins")
				break
			}
		}
		var play, speechEnd, stop, requested, silenced, candidate, detected *timingPoint
		for i := range points {
			p := &points[i]
			switch p.Stage {
			case "turnEnd":
				if sample.Outcome != "incomplete" {
					valid = false
					sample.Issues = append(sample.Issues, "duplicate terminal outcome")
				}
				switch p.Outcome {
				case "complete", "empty", "error", "aborted", "superseded", "closed":
					sample.Outcome = p.Outcome
				default:
					valid = false
					sample.Issues = append(sample.Issues, "invalid terminal outcome")
				}
			case "playStart":
				if p.Purpose == "final" {
					if play != nil {
						valid = false
						sample.Issues = append(sample.Issues, "duplicate final playStart")
					}
					play = p
				}
			case "speechEnd":
				if speechEnd != nil || p.Source != "annotation" && p.Source != "vad" {
					valid = false
					sample.Issues = append(sample.Issues, "invalid speech-end evidence")
				}
				speechEnd = p
			case "manualStop":
				stop = p
			case "interruptRequested":
				requested = p
			case "localSilenced":
				silenced = p
			case "speechCandidate":
				if candidate != nil {
					valid = false
					sample.Issues = append(sample.Issues, "duplicate speech candidate")
				}
				candidate = p
			case "speechDetected":
				if detected != nil {
					valid = false
					sample.Issues = append(sample.Issues, "duplicate speech detection")
				}
				detected = p
			}
		}
		if play != nil {
			sample.PlayClock = play.ClockSource
			if play.ClockSource != "output_timestamp" && play.ClockSource != "context_clock" {
				valid = false
				sample.Issues = append(sample.Issues, "invalid playback clock evidence")
			}
		}
		metric := func(a, b *timingPoint) *float64 {
			if !valid || a == nil || b == nil {
				return nil
			}
			if *a.AtMS < *start.AtMS || *b.AtMS < *a.AtMS {
				sample.Issues = append(sample.Issues, "invalid timing order")
				return nil
			}
			value := *b.AtMS - *a.AtMS
			return &value
		}
		sample.EndToPlay, sample.ManualToPlay, sample.Silence = metric(speechEnd, play), metric(stop, play), metric(requested, silenced)
		if sample.Mode == "duplex" {
			sample.Detection = metric(candidate, detected)
		}
		if speechEnd == nil {
			sample.Issues = append(sample.Issues, "speechEnd unmeasured")
		}
		if play == nil {
			sample.Issues = append(sample.Issues, "final playStart unmeasured")
		}
		report.Outcomes[sample.Outcome]++
		first := report.Samples[0]
		if sample.Build != first.Build || sample.Host != first.Host || sample.Mode != first.Mode || sample.Codec != first.Codec {
			return report, fmt.Errorf("mixed build/host/mode/codec: use separate captures for each comparison cohort")
		}
		// Report every outcome, but successful latency percentiles never absorb failures.
		if sample.Outcome == "complete" {
			if sample.EndToPlay != nil {
				e2e = append(e2e, *sample.EndToPlay)
			}
			if sample.ManualToPlay != nil {
				manual = append(manual, *sample.ManualToPlay)
			}
		}
		if sample.Silence != nil {
			silence = append(silence, *sample.Silence)
		}
		if sample.Detection != nil {
			detection = append(detection, *sample.Detection)
		}
	}
	report.EndToPlay, report.ManualToPlay, report.Silence = timingQuantiles(e2e), timingQuantiles(manual), timingQuantiles(silence)
	report.Detection = timingQuantiles(detection)
	if len(report.Samples) > 0 {
		rate := float64(report.Outcomes["error"]) / float64(len(report.Samples))
		report.ErrorRate = &rate
	}
	return report, nil
}

func timingQuantiles(values []float64) timingStats {
	sort.Float64s(values)
	out := timingStats{Count: len(values)}
	if len(values) > 0 {
		p50, p95 := values[int(math.Ceil(.5*float64(len(values))))-1], values[int(math.Ceil(.95*float64(len(values))))-1]
		out.P50, out.P95 = &p50, &p95
	}
	return out
}

func reportTimings(path string, output io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("unable to open timing capture")
	}
	defer f.Close()
	report, err := summarizeTimings(f)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}
