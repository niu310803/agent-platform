package mailbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"agent-platform/internal/chat"
	"agent-platform/internal/contracts"
	"agent-platform/internal/runtime/controlscope"
)

func workLetter(t *testing.T, s *Service, id, run, from, to, parent string) Mail {
	t.Helper()
	m := sample(id, Letter, Delivered)
	m.FromChatID = from
	m.ToChatID = to
	m.RunID = run
	m.ParentID = parent
	m.Delivery = "started"
	put(t, s.Store, m)
	s.setScope(m.ID, controlscope.Scope{Subject: "user", Transport: "http"})
	return m
}
func completeWork(t *testing.T, s *Service, run, body string) {
	t.Helper()
	if err := s.Completed(chat.RunCompletion{RunID: run, AssistantText: body, FinishReason: "complete"}, 2); err != nil {
		t.Fatal(err)
	}
}
func consumeReply(t *testing.T, s *Service, letterID, run string) Mail {
	t.Helper()
	l, err := s.Store.Get(letterID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Store.Get(l.AnsweredBy)
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Store.Change(r.ID, func(m *Mail) error {
		m.Status = Delivered
		m.RunID = run
		m.Delivery = "started"
		m.ConsumedAt = now()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestWorkCrossRunParallelAndDuplicateCompletion(t *testing.T) {
	s, _ := testBox(t)
	original := workLetter(t, s, "upstream", "b1", "a", "b", "")
	workLetter(t, s, "child1", "w1", "b", "w1", original.ID)
	workLetter(t, s, "child2", "w2", "b", "w2", original.ID)
	completeWork(t, s, "b1", "dispatched")
	got, _ := s.Store.Get(original.ID)
	if got.AnsweredBy != "" {
		t.Fatal("dispatch acknowledged original prematurely")
	}
	completeWork(t, s, "w2", "second result")
	r2 := consumeReply(t, s, "child2", "b2")
	completeWork(t, s, "b2", "only second done")
	got, _ = s.Store.Get(original.ID)
	if got.AnsweredBy != "" {
		t.Fatal("partial result settled original")
	}
	completeWork(t, s, "w1", "first result")
	r1 := consumeReply(t, s, "child1", "b3")
	header := s.renderHeader(r1)
	if !strings.Contains(header, "second result") || !strings.Contains(header, original.ID) || !strings.Contains(header+s.Body(r1), "first result") {
		t.Fatalf("incomplete aggregation materials: %s", header)
	}
	if strings.Contains(header, "first result") {
		t.Fatalf("the delivered reply body is repeated in its own header: %s", header)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Completed(chat.RunCompletion{RunID: "b3", AssistantText: "final"}, 2); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ = s.Store.Get(original.ID)
	reply, err := s.Store.Get(got.AnsweredBy)
	if err != nil || reply.Message != "final" || reply.ReplyOfRun != "b3" || reply.ParentID != original.ID || reply.Hop != 2 {
		t.Fatalf("wrong final reply: %+v %v", reply, err)
	}
	replies, err := s.Store.query("KIND_='reply' AND PARENT_MAIL_ID_=?", original.ID)
	if err != nil || len(replies) != 1 {
		t.Fatalf("duplicate final reply: %d %v", len(replies), err)
	}
	if w, err := s.Store.WorkForMail(r2); err != nil || w.ID != original.ID {
		t.Fatalf("lost work: %+v %v", w, err)
	}
}
func TestWorkDifferentRootsQueueAndSendParent(t *testing.T) {
	s, _ := testBox(t)
	root := workLetter(t, s, "root", "root-run", "", "sender", "")
	_, err := s.Store.Change(root.ID, func(m *Mail) error { m.OriginType = "user"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := s.Send(context.Background(), Request{Start: contracts.RunStartRequest{AgentKey: "worker", Message: "job", Input: map[string]any{"message": "job"}, Origin: contracts.RunOrigin{RunID: "root-run", ChatID: "sender", AgentKey: "lead", Subject: "user", ToolID: "send"}}})
	if err != nil {
		t.Fatal(err)
	}
	sent, err := s.Store.Get(receipt.MailID)
	if err != nil || sent.ParentID != root.ID {
		t.Fatalf("missing user root: %+v %v", sent, err)
	}
	task := workLetter(t, s, "task", "active", "sender", "receiver", root.ID)
	same := task
	same.ID = "followup"
	same.RunID = ""
	if ok, e := s.CanInsert("active", same); e != nil || !ok {
		t.Fatalf("same work rejected: %v %v", ok, e)
	}
	same.ParentID = "different-root"
	if ok, e := s.CanInsert("active", same); e != nil || ok {
		t.Fatalf("different work inserted: %v %v", ok, e)
	}
	if ok, e := s.CanInsert("root-run", task); e != nil || ok {
		t.Fatalf("agent mail inserted into user Run: %v %v", ok, e)
	}
}
func TestDiscardReplacementBothArrivalOrders(t *testing.T) {
	for _, discardFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "discard-first", false: "discard-last"}[discardFirst], func(t *testing.T) {
			s, _ := testBox(t)
			original := workLetter(t, s, "original", "b1", "a", "b", "")
			workLetter(t, s, "one", "w1", "b", "w1", original.ID)
			workLetter(t, s, "two", "w2", "b", "w2", original.ID)
			completeWork(t, s, "w1", "lost result")
			one, _ := s.Store.Get("one")
			_, err := s.Store.Change(one.AnsweredBy, func(m *Mail) error { m.Status = Suspended; return nil })
			if err != nil {
				t.Fatal(err)
			}
			discard := func() {
				if _, e := s.Action(context.Background(), one.AnsweredBy, "discard", "user", controlscope.Scope{Subject: "user"}); e != nil {
					t.Fatal(e)
				}
				replacements, e := s.Store.query("REPLY_KEY_=?", "discard:"+one.AnsweredBy)
				if e != nil || len(replacements) != 1 {
					t.Fatalf("no replacement: %+v %v", replacements, e)
				}
				_, e = s.Store.Change(replacements[0].ID, func(m *Mail) error { m.Status = Delivered; m.Delivery = "started"; m.RunID = "discard-run"; return nil })
				if e != nil {
					t.Fatal(e)
				}
			}
			if discardFirst {
				discard()
				completeWork(t, s, "discard-run", "waiting for two")
			}
			completeWork(t, s, "w2", "good result")
			r := consumeReply(t, s, "two", "result-run")
			if !discardFirst {
				completeWork(t, s, "result-run", "waiting for one")
				discard()
			}
			last := "result-run"
			if !discardFirst {
				last = "discard-run"
			}
			completeWork(t, s, last, "final includes discarded failure")
			got, _ := s.Store.Get(original.ID)
			if got.AnsweredBy == "" {
				t.Fatal("original stuck after replacement")
			}
			if discardFirst && !strings.Contains(s.workHeader(r), "discarded") { // Work is terminal now; inspect the persisted replacement instead.
				rows, _ := s.Store.query("REPLY_KEY_=?", "discard:"+one.AnsweredBy)
				if len(rows) != 1 || rows[0].Outcome != "failed" {
					t.Fatal("discard reason lost")
				}
			}
		})
	}
}
func TestWorkFilesIncludeEarlierRunsAndChildArtifacts(t *testing.T) {
	s, p := testBox(t)
	original := workLetter(t, s, "original", "b1", "a", "b", "")
	child := workLetter(t, s, "child", "w1", "b", "w", original.ID)
	for _, dir := range []string{"a", "b", "w"} {
		if err := os.MkdirAll(filepath.Join(s.root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{"draft.md": "old", "draft2.md": "new", "child.md": "child"} {
		if err := os.WriteFile(filepath.Join(s.root, "b", name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	r := sample("child-reply", Reply, Delivered)
	r.FromToolID = ""
	r.ParentID = child.ID
	r.ToChatID = "b"
	r.RunID = "b2"
	r.Delivery = "started"
	r.Files = []File{{Name: "draft.md", Path: "child.md", SHA256: "child-hash", Size: 5}}
	put(t, s.Store, r)
	_, err := s.Store.Change(child.ID, func(m *Mail) error { m.Status = Done; m.AnsweredBy = r.ID; return nil })
	if err != nil {
		t.Fatal(err)
	}
	s.Bind(&artifactsPort{testPort: p, items: []chat.ArtifactManifestItem{
		{RunID: "b1", ArtifactItemState: chat.ArtifactItemState{URL: "@chat/draft.md"}},
		{RunID: "b2", ArtifactItemState: chat.ArtifactItemState{URL: "@chat/draft2.md"}},
	}})
	reply := s.newReply(original, "run:b2:a", "b2", "done", "completed")
	if err := s.replyFiles(&reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Files) != 3 || reply.Files[2].FromMailID != r.ID || reply.Files[2].Name != "draft (2).md" {
		t.Fatalf("missing earlier/child files: %+v", reply.Files)
	}
}

func TestConfirmedPlanExecutionKeepsUserRoot(t *testing.T) {
	s, _ := testBox(t)
	m := workLetter(t, s, "user-root", "plan", "", "sender", "")
	_, err := s.Store.Change(m.ID, func(m *Mail) error { m.OriginType = "user"; m.Status = Done; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InheritWork("execute", "plan"); err != nil {
		t.Fatal(err)
	}
	work, err := s.workForRun("execute")
	if err != nil || work.ID != m.ID {
		t.Fatalf("lost plan root: %+v %v", work, err)
	}
	completeWork(t, s, "execute", "done")
	s.mu.Lock()
	_, kept := s.inheritedWork["execute"]
	s.mu.Unlock()
	if kept {
		t.Fatal("finished execution Run kept its inherited plan work")
	}
}

// X1 handles U and defers on a child whose reply opens X2. A supplement from
// the same sender must not join X2: completion answers only X1's letter group,
// so it queues and its own Run answers it.
func TestSupplementForLaterWorkRunQueuesAndIsAnswered(t *testing.T) {
	s, _ := testBox(t)
	u := workLetter(t, s, "u", "x1", "p", "x", "p-root")
	workLetter(t, s, "c", "y1", "x", "y", u.ID)
	completeWork(t, s, "x1", "dispatched")
	completeWork(t, s, "y1", "child result")
	consumeReply(t, s, "c", "x2")
	supplement := sample("m", Letter, Queued)
	supplement.FromChatID, supplement.ToChatID, supplement.ParentID = "p", "x", "p-root"
	if ok, err := s.CanInsert("x2", supplement); err != nil || ok {
		t.Fatalf("supplement joined a later Run of the work: %v %v", ok, err)
	}
	same := sample("m1", Letter, Queued)
	same.FromChatID, same.ToChatID, same.ParentID = "p", "x", "p-root"
	if ok, err := s.CanInsert("x1", same); err != nil || !ok {
		t.Fatalf("supplement rejected by the Run that consumed the work: %v %v", ok, err)
	}
	completeWork(t, s, "x2", "final for u")
	workLetter(t, s, supplement.ID, "x3", "p", "x", "p-root")
	completeWork(t, s, "x3", "final for m")
	for id, want := range map[string]string{u.ID: "final for u", supplement.ID: "final for m"} {
		got, _ := s.Store.Get(id)
		reply, err := s.Store.Get(got.AnsweredBy)
		if err != nil || got.Status != Done || reply.Message != want {
			t.Fatalf("%s not answered by its own Run: %+v %+v %v", id, got, reply, err)
		}
	}
}

type matchingMailPort struct{ *testPort }

func (p *matchingMailPort) MailTarget(_ context.Context, m Mail) (Target, error) {
	if m.ID == "unrelated" {
		return Target{RunID: "active", Queue: true, AllowLaterMail: true}, nil
	}
	return Target{RunID: "active"}, nil
}
func TestUnrelatedQueuedMailDoesNotBlockMatchingReply(t *testing.T) {
	s, p := testBox(t)
	s.Bind(&matchingMailPort{p})
	for i, id := range []string{"unrelated", "matching"} {
		m := sample(id, Reply, Queued)
		m.CreatedAt = int64(i + 1)
		m.ReplyKey = "run:" + id + ":sender"
		m.FromToolID = ""
		put(t, s.Store, m)
		s.setScope(id, controlscope.Scope{Subject: "user"})
	}
	if err := s.dispatch("receiver"); err != nil {
		t.Fatal(err)
	}
	unrelated, _ := s.Store.Get("unrelated")
	matching, _ := s.Store.Get("matching")
	if unrelated.Status != Queued || matching.Status != Inflight {
		t.Fatalf("head-of-line block: unrelated=%s matching=%s", unrelated.Status, matching.Status)
	}
}
func TestMultiLayerWorkReturnsToEachOriginalSender(t *testing.T) {
	s, _ := testBox(t)
	workLetter(t, s, "a-to-b", "b1", "a", "b", "")
	workLetter(t, s, "d-to-b", "b-other", "d", "b", "")
	workLetter(t, s, "b-to-e", "e1", "b", "e", "a-to-b")
	workLetter(t, s, "e-to-worker", "w1", "e", "w", "b-to-e")
	workLetter(t, s, "other-worker", "other", "b", "w-other", "d-to-b")
	completeWork(t, s, "b1", "dispatch")
	completeWork(t, s, "e1", "dispatch")
	completeWork(t, s, "b-other", "dispatch")
	completeWork(t, s, "other", "D result")
	consumeReply(t, s, "other-worker", "b-other2")
	completeWork(t, s, "b-other2", "for D")
	d, _ := s.Store.Get("d-to-b")
	dr, err := s.Store.Get(d.AnsweredBy)
	if err != nil || dr.ToChatID != "d" {
		t.Fatalf("wrong D routing: %+v %v", dr, err)
	}
	completeWork(t, s, "w1", "deep result")
	consumeReply(t, s, "e-to-worker", "e2")
	completeWork(t, s, "e2", "from E")
	consumeReply(t, s, "b-to-e", "b2")
	completeWork(t, s, "b2", "for A")
	a, _ := s.Store.Get("a-to-b")
	ar, err := s.Store.Get(a.AnsweredBy)
	if err != nil || ar.ToChatID != "a" || ar.Message != "for A" {
		t.Fatalf("wrong A routing: %+v %v", ar, err)
	}
}

func TestDiscardUserInputDoesNotGenerateAgentReply(t *testing.T) {
	s, _ := testBox(t)
	m := sample("user-input", Letter, Suspended)
	m.OriginType = "user"
	m.FromChatID = ""
	m.FromAgentKey = ""
	put(t, s.Store, m)
	got, err := s.Action(context.Background(), m.ID, "discard", "user", controlscope.Scope{Subject: "user"})
	if err != nil || got.Status != Discarded {
		t.Fatalf("%+v %v", got, err)
	}
	replies, err := s.all(Filter{Kind: Reply, Subject: "user"})
	if err != nil || len(replies) != 0 {
		t.Fatalf("user input generated reply: %+v %v", replies, err)
	}
}

func TestWorkLocalArtifactRevisionUsesLastPublication(t *testing.T) {
	s, p := testBox(t)
	original := workLetter(t, s, "original", "first", "a", "b", "")
	s.Bind(&artifactsPort{testPort: p, items: []chat.ArtifactManifestItem{
		{RunID: "last", PublishedAt: 20, ArtifactItemState: chat.ArtifactItemState{URL: "@chat/artifacts/last/report.md", SHA256: "new", SizeBytes: 3}},
		{RunID: "first", PublishedAt: 10, ArtifactItemState: chat.ArtifactItemState{URL: "@chat/artifacts/first/report.md", SHA256: "old", SizeBytes: 3}},
	}})
	reply := s.newReply(original, "run:last:a", "last", "done", "completed")
	if err := s.replyFiles(&reply); err != nil {
		t.Fatal(err)
	}
	if len(reply.Files) != 1 || reply.Files[0].SHA256 != "new" {
		t.Fatalf("stale local revision: %+v", reply.Files)
	}
}
