package mailbox

import (
	"agent-platform/internal/chat"
	"agent-platform/internal/runtime/controlscope"
	"context"
	"fmt"
	"testing"
)

func TestMailboxBoxesAndIntersection(t *testing.T) {
	s, _ := testBox(t)
	statuses := []Status{Accepting, Queued, Inflight, Suspended, Delivered, Done, Failed, Discarded, Withdrawn}
	for i, status := range statuses {
		m := sample(fmt.Sprint(i), Letter, status)
		put(t, s.Store, m)
	}
	for box, want := range map[string]int{"pending": 4, "received": 2, "trash": 3} {
		filter := Filter{Box: box, Subject: "user"}
		rows, err := s.Store.List(filter)
		n, e := s.Store.Count(filter)
		if err != nil || e != nil || n != want || len(rows) != want {
			t.Fatalf("%s: rows=%d count=%d %v %v", box, len(rows), n, err, e)
		}
	}
	if n, _ := s.Store.Count(Filter{Box: "pending", Status: Done, Subject: "user"}); n != 0 {
		t.Fatal("box/status did not intersect")
	}
	if n, _ := s.Store.Count(Filter{AllSubjects: true}); n != 9 {
		t.Fatal("internal scans must include all boxes")
	}
}
func TestUserMailHasNoEnvelopeReplyOrDerivedControl(t *testing.T) {
	s, p := testBox(t)
	p.target = Target{}
	m := sample("user-mail", Letter, Queued)
	m.OriginType = "user"
	m.RequestID = "req-user"
	m.FromAgentKey = ""
	m.FromChatID = ""
	m.FromRunID = ""
	m.FromToolID = ""
	m.Hop = 0
	receipt, err := s.AcceptUser(context.Background(), m, controlscope.Scope{Subject: "user", Transport: "http"})
	if err != nil || receipt.Delivery != "started" {
		t.Fatal(receipt, err)
	}
	saved, _ := s.Store.Get(m.ID)
	if s.renderHeader(saved) != "" {
		t.Fatal("user envelope")
	}
	if ok, _ := s.Store.OwnsRun(saved.RunID, "", "user"); ok {
		t.Fatal("user mail grants Agent control")
	}
	if _, err := s.Store.StartedLetter(saved.RunID); err != ErrNotFound {
		t.Fatal("user Run treated as derived letter", err)
	}
	if err := s.Consumed(saved.Metadata(), saved.RunID); err != nil {
		t.Fatal(err)
	}
	// Completion should settle the user row without producing a reply.
	if err := s.Completed(chat.RunCompletion{RunID: saved.RunID, ChatID: saved.ToChatID, AgentKey: saved.ToAgentKey, FinishReason: "complete", AssistantText: "answer"}, 0); err != nil {
		t.Fatal(err)
	}
	saved, _ = s.Store.Get(m.ID)
	if saved.Status != Done || saved.AnsweredBy != "" {
		t.Fatalf("completion: %#v", saved)
	}
	if n, _ := s.Store.Count(Filter{AllSubjects: true}); n != 1 {
		t.Fatalf("unexpected reply: %d", n)
	}
}

func TestCancelQueuedLetterCannotWithdrawDeliveredRun(t *testing.T) {
	s, p := testBox(t)
	p.target = Target{Queue: true}
	m := sample("cancel-queued", Letter, Queued)
	m.OriginType = "user"
	m.RequestID = "request-cancel"
	if _, err := s.AcceptUser(t.Context(), m, controlscope.Scope{Subject: m.Subject}); err != nil {
		t.Fatal(err)
	}
	got, err := s.CancelUser(m.ID)
	if err != nil || got.Status != Withdrawn {
		t.Fatalf("queued cancellation: %+v %v", got, err)
	}
	delivered := sample("cancel-delivered", Letter, Delivered)
	delivered.OriginType = "user"
	delivered.Delivery = "started"
	delivered.RunID = "exact-run"
	put(t, s.Store, delivered)
	got, err = s.CancelUser(delivered.ID)
	if err != nil || got.Status == Withdrawn || got.RunID != "exact-run" {
		t.Fatalf("delivered letter mislabeled: %+v %v", got, err)
	}
}

func immediateSample(id string) Mail {
	m := sample(id, Letter, Queued)
	m.OriginType, m.RequestID, m.Hop = "user", "request-"+id, 0
	m.FromAgentKey, m.FromChatID, m.FromRunID, m.FromToolID = "", "", "", ""
	return m
}

func TestImmediateVerdictStoresNothingAndSkipsQueue(t *testing.T) {
	s, p := testBox(t)
	p.targetErr = failure("active_run_conflict", "active run found for chat")
	m := immediateSample("now-conflict")
	if _, err := s.AcceptUserNow(m, controlscope.Scope{Subject: m.Subject}); err == nil || err.Error() != "active run found for chat" {
		t.Fatalf("verdict: %v", err)
	}
	if _, err := s.Store.Get(m.ID); err != ErrNotFound {
		t.Fatalf("rejected immediate message was stored: %v", err)
	}
	// A queued letter ahead must not hold an immediate steer behind it.
	p.targetErr = nil
	p.target = Target{Queue: true}
	queued := immediateSample("ahead")
	if _, err := s.AcceptUser(t.Context(), queued, controlscope.Scope{Subject: queued.Subject}); err != nil {
		t.Fatal(err)
	}
	p.target = Target{RunID: "target-run"}
	p.callback = func(m Mail) (DeliveryResult, error) {
		if m.ID == queued.ID {
			return DeliveryResult{Accepted: false}, nil
		}
		return DeliveryResult{Accepted: true, RunID: m.RunID}, nil
	}
	steer := immediateSample("steer")
	steer.SteerID = "client-steer"
	got, err := s.AcceptUserNow(steer, controlscope.Scope{Subject: steer.Subject})
	if err != nil || got.Delivery != "inserted" || got.RunID != "target-run" || got.Status != Inflight {
		t.Fatalf("immediate steer: %+v %v", got, err)
	}
	saved, _ := s.Store.Get(steer.ID)
	if !saved.Immediate || saved.SteerID != "client-steer" || saved.Attempts != 1 {
		t.Fatalf("steer identity: %+v", saved)
	}
}

func TestImmediateSteerFailsWithRunVerdictAndIsNeverRequeued(t *testing.T) {
	s, p := testBox(t)
	p.callback = func(Mail) (DeliveryResult, error) {
		return DeliveryResult{Accepted: false, Status: "unmatched", Detail: "Run is no longer accepting steer"}, nil
	}
	declined := immediateSample("declined")
	got, err := s.AcceptUserNow(declined, controlscope.Scope{Subject: declined.Subject})
	if err != nil || got.Status != Failed || got.Error["code"] != "unmatched" {
		t.Fatalf("declined steer: %+v %v", got, err)
	}
	p.callback = nil
	unread := immediateSample("unread")
	if _, err := s.AcceptUserNow(unread, controlscope.Scope{Subject: unread.Subject}); err != nil {
		t.Fatal(err)
	}
	if err := s.Completed(chat.RunCompletion{RunID: "target-run", ChatID: unread.ToChatID, AgentKey: unread.ToAgentKey, FinishReason: "cancel"}, 0); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Store.Get(unread.ID)
	if saved.Status != Failed || saved.Error["code"] != "mail_run_ended" {
		t.Fatalf("unread steer was not settled: %+v", saved)
	}
	p.mu.Lock()
	deliveries := len(p.deliveries)
	p.mu.Unlock()
	if deliveries != 2 {
		t.Fatalf("immediate mail was redelivered: %d deliveries", deliveries)
	}
}
