package mailbox

import (
	"agent-platform/internal/chat"
	"testing"
)

// Keep this regression independent of the new helpers so it also reproduces
// the premature acknowledgement against the pre-change implementation.
func TestDeferredReplyDoesNotAcknowledgeDispatchRun(t *testing.T) {
	s, _ := testBox(t)
	original := sample("original", Letter, Delivered)
	original.RunID = "dispatch"
	original.Delivery = "started"
	put(t, s.Store, original)
	child := sample("child", Letter, Delivered)
	child.FromChatID = original.ToChatID
	child.ToChatID = "worker-chat"
	child.ParentID = original.ID
	child.RunID = "worker"
	child.Delivery = "started"
	put(t, s.Store, child)
	if err := s.Completed(chat.RunCompletion{RunID: "dispatch", AssistantText: "assigned"}, 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.Store.Get(original.ID)
	if err != nil || got.AnsweredBy != "" || got.Status != Delivered {
		t.Fatalf("dispatch must retain reply responsibility while its child is pending: %+v %v", got, err)
	}
}
