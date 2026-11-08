package mailbox

import (
	"agent-platform/internal/contracts/queryinput"
	"context"
	"errors"
	"fmt"

	"agent-platform/internal/chat"
	"agent-platform/internal/runtime/controlscope"
)

// AcceptUser is called only after Runtime's ordinary user admission has passed.
func (s *Service) AcceptUser(ctx context.Context, m Mail, scope controlscope.Scope) (Receipt, error) {
	s.setScope(m.ID, scope)
	stored, created, err := s.Store.Accept(m)
	if !created {
		s.dropScope(m.ID)
	}
	if err != nil {
		return Receipt{}, err
	}
	if created {
		s.changed(stored)
	}
	// Drain under the same per-Chat lock used by background delivery. The caller
	// receives the accepted delivery decision, without joining the Run's stream.
	s.drain(stored.ToChatID)
	current, err := s.Store.Get(stored.ID)
	if err != nil {
		return Receipt{}, err
	}
	return receipt(current), nil
}

// AcceptUserNow hands an interactive query or steer to a Run while it is
// accepted, under the per-Chat lock used by background delivery. It never
// queues: when the Chat cannot take it now, the caller gets that verdict and
// nothing is stored. A steer the Run then declines is kept as failed, with the
// Run's verdict in the receipt error.
func (s *Service) AcceptUserNow(m Mail, scope controlscope.Scope) (Receipt, error) {
	var out Receipt
	err := s.withDeliveryLock(m.ToChatID, func() error {
		// Earlier mail keeps its turn; a Run it opens makes this query conflict.
		if err := s.dispatch(m.ToChatID); err != nil {
			s.retry(m.ToChatID, err)
		}
		target, err := s.port.MailTarget(s.ctx, m)
		if err != nil {
			return err
		}
		if target.Queue {
			return failure("internal_error", "an immediate message cannot queue")
		}
		m.Immediate = true
		m.Status, m.Attempts, m.IgnoredOptions = Inflight, 1, target.IgnoredOptions
		m.RunID, m.Delivery = target.RunID, "inserted"
		if m.RunID == "" {
			m.RunID, m.Delivery = target.StartRunID, "started"
			if m.RunID == "" {
				m.RunID = chat.NewRunID()
			}
		}
		if m.SteerID == "" {
			m.SteerID = m.ID + ":1"
		}
		s.setScope(m.ID, scope)
		stored, created, err := s.Store.Accept(m)
		if !created {
			s.dropScope(m.ID)
			out = receipt(stored)
			return err
		}
		s.changed(stored)
		stored.Header = s.renderHeader(stored)
		result, deliverErr := s.port.DeliverMail(s.ctx, stored, scope)
		if deliverErr == nil && result.Accepted {
			current, err := s.Store.Get(stored.ID)
			out = receipt(current)
			return err
		}
		if result.Unknown {
			current, err := s.change(stored.ID, func(row *Mail) error {
				row.ResumePhase, row.Status, row.Evidence = Inflight, Suspended, "unknown"
				row.Error = map[string]any{"code": "delivery_unknown", "message": fmt.Sprint(deliverErr)}
				return nil
			})
			out = receipt(current)
			return errors.Join(deliverErr, err)
		}
		cause := deliverErr
		if result.Status != "" {
			cause = failure(result.Status, result.Detail)
		} else if cause == nil {
			cause = failure("mail_delivery_failed", "the Run did not take the message")
		}
		if err := s.fail(stored, cause); err != nil {
			return errors.Join(deliverErr, err)
		}
		current, err := s.Store.Get(stored.ID)
		out = receipt(current)
		return errors.Join(deliverErr, err)
	})
	return out, err
}
func (s *Service) drain(chatID string) {
	if err := s.withDeliveryLock(chatID, func() error { return s.dispatch(chatID) }); err != nil {
		s.retry(chatID, err)
	}
}
func (s *Service) withDeliveryLock(chatID string, fn func() error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return context.Canceled
	}
	lock := s.chatLocks[chatID]
	if lock == nil {
		lock = &chatLock{}
		s.chatLocks[chatID] = lock
	}
	lock.refs++
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	lock.mu.Lock()
	err := fn()
	lock.mu.Unlock()
	s.mu.Lock()
	lock.refs--
	if lock.refs == 0 {
		delete(s.chatLocks, chatID)
	}
	s.mu.Unlock()
	return err
}

// CancelUser serializes against dispatch. A delivered letter is never declared
// withdrawn; the caller then interrupts its exact started Run instead.
func (s *Service) CancelUser(id string) (Mail, error) {
	m, err := s.Store.Get(id)
	if err != nil {
		return m, err
	}
	err = s.withDeliveryLock(m.ToChatID, func() error {
		var e error
		m, e = s.change(id, func(row *Mail) error {
			if !queryinput.UserMailOrigin(row.OriginType) {
				return failure("mail_not_owned", "only user/automation messages may be cancelled here")
			}
			if row.Status == Queued || row.Status == Accepting || row.Status == Suspended && row.ConsumedAt == 0 && row.Delivery == "" {
				row.Status = Withdrawn
				row.TerminalAt = now()
			}
			return nil
		})
		return e
	})
	return m, err
}
