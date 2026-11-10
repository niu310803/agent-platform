package voice

import (
	"time"

	"agent-platform/internal/observability"
)

// timing records only stage metadata on this process's monotonic session
// clock. It never supplies speechEnd/playStart, nor opens the playback gate.
func (s *session) timing(stage string, at time.Time, fields map[string]any) {
	if at.IsZero() || s.started.IsZero() {
		return // No invented timestamp for a fixture or missing measurement.
	}
	fields["sessionId"], fields["stage"] = s.id, stage
	fields["clock"], fields["atMs"] = "server_session_monotonic", float64(at.Sub(s.started).Microseconds())/1000
	observability.Log("voice.timing", fields)
}

func (t *taskTrace) fields(turn int) map[string]any {
	if t == nil {
		return map[string]any{"turn": turn}
	}
	fields := map[string]any{"turn": turn, "connectionReused": t.reused.Load(), "startupRetried": t.retried.Load()}
	if id := t.taskID.Load(); id != nil {
		fields["providerTaskId"] = *id
	}
	return fields
}
