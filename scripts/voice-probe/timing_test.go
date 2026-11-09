package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func timingFixture(turn int, stage string, at float64, extra map[string]any) string {
	p := map[string]any{"category": "voice.timing", "clock": "client_performance", "timeOrigin": 100000, "sessionId": "session", "turn": turn, "stage": stage, "atMs": at, "build": "test", "host": "web", "mode": "button", "codec": "pcm_s16le"}
	for key, value := range extra {
		p[key] = value
	}
	b, _ := json.Marshal(p)
	return string(b) + "\n"
}

func completedTimingFixture() string {
	return timingFixture(1, "listenStart", 100, nil) +
		timingFixture(1, "speechEnd", 500, map[string]any{"source": "annotation"}) +
		timingFixture(1, "manualStop", 600, nil) +
		timingFixture(1, "playStart", 2500, map[string]any{"purpose": "final", "clockSource": "output_timestamp"}) +
		timingFixture(1, "turnEnd", 2600, map[string]any{"outcome": "complete"})
}

func TestTimingReportKeepsFailuresAndSeparateClocks(t *testing.T) {
	input := completedTimingFixture() +
		timingFixture(2, "listenStart", 3000, nil) + timingFixture(2, "turnEnd", 4000, map[string]any{"outcome": "error"}) +
		timingFixture(3, "listenStart", 5000, nil) +
		timingFixture(3, "interruptRequested", 5500, nil) + timingFixture(3, "localSilenced", 5510, nil) +
		timingFixture(3, "turnEnd", 5600, map[string]any{"outcome": "superseded"}) +
		timingFixture(4, "listenStart", 6000, nil) +
		"2026/11/07 10:00:00 " + timingFixture(1, "asrFinal", 2000000, map[string]any{"clock": "server_session_monotonic", "connectionReused": true, "startupRetried": true, "text": "never export"})
	r, err := summarizeTimings(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Samples) != 4 || r.Outcomes["error"] != 1 || r.Outcomes["incomplete"] != 1 || *r.ErrorRate != .25 {
		t.Fatalf("outcomes lost: %+v", r)
	}
	if r.EndToPlay.Count != 1 || *r.EndToPlay.P50 != 2000 || *r.EndToPlay.P95 != 2000 || *r.ManualToPlay.P50 != 1900 || *r.Silence.P95 != 10 {
		t.Fatalf("wrong clocks: %+v", r)
	}
	if r.ServerStageCounts["asrFinal"] != 1 || r.Connections["asrFinal"]["warm"] != 1 || r.Connections["asrFinal"]["startupRetried"] != 1 {
		t.Fatal("server metadata missing")
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "never export") {
		t.Fatal("unexpected content in report")
	}
}

func TestTimingReportRejectsInvalidEvidenceWithoutInventingLatency(t *testing.T) {
	for name, extra := range map[string]string{
		"different origin":        timingFixture(1, "runEndReceived", 200, map[string]any{"timeOrigin": 100001}),
		"duplicate start":         timingFixture(1, "listenStart", 100, nil),
		"duplicate terminal":      timingFixture(1, "turnEnd", 2700, map[string]any{"outcome": "complete"}),
		"asr substituted for end": timingFixture(1, "speechEnd", 400, map[string]any{"source": "asr"}),
		"duplicate playback":      timingFixture(1, "playStart", 2500, map[string]any{"purpose": "final", "clockSource": "output_timestamp"}),
	} {
		t.Run(name, func(t *testing.T) {
			r, err := summarizeTimings(strings.NewReader(completedTimingFixture() + extra))
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Samples) != 1 || r.EndToPlay.Count != 0 || len(r.Samples[0].Issues) == 0 {
				t.Fatalf("invalid sample counted: %+v", r)
			}
		})
	}
	for _, input := range []string{
		`{"category":"voice.timing","stage":"probeOverflow"}`,
		timingFixture(1, "listenStart", 0, map[string]any{"atMs": nil}),
		completedTimingFixture() + timingFixture(2, "listenStart", 3000, map[string]any{"host": "desktop"}),
	} {
		if _, err := summarizeTimings(strings.NewReader(input)); err == nil {
			t.Fatal("expected invalid capture error")
		}
	}
}

func TestTimingReportDoesNotReplaceMissingSpeechEndWithManualStop(t *testing.T) {
	input := timingFixture(1, "listenStart", 100, nil) + timingFixture(1, "manualStop", 600, nil) +
		timingFixture(1, "playStart", 2500, map[string]any{"purpose": "notice", "clockSource": "context_clock"}) +
		timingFixture(1, "turnEnd", 2600, map[string]any{"outcome": "complete"})
	r, err := summarizeTimings(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if r.EndToPlay.Count != 0 || r.ManualToPlay.Count != 0 || r.Samples[0].EndToPlay != nil {
		t.Fatal("notice or manual stop substituted for final speech latency")
	}
}

func TestTimingReportSeparatesDetectionFromLocalSilence(t *testing.T) {
	point := func(stage string, at float64, extra map[string]any) string {
		if extra == nil {
			extra = map[string]any{}
		}
		extra["mode"] = "duplex"
		return timingFixture(1, stage, at, extra)
	}
	input := point("listenStart", 100, nil) + point("speechCandidate", 500, nil) + point("speechDetected", 680, nil) + point("interruptRequested", 681, nil) + point("localSilenced", 686, nil) + point("turnEnd", 687, map[string]any{"outcome": "superseded"})
	report, err := summarizeTimings(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if report.Detection.Count != 1 || *report.Detection.P95 != 180 || *report.Silence.P95 != 5 || report.EndToPlay.Count != 0 {
		t.Fatalf("mixed detection and playback timings: %+v", report)
	}
	duplicate, err := summarizeTimings(strings.NewReader(input + point("speechDetected", 690, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Detection.Count != 0 {
		t.Fatal("duplicate detection accepted")
	}
	if _, err = summarizeTimings(strings.NewReader(input + timingFixture(1, "runEndReceived", 690, nil))); err == nil {
		t.Fatal("mode switch mixed into homogeneous cohort")
	}
}
