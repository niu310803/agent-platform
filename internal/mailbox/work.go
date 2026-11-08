package mailbox

import (
	"errors"
	"fmt"
	"strings"

	"agent-platform/internal/contracts/queryinput"
)

// Each child needs a consumed reply. A discarded reply is replaced by a failed
// reply with the same parent; merely reaching a terminal delivery state is not
// evidence that the owning Chat has seen a result.
const pendingChildrenSQL = `SELECT COUNT(*) FROM MAILS c
 LEFT JOIN MAILS a ON a.MAIL_ID_=c.ANSWERED_BY_MAIL_ID_
 WHERE c.KIND_='letter' AND c.PARENT_MAIL_ID_=? AND (
 NOT EXISTS (SELECT 1 FROM MAILS r WHERE r.KIND_='reply'
  AND r.PARENT_MAIL_ID_=COALESCE(NULLIF(a.PARENT_MAIL_ID_,''),c.MAIL_ID_) AND r.STATUS_='delivered')
 OR EXISTS (SELECT 1 FROM MAILS r WHERE r.KIND_='reply'
  AND r.PARENT_MAIL_ID_=COALESCE(NULLIF(a.PARENT_MAIL_ID_,''),c.MAIL_ID_) AND r.STATUS_ IN ('queued','inflight','suspended','accepting')))`

func (s *Store) PendingChildren(id string) (int, error) {
	var n int
	err := s.db.QueryRow(pendingChildrenSQL, id).Scan(&n)
	return n, err
}
func (s *Store) Children(id string) ([]Mail, error) {
	return s.query("PARENT_MAIL_ID_=? AND KIND_='letter'", id)
}
func (s *Store) WorkGroup(m Mail) ([]Mail, error) {
	return s.query("KIND_='letter' AND RUN_ID_=? AND FROM_CHAT_ID_=? AND PARENT_MAIL_ID_=? AND ORIGIN_TYPE_='agent' AND STATUS_='delivered' AND ANSWERED_BY_MAIL_ID_=''", m.RunID, m.FromChatID, m.ParentID)
}
func (s *Store) WorkForMail(m Mail) (Mail, error) {
	if m.Kind == Letter {
		// Supplementary letters in an ordinary Run share its first letter.
		if !queryinput.UserMailOrigin(m.OriginType) && m.RunID != "" {
			rows, err := s.query("KIND_='letter' AND RUN_ID_=? AND FROM_CHAT_ID_=? AND PARENT_MAIL_ID_=? AND ORIGIN_TYPE_='agent'", m.RunID, m.FromChatID, m.ParentID)
			if err != nil {
				return Mail{}, err
			}
			if len(rows) > 0 {
				return rows[0], nil
			}
		}
		return m, nil
	}
	if m.ParentID == "" {
		return Mail{}, ErrNotFound
	}
	parent, err := s.Get(m.ParentID)
	if err != nil {
		return Mail{}, err
	}
	if parent.ParentID == "" {
		return Mail{}, ErrNotFound
	}
	return s.Get(parent.ParentID)
}
func (s *Store) WorkForRun(runID string) (Mail, error) {
	rows, err := s.ByRun(runID)
	if err != nil {
		return Mail{}, err
	}
	// The opening mail fixes the work; user steers never change that identity.
	for _, m := range rows {
		if m.Delivery == "started" {
			return s.WorkForMail(m)
		}
	}
	for _, m := range rows {
		if m.Status == Delivered {
			return s.WorkForMail(m)
		}
	}
	return Mail{}, ErrNotFound
}

