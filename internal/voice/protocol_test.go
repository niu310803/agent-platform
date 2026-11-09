package voice

import "testing"

func TestVoiceV2StrictControlFields(t *testing.T) {
	for _, raw := range []string{
		`{"type":"listen","state":"start","requestId":"r","turn":1,"intent":"conversation"}`,
		`{"type":"listen","state":"start","requestId":"r","turn":1,"intent":"question","runId":"run","awaitingId":"ask","questionId":"q1"}`,
		`{"type":"listen","state":"stop","requestId":"r","turn":1}`,
		`{"type":"abort","reason":"user_stop"}`, `{"type":"close"}`,
	} {
		if _, err := decodeMessage([]byte(raw)); err != nil {
			t.Fatalf("valid control: %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`null`, `[]`, `{"type":"close"} {"type":"close"}`,
		`{"type":"listen","state":"start","mode":"auto"}`,
		`{"type":"listen","state":"start","requestId":"r","turn":0,"intent":"conversation"}`,
		`{"type":"listen","state":"start","requestId":"r","turn":1.5,"intent":"conversation"}`,
		`{"type":"listen","state":"start","requestId":"r","turn":1,"intent":"question"}`,
		`{"type":"listen","state":"start","requestId":"r","turn":1,"intent":"conversation","runId":""}`,
		`{"type":"listen","state":"stop","requestId":"r","turn":1,"intent":"conversation"}`,
		`{"type":"listen","state":"start","requestId":"r","Turn":1,"intent":"conversation"}`,
		`{"type":"listen","state":"start","requestId":"r","turn":1,"intent":"conversation","version":2}`,
		`{"type":"close","runId":"foreign"}`, `{"type":"submit","answer":"yes"}`,
		`{"type":"hello","version":2,"audio":{"input":{"Codec":"pcm_s16le"}}}`,
	} {
		if _, err := decodeMessage([]byte(raw)); err == nil {
			t.Fatalf("invalid control accepted: %s", raw)
		}
	}
}
