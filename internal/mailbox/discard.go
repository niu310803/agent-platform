package mailbox

import (
	"agent-platform/internal/contracts/queryinput"
	"agent-platform/internal/runtime/controlscope"
	"context"
)

// Discard and replacement are committed together so a lost dispatch or a
// racing terminal callback can never leave a parent without a result.
func (s *Service) discard(ctx context.Context, m Mail, scope controlscope.Scope) (Mail, error) {
	var r Mail
	if queryinput.UserMailOrigin(m.OriginType) {
		// User input has no Agent sender and never receives a generated reply.
	} else if m.Kind == Letter {
		r = s.newReply(m, "discard:"+m.ID, "", "Result discarded by the user before delivery (mail "+m.ID+").", "failed")
	} else {
		r = m
		r.ID = NewID("mail")
		r.ReplyKey = "discard:" + m.ID
		r.Message = "Result discarded by the user before delivery (mail " + m.ID + ")."
		r.Outcome = "failed"
		r.Status = Queued
		r.CreatedAt = now()
		r.UpdatedAt = now()
		r.RunID = ""
		r.SteerID = ""
		r.Delivery = ""
		r.Attempts = 0
		r.ConsumedAt = 0
		r.TerminalAt = 0
		r.FromRunID = ""
		r.FromToolID = ""
		r.ReplyOfRun = ""
		r.Input = nil
		r.RequestDigest = ""
		r.Files = nil
		r.References = nil
		r.Error = nil
		r.ResumePhase = ""
		r.Evidence = ""
	}
	if m.Kind == Letter && m.FromChatClosedAt != 0 || m.Kind == Reply && m.ToChatClosedAt != 0 {
		r = Mail{}
	}
	if r.ID != "" {
		s.setScope(r.ID, scope)
	}
	changed, err := s.Store.discardWithReply(m.ID, r)
	if err != nil {
		s.dropScope(r.ID)
		return m, err
	}
	s.changed(changed)
	if r.ID != "" {
		s.changed(r)
		s.Kick(r.ToChatID)
	}
	return changed, nil
}
func (s *Store) discardWithReply(id string, r Mail) (Mail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Mail{}, err
	}
	defer tx.Rollback()
	m, err := scan(tx.QueryRow("SELECT "+columns+" FROM MAILS WHERE MAIL_ID_=?", id))
	if err != nil {
		return m, err
	}
	if m.Status != Suspended || m.PurgedAt != 0 {
		return m, failure("mail_state_conflict", "mail state changed")
	}
	m.Status = Discarded
	m.UpdatedAt = now()
	stampTerminal(&m)
	if r.ID != "" {
		if m.Kind == Letter && m.FromChatClosedAt != 0 || m.Kind == Reply && m.ToChatClosedAt != 0 {
			return m, failure("chat_deleted", "receiving Chat closed")
		}
		if err = insert(tx, r); err != nil {
			return m, err
		}
		if m.Kind == Letter {
			m.AnsweredBy = r.ID
		}
	}
	if err = update(tx, m); err != nil {
		return m, err
	}
	return m, tx.Commit()
}
