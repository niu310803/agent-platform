package contracts

import (
	"agent-platform/internal/api"
	"context"
	"errors"
	"testing"
)

func TestSteerPreparationRechecksLifecycleAndOwnsInput(t *testing.T) {
	c := NewRunControl(context.Background(), "run-a")
	req := api.SteerRequest{RunID: "run-a", Message: "look", References: []api.Reference{{URL: "a.png"}}}
	if accepted, err := c.PrepareAndEnqueueSteer(req); accepted || err == nil {
		t.Fatal("unprepared image accepted")
	}
	started, release := make(chan struct{}), make(chan struct{})
	c.SetSteerPreparer(func(req api.SteerRequest) (api.SteerRequest, error) {
		close(started)
		<-release
		req.PreparedMessages = []map[string]any{{"role": "user", "content": "frozen"}}
		return req, nil
	})
	result := make(chan bool, 1)
	go func() { accepted, _ := c.PrepareAndEnqueueSteer(req); result <- accepted }()
	<-started
	c.CloseSteers()
	close(release)
	if <-result || len(c.DrainSteers()) != 0 {
		t.Fatal("closed run accepted prepared image")
	}
	c = NewRunControl(context.Background(), "run-b")
	req.PreparedMessages = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "original"}}}}
	if !c.EnqueueSteer(req) {
		t.Fatal("enqueue")
	}
	req.References[0].URL = "changed.png"
	req.PreparedMessages[0]["content"].([]map[string]any)[0]["text"] = "changed"
	got := c.DrainSteers()[0]
	if got.References[0].URL != "a.png" || got.PreparedMessages[0]["content"].([]map[string]any)[0]["text"] != "original" {
		t.Fatal("queue aliases caller input")
	}
}

func TestGuardedSteerRechecksAwaitingAfterPreparation(t *testing.T) {
	for _, mode := range []string{"question", "approval", "planning", "form"} {
		t.Run(mode, func(t *testing.T) {
			c := NewRunControl(context.Background(), "run")
			defer c.Finish()
			started, release := make(chan struct{}), make(chan struct{})
			c.SetSteerPreparer(func(req api.SteerRequest) (api.SteerRequest, error) {
				close(started)
				<-release
				req.PreparedMessages = []map[string]any{{"role": "user", "content": "frozen"}}
				return req, nil
			})
			result := make(chan error, 1)
			go func() {
				accepted, err := c.PrepareAndEnqueueSteer(api.SteerRequest{RunID: "run", RequireNoAwaiting: true, References: []api.Reference{{URL: "a.png"}}})
				if accepted {
					err = errors.New("accepted across awaiting registration")
				}
				result <- err
			}()
			<-started
			if mode == "planning" {
				c.ExpectPlanningSubmit(AwaitingSubmitContext{AwaitingID: "ask"})
			} else {
				c.ExpectSubmit(AwaitingSubmitContext{AwaitingID: "ask", Mode: mode})
			}
			close(release)
			if err := <-result; !errors.Is(err, ErrSteerAwaitingPending) {
				t.Fatal(err)
			}
			if _, ok := c.LookupAwaiting("ask"); !ok || len(c.DrainSteers()) != 0 {
				t.Fatal("guard changed waiting state")
			}
		})
	}
}