// CanInsert is independent of mode. Runtime retains its existing primary Chat,
// planning and compaction gates before asking this question.
func (s *Service) CanInsert(runID string, m Mail) (bool, error) {
	if s == nil || s.Store == nil {
		return false, nil
	}
	work, err := s.workForRun(runID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if m.Kind == Letter {
		// A supplement joins only the Run that consumed the work's first letter.
		// Completion answers that Run's letter group, so a later Run of the same
		// work would leave the supplement unanswered; it queues for its own Run.
		return !queryinput.UserMailOrigin(work.OriginType) && work.RunID == runID && m.FromChatID == work.FromChatID && m.ParentID == work.ParentID, nil
	}
	incoming, err := s.Store.WorkForMail(m)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil && incoming.ID == work.ID, err
}

// Work headers are frozen into the ordinary mailHeader JSONL field. Model
// context compaction cannot remove the material needed for final aggregation.
func (s *Service) workHeader(m Mail) string {
	if m.Kind == Letter {
		return ""
	}
	work, err := s.Store.WorkForMail(m)
	if err != nil || work.Terminal() && !queryinput.UserMailOrigin(work.OriginType) {
		return ""
	}
	children, err := s.Store.Children(work.ID)
	if err != nil || len(children) == 0 {
		return ""
	}
	pending := 0
	type result struct {
		letter  Mail
		replies []Mail
	}
	results := []result{}
	for _, c := range children {
		parent := c.ID
		if c.AnsweredBy != "" {
			if a, e := s.Store.Get(c.AnsweredBy); e == nil && a.ParentID != "" {
				parent = a.ParentID
			}
		}
		replies, e := s.Store.query("KIND_='reply' AND PARENT_MAIL_ID_=?", parent)
		if e != nil {
			return ""
		}
		seen, waiting := false, false
		for _, r := range replies {
			if r.Status == Delivered || r.ID == m.ID {
				seen = true
			}
			if !r.Terminal() && r.ID != m.ID {
				waiting = true
			}
		}
		if !seen || waiting {
			pending++
		}
		results = append(results, result{c, replies})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Work %s from agent %q (chat %s): %d replies still pending.\n", work.ID, work.FromAgentKey, work.FromChatID, pending)
	if pending > 0 {
		for _, r := range results {
			fmt.Fprintf(&b, "- %s: %s\n", r.letter.ID, r.letter.Status)
		}
		return b.String()
	}
	b.WriteString("Summarize every result below, including failures and discarded results. Your final answer completes this work.\n")
	fmt.Fprintf(&b, "Original request %s: %s\n", work.ID, headerText(work))
	for _, f := range work.Files {
		fmt.Fprintf(&b, "Original attachment: @chat/%s\n", f.Path)
	}
	for _, r := range results {
		fmt.Fprintf(&b, "Child %s to agent %q (chat %s), request: %s\n", r.letter.ID, r.letter.ToAgentKey, r.letter.ToChatID, headerText(r.letter))
		for _, reply := range r.replies {
			if reply.Status != Delivered && reply.ID != m.ID {
				continue
			}
			if reply.ID == m.ID {
				// This reply's body follows the header; repeating it wastes context.
				fmt.Fprintf(&b, "Result %s (%s): see this mail's body.\n", reply.ID, reply.Outcome)
			} else {
				fmt.Fprintf(&b, "Result %s (%s): %s\n", reply.ID, reply.Outcome, headerText(reply))
			}
			for _, f := range reply.Files {
				fmt.Fprintf(&b, "Artifact: @chat/%s\n", f.Path)
			}
		}
	}
	return b.String()
}
func headerText(m Mail) string {
	const limit = 32000
	text := []rune(m.Message)
	if len(text) > limit {
		return string(text[:limit]) + " [truncated mail " + m.ID + "]"
	}
	if m.PurgedAt != 0 {
		return "[body purged for mail " + m.ID + "]"
	}
	return m.Message
}

// WorkRuns returns the delivery Runs that have handled this work, in order.
func (s *Store) WorkRuns(work Mail) ([]string, error) {
	rows, err := s.query(`RUN_ID_<>'' AND TO_CHAT_ID_=? AND
 (MAIL_ID_=? OR (KIND_='letter' AND RUN_ID_=? AND FROM_CHAT_ID_=? AND PARENT_MAIL_ID_=?)
 OR (KIND_ IN ('reply','notify','ask') AND PARENT_MAIL_ID_ IN
 (SELECT MAIL_ID_ FROM MAILS WHERE KIND_='letter' AND PARENT_MAIL_ID_=?)))`, work.ToChatID, work.ID, work.RunID, work.FromChatID, work.ParentID, work.ID)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, m := range rows {
		if !seen[m.RunID] {
			seen[m.RunID] = true
			ids = append(ids, m.RunID)
		}
	}
	return ids, nil
}

// InheritWork carries a confirmed plan's user root into its execution Run.
// Only trusted Runtime admission calls this; the public query cannot set it.
func (s *Service) InheritWork(runID, sourceRunID string) error {
	work, err := s.workForRun(sourceRunID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inheritedWork == nil {
		s.inheritedWork = map[string]string{}
	}
	s.inheritedWork[runID] = work.ID
	return nil
}
func (s *Service) forgetInheritedWork(runID string) {
	s.mu.Lock()
	delete(s.inheritedWork, runID)
	s.mu.Unlock()
}
func (s *Service) workForRun(runID string) (Mail, error) {
	s.mu.Lock()
	id := s.inheritedWork[runID]
	s.mu.Unlock()
	if id != "" {
		return s.Store.Get(id)
	}
	return s.Store.WorkForRun(runID)
}
